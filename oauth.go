package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// OAuth gates the /admin surface via Google sign-in. Operator config
// is supplied through env vars: ADMIN_GOOGLE_CLIENT_ID,
// ADMIN_GOOGLE_CLIENT_SECRET, ADMIN_GOOGLE_REDIRECT_URL, and
// ADMIN_GOOGLE_ALLOWED_EMAILS (comma-separated allowlist).
//
// When any of the first three are missing, OAuth.Configured() returns
// false and the admin routes are not wired — opt-in feature, default
// off so the bare watchparty still runs on a host with no Google
// account at all.
type OAuth struct {
	cfg           *oauth2.Config
	allowedEmails map[string]bool
	auth          *Auth
	configured    bool
}

// stateCookie is the per-request CSRF token that ties an outbound
// Google redirect to its return callback. Short-lived, narrow path so
// it does not leak into other browser navigations.
const stateCookie = "wp_oauth_state"

// userinfoEndpoint is Google's OpenID Connect userinfo URL. Returns
// the verified email + name for the bearer token issued by the code
// exchange.
const userinfoEndpoint = "https://openidconnect.googleapis.com/v1/userinfo"

// NewOAuth builds the OAuth gate. Returns a configured client if all
// required env values are present; otherwise returns an OAuth value
// whose Configured() reports false and whose handlers explain that
// admin sign-in is disabled.
func NewOAuth(clientID, clientSecret, redirectURL, allowedEmailsCSV string, auth *Auth) *OAuth {
	o := &OAuth{auth: auth}
	if clientID == "" || clientSecret == "" || redirectURL == "" {
		return o
	}
	allow := make(map[string]bool)
	for _, e := range strings.Split(allowedEmailsCSV, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			allow[e] = true
		}
	}
	if len(allow) == 0 {
		// Without an allowlist any Google account on Earth could sign
		// in. Refuse to come up — operator must specify at least one.
		log.Printf("oauth: ADMIN_GOOGLE_ALLOWED_EMAILS is required when client ID/secret are set; admin sign-in disabled")
		return o
	}
	o.cfg = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{"openid", "email"},
		Endpoint:     google.Endpoint,
	}
	o.allowedEmails = allow
	o.configured = true
	log.Printf("oauth: admin sign-in enabled · redirect=%s · allowlist=%d email(s)",
		redirectURL, len(allow))
	return o
}

// Configured reports whether the OAuth gate has all required env
// values. When false, the /admin/* routes should not be registered.
func (o *OAuth) Configured() bool { return o.configured }

// HandleLogin renders the admin landing page with a "Sign in with
// Google" button. The button POSTs to /admin/oauth/start to mint a
// fresh state cookie before bouncing to Google. (A GET to /admin/login
// is safe to bookmark; the redirect to Google only happens on the
// subsequent click.)
func (o *OAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// If already signed in, jump straight to /admin.
	if o.auth.AdminEmail(r) != "" {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(adminLoginHTML)
}

// HandleStart mints a CSRF state cookie and 303s to Google. Must be
// reached via POST from /admin/login so the state cookie is bound to
// an explicit user gesture, not a drive-by GET.
func (o *OAuth) HandleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
		Secure:   requestIsHTTPS(r),
	})
	http.Redirect(w, r, o.cfg.AuthCodeURL(state, oauth2.AccessTypeOnline), http.StatusSeeOther)
}

// HandleCallback is the Google redirect target. Validates the state
// cookie, exchanges the code for tokens, fetches the userinfo email,
// enforces the allowlist, and mints the admin session cookie.
func (o *OAuth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	stateInQuery := r.URL.Query().Get("state")
	stateInCookie, err := r.Cookie(stateCookie)
	if err != nil || stateInCookie.Value == "" || stateInQuery == "" ||
		stateInCookie.Value != stateInQuery {
		log.Printf("oauth: state mismatch ip=%s", clientIP(r))
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	// One-shot — burn the state cookie regardless of what comes next.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", Path: "/admin", MaxAge: -1,
	})

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		log.Printf("oauth: google returned error %q ip=%s", errParam, clientIP(r))
		http.Error(w, "google: "+errParam, http.StatusForbidden)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	token, err := o.cfg.Exchange(ctx, code)
	if err != nil {
		log.Printf("oauth: code exchange failed: %v", err)
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}

	// Fetch the verified email from Google's userinfo endpoint. The
	// access token granted by the openid+email scopes is enough.
	client := o.cfg.Client(ctx, token)
	resp, err := client.Get(userinfoEndpoint)
	if err != nil {
		log.Printf("oauth: userinfo fetch failed: %v", err)
		http.Error(w, "userinfo fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("oauth: userinfo status %d body=%q", resp.StatusCode, strings.TrimSpace(string(body)))
		http.Error(w, "userinfo error", http.StatusBadGateway)
		return
	}
	var ui struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ui); err != nil {
		log.Printf("oauth: userinfo decode: %v", err)
		http.Error(w, "userinfo decode", http.StatusBadGateway)
		return
	}
	email := strings.ToLower(strings.TrimSpace(ui.Email))
	if !ui.EmailVerified || email == "" {
		log.Printf("oauth: refused unverified email %q ip=%s", email, clientIP(r))
		http.Error(w, "email not verified by google", http.StatusForbidden)
		return
	}
	if !o.allowedEmails[email] {
		log.Printf("oauth: REJECT non-allowlisted email %q ip=%s", email, clientIP(r))
		http.Error(w, "not authorized", http.StatusForbidden)
		return
	}

	log.Printf("oauth: ADMIN sign-in email=%q ip=%s", email, clientIP(r))
	o.auth.SetAdminCookie(w, email)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// HandleLogout clears the admin cookie and lands on the sign-in page.
func (o *OAuth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if email := o.auth.AdminEmail(r); email != "" {
		log.Printf("oauth: admin sign-out email=%q ip=%s", email, clientIP(r))
	}
	o.auth.ClearAdminCookie(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// randomHex returns 2n hex chars of cryptographic randomness, used for
// CSRF state tokens.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand.Read failure is essentially "the OS RNG is broken" —
		// time.Now isn't a real fallback but we don't want to panic.
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))
	}
	return hex.EncodeToString(b)
}

// requestIsHTTPS is conservative — used only to mark cookies Secure.
// Honors r.TLS for direct TLS and the de-facto X-Forwarded-Proto
// header for reverse-proxy deployments (which is the project's
// recommended setup per the README).
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
