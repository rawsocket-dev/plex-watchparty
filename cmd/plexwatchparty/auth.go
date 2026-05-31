package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const sessionCookie = "wp_session"
const sessionTTL = 30 * 24 * time.Hour

// Role is who the signed-in user is in the watch party.
type Role int

const (
	RoleAnon   Role = iota // no valid session, or email not allowlisted
	RoleViewer             // allowed: watch only
	RoleHost               // allowed + host: can pick / play / pause / seek
)

func (r Role) String() string {
	switch r {
	case RoleHost:
		return "host"
	case RoleViewer:
		return "viewer"
	default:
		return "anon"
	}
}

// Auth gates the app by Google-verified email. Three allowlists,
// each an email set loaded from the environment:
//
//   - allowed: may sign in and watch.
//   - hosts:   subset that may drive playback. EMPTY means every
//     allowed user is a host (preserves the "any friend drives" default).
//   - admins:  subset that may open the maintenance panel.
//
// The session cookie carries the verified email + an HMAC over it.
// Roles are resolved live from the lists on every request, so editing
// the environment (and restarting) re-grades access with no re-login,
// and removing an email revokes it on the next request.
type Auth struct {
	secret  []byte
	allowed map[string]bool
	hosts   map[string]bool
	admins  map[string]bool
}

// NewAuth derives the cookie-signing secret from secretSeed (the Google
// client secret in production — stable + already secret; rotating it
// invalidates all sessions) and parses the three comma-separated lists.
func NewAuth(secretSeed, allowedCSV, hostsCSV, adminsCSV string) *Auth {
	mac := hmac.New(sha256.New, []byte("plexwatchparty-identity-v1"))
	mac.Write([]byte(secretSeed))
	return &Auth{
		secret:  mac.Sum(nil),
		allowed: parseEmailSet(allowedCSV),
		hosts:   parseEmailSet(hostsCSV),
		admins:  parseEmailSet(adminsCSV),
	}
}

func parseEmailSet(csv string) map[string]bool {
	m := make(map[string]bool)
	for _, e := range strings.Split(csv, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			m[e] = true
		}
	}
	return m
}

// Allowed reports whether the (already lowercased) email may sign in.
func (a *Auth) Allowed(email string) bool { return a.allowed[email] }

// tokenWithExpiry mints the cookie value "<email>:<exp>:<hmac(email:exp)>".
// exp is unix seconds and is covered by the signature, so a client can't
// extend its own session by editing the cookie. Email is visible (logs /
// UI); the HMAC authenticates.
func (a *Auth) tokenWithExpiry(email string, exp int64) string {
	email = strings.ToLower(strings.TrimSpace(email))
	payload := email + ":" + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("identity:"))
	mac.Write([]byte(payload))
	return payload + ":" + hex.EncodeToString(mac.Sum(nil))
}

// token mints a session token that expires sessionTTL from now.
func (a *Auth) token(email string) string {
	return a.tokenWithExpiry(email, time.Now().Add(sessionTTL).Unix())
}

// Email returns the request's verified email, or "" if the cookie is
// missing, invalid, or expired. The expiry is enforced server-side here
// (not just via the cookie's own Expires): a captured cookie stops
// working once its baked-in exp passes. Constant-time compare so timing
// doesn't leak which characters matched.
func (a *Auth) Email(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	// Layout: email:exp:hmac — a valid email contains no colon.
	parts := strings.SplitN(c.Value, ":", 3)
	if len(parts) != 3 {
		return ""
	}
	email := parts[0]
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || email == "" {
		return ""
	}
	// A tampered email or exp changes the expected HMAC, so recomputing
	// the whole token for (email, exp) and comparing covers both.
	if subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.tokenWithExpiry(email, exp))) != 1 {
		return ""
	}
	if time.Now().Unix() > exp {
		return ""
	}
	return email
}

// Role resolves the request's effective role live from the allowlists.
func (a *Auth) Role(r *http.Request) Role {
	return a.roleForEmail(a.Email(r))
}

