package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"
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

// token mints the cookie value "<email>:<hmac(email)>". Email visible
// so logs / UI can show it; the HMAC is what authenticates.
func (a *Auth) token(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("identity:"))
	mac.Write([]byte(email))
	return email + ":" + hex.EncodeToString(mac.Sum(nil))
}

// Email returns the request's verified email, or "" if the cookie is
// missing/invalid. Constant-time compare so timing doesn't leak which
// characters matched.
func (a *Auth) Email(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	email, _, ok := strings.Cut(c.Value, ":")
	if !ok || email == "" {
		return ""
	}
	if subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.token(email))) != 1 {
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
