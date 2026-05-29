package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// stateCookie is the per-request CSRF token that ties an outbound
// Google redirect to its return callback. Short-lived, narrow path so
// it does not leak into other browser navigations.
const stateCookie = "wp_oauth_state"

// userinfoEndpoint is Google's OpenID Connect userinfo URL. Returns
// the verified email + name for the bearer token issued by the code
// exchange.
const userinfoEndpoint = "https://openidconnect.googleapis.com/v1/userinfo"

type OAuth struct {
	cfg        *oauth2.Config
	auth       *Auth
	configured bool
}

// NewOAuth builds the Google sign-in gate for the whole app. Returns a
// value whose Configured() is false if any required field is missing
// (main.go fails fast in that case).
func NewOAuth(clientID, clientSecret, redirectURL string, auth *Auth) *OAuth {
	o := &OAuth{auth: auth}
	if clientID == "" || clientSecret == "" || redirectURL == "" {
		return o
	}
	o.cfg = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint:     google.Endpoint,
	}
	o.configured = true
	log.Printf("oauth: google sign-in enabled · redirect=%s", redirectURL)
	return o
}

func (o *OAuth) Configured() bool { return o.configured }

// HandleLogin renders the sign-in page. If already signed in, jumps to /.
func (o *OAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if o.auth.Email(r) != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(loginHTML)
}

// HandleStart mints a CSRF state cookie and 303s to Google. POST-only so
// the state binds to an explicit gesture.
func (o *OAuth) HandleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600,
		Secure: requestIsHTTPS(r),
	})
	http.Redirect(w, r, o.cfg.AuthCodeURL(state, oauth2.AccessTypeOnline), http.StatusSeeOther)
}

// HandleCallback validates state, exchanges the code, fetches the
// verified email + name, enforces ALLOWED_EMAILS, and sets the identity
// + display-name cookies.
func (o *OAuth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	stateInQuery := r.URL.Query().Get("state")
	stateInCookie, err := r.Cookie(stateCookie)
	if err != nil || stateInCookie.Value == "" || stateInQuery == "" ||
		stateInCookie.Value != stateInQuery {
		log.Printf("oauth: state mismatch ip=%s", clientIP(r))
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/", MaxAge: -1})

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		log.Printf("oauth: google returned error %q ip=%s", errParam, clientIP(r))
		http.Redirect(w, r, "/login?error=google", http.StatusSeeOther)
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
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
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
	if !o.auth.Allowed(email) {
		log.Printf("oauth: REJECT non-allowlisted email %q ip=%s", email, clientIP(r))
		http.Redirect(w, r, "/login?error=denied", http.StatusSeeOther)
		return
	}

	log.Printf("oauth: sign-in email=%q ip=%s", email, clientIP(r))
	o.auth.SetSession(w, email, requestIsHTTPS(r))
	if name := sanitizeName(ui.Name); name != "" {
		http.SetCookie(w, &http.Cookie{
			Name: nameCookie, Value: url.QueryEscape(name), Path: "/",
			HttpOnly: false, SameSite: http.SameSiteLaxMode,
			Expires: time.Now().Add(365 * 24 * time.Hour),
		})
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