// roleForEmail resolves the tier for an already-verified email.
func (a *Auth) roleForEmail(email string) Role {
	if email == "" || !a.allowed[email] {
		return RoleAnon
	}
	if len(a.hosts) == 0 || a.hosts[email] {
		return RoleHost
	}
	return RoleViewer
}

// isAdminEmail reports whether an allowlisted email is also an admin.
func (a *Auth) isAdminEmail(email string) bool {
	return a.allowed[email] && a.admins[email]
}

// IsAdmin reports whether the request's verified email is allowlisted
// AND on the admin list.
func (a *Auth) IsAdmin(r *http.Request) bool {
	email := a.Email(r)
	return email != "" && a.allowed[email] && a.admins[email]
}

// SetSession writes the signed identity cookie for email.
func (a *Auth) SetSession(w http.ResponseWriter, email string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.token(email),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
		Secure:   secure,
	})
}

// HandleLogout clears the identity + display-name cookies and lands on
// the sign-in page. Safe to call when not signed in.
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{sessionCookie, nameCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1, SameSite: http.SameSiteLaxMode,
		})
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Guard allows any allowlisted session through; redirects unauthenticated
// browsers to /login and returns 401 for SSE / control / api calls.
func (a *Auth) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Role(r) != RoleAnon {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Accept") == "text/event-stream" ||
			r.URL.Path == "/control" ||
			strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// RequireHost wraps handlers that need host role (/control).
func (a *Auth) RequireHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Role(r) == RoleHost {
			next.ServeHTTP(w, r)
			return
		}
		log.Printf("auth: 403 host-only %s %s ip=%s role=%s",
			r.Method, r.URL.Path, clientIP(r), a.Role(r))
		http.Error(w, "host only", http.StatusForbidden)
	})
}

// RequireAdmin wraps the /admin tree. Browser navigations get a 303 to
// /login; /admin/api/* gets a 401 so the panel JS can detect expiry.
func (a *Auth) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.IsAdmin(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin/api/") {
			http.Error(w, "admin only", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// csrfGuard rejects state-changing (non-safe-method) requests whose Origin
// — or Referer, as a fallback — names a different host than the request.
// It's defense-in-depth on top of the SameSite=Lax session cookie (which
// already withholds the cookie on cross-site browser POSTs). Requests with
// no Origin/Referer (non-browser clients, which carry no ambient cookies)
// pass through. Safe methods (GET/HEAD/OPTIONS) are never blocked, so SSE
// and page loads are unaffected.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = r.Header.Get("Referer")
		}
		if origin != "" && !sameHost(origin, expectedHost(r)) {
			log.Printf("csrf: blocked %s %s origin=%q host=%q ip=%s",
				r.Method, r.URL.Path, origin, expectedHost(r), clientIP(r))
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// expectedHost is the public host the browser sees: X-Forwarded-Host (set
// by a reverse proxy) when present, else the request Host. An attacker
// doing browser-CSRF can't set X-Forwarded-Host (browsers don't send it;
// the proxy adds it), so trusting it here is safe.
func expectedHost(r *http.Request) string {
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		if i := strings.IndexByte(xfh, ','); i >= 0 {
			xfh = xfh[:i] // first hop = original client-facing host
		}
		return strings.TrimSpace(xfh)
	}
	return r.Host
}

// sameHost reports whether rawURL's host matches host. Only the host is
// compared — behind a TLS-terminating proxy the scheme can differ while the
// host (what matters for CSRF) is preserved.
func sameHost(rawURL, host string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// actorCtxKey keys the verified email of the user driving a request,
// stashed by WithActor so downstream handlers (the Hub's /control,
// which is intentionally decoupled from Auth) can attribute actions
// without re-parsing the cookie.
type actorCtxKey struct{}

// WithActor stashes the request's verified email in the context. Wrap
// handlers that need to know "who did this" but don't otherwise hold
// an *Auth (e.g. hub.HandleControl).
func (a *Auth) WithActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), actorCtxKey{}, a.Email(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// actorEmail returns the email stashed by WithActor, or "" if absent.
func actorEmail(r *http.Request) string {
	if v, ok := r.Context().Value(actorCtxKey{}).(string); ok {
		return v
	}
	return ""
}
