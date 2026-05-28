package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const sessionCookie = "wp_session"

// adminCookie carries the email of a Google-authenticated admin
// alongside an HMAC over the email. Independent of sessionCookie so
// the operator can be signed in as host AND admin simultaneously
// (watching a movie while running maintenance from the same browser).
const adminCookie = "wp_admin"

// adminCookieTTL bounds how long an admin sign-in stays valid before
// the operator must re-authenticate through Google. Maintenance is
// not an everyday activity — short-ish lifetime is fine and limits
// blast radius if a session cookie ever leaks.
const adminCookieTTL = 12 * time.Hour

// Role is who the viewer is in the watch party.
type Role int

const (
	RoleAnon   Role = iota // not authenticated
	RoleViewer             // authenticated, watch-only
	RoleHost               // authenticated, can pick / play / pause / seek
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

// Auth gates access via shared passwords. There are two:
//
//   - WATCH_PASSWORD: required, distributed to all friends. Grants
//     viewer access (watch a movie the host already started, but no
//     control over what plays).
//
//   - HOST_PASSWORD (optional): grants the privileged role — can pick
//     the movie, play, pause, seek. If unset, anyone with the watch
//     password is treated as a host (preserves the original
//     "any-friend-can-drive" behaviour).
//
// The signing secret is derived from both passwords so sessions
// survive process restarts without extra config, but invalidate when
// you change a password.
type Auth struct {
	watch, host string
	secret      []byte
	// Precomputed cookie values for each role. Computed once in
	// NewAuth so Role() can skip the per-request HMAC — every
	// protected request (segments, SSE, control) reaches Guard, and
	// at 4 viewers × 1 segment / 4s × 90 min that's thousands of
	// HMACs per movie. Now it's a single constant-time string compare.
	hostToken   string
	viewerToken string
}

func NewAuth(watch, host string) *Auth {
	mac := hmac.New(sha256.New, []byte("plexwatchparty-v2"))
	mac.Write([]byte(watch))
	mac.Write([]byte{0})
	mac.Write([]byte(host))
	a := &Auth{watch: watch, host: host, secret: mac.Sum(nil)}
	a.hostToken = a.token(RoleHost)
	a.viewerToken = a.token(RoleViewer)
	return a
}

// HostEnabled reports whether HOST_PASSWORD was configured (and the
// two passwords are distinct). When false, every authenticated user
// is implicitly a host.
func (a *Auth) HostEnabled() bool {
	return a.host != "" && a.host != a.watch
}

// token mints the cookie value for the given role. The cookie shape
// is "<role>:<hmac>" — role visible in plain text so the server can
// route logic before verifying, but the hmac is what authenticates.
func (a *Auth) token(role Role) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(role.String()))
	return role.String() + ":" + hex.EncodeToString(mac.Sum(nil))
}

// Role returns the request's authenticated role, or RoleAnon if the
// cookie is missing or invalid. Compares against precomputed tokens
// so the hot path is a single constant-time compare per request.
func (a *Auth) Role(r *http.Request) Role {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return RoleAnon
	}
	roleName, _, ok := strings.Cut(c.Value, ":")
	if !ok {
		return RoleAnon
	}
	var want string
	var role Role
	switch roleName {
	case "host":
		want = a.hostToken
		role = RoleHost
	case "viewer":
		want = a.viewerToken
		role = RoleViewer
	default:
		return RoleAnon
	}
	if subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) != 1 {
		return RoleAnon
	}
	return role
}

// EffectiveRole is like Role, but when HOST_PASSWORD isn't configured
// it upgrades any authenticated viewer to a host (preserving the
// original behaviour where any logged-in friend can drive).
func (a *Auth) EffectiveRole(r *http.Request) Role {
	role := a.Role(r)
	if role == RoleViewer && !a.HostEnabled() {
		return RoleHost
	}
	return role
}

// Guard wraps a handler. Allows any authenticated role through;
// redirects unauthenticated browsers to /login and returns 401 for
// SSE / control / api calls.
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

// RequireHost wraps a handler that needs host role. Used for
// /control (load / play / pause / seek). Returns 403 for viewers.
func (a *Auth) RequireHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.EffectiveRole(r) == RoleHost {
			next.ServeHTTP(w, r)
			return
		}
		log.Printf("auth: 403 host-only %s %s ip=%s role=%s",
			r.Method, r.URL.Path, clientIP(r), a.EffectiveRole(r))
		http.Error(w, "host only", http.StatusForbidden)
	})
}

func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(loginHTML)
		return
	}
	// Cap the form body — /login takes two short strings (name +
	// password), no need to read megabytes.
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	pw := []byte(r.FormValue("password"))
	ip := clientIP(r)
	var role Role
	if a.host != "" && subtle.ConstantTimeCompare(pw, []byte(a.host)) == 1 {
		role = RoleHost
	} else if a.watch != "" && subtle.ConstantTimeCompare(pw, []byte(a.watch)) == 1 {
		role = RoleViewer
	} else {
		log.Printf("login: FAIL ip=%s ua=%q", ip, r.UserAgent())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(loginHTML)
		return
	}
	// Display name is optional. sanitizeName trims, drops anything
	// outside printable ASCII, and caps at maxViewerName.
	name := sanitizeName(r.FormValue("name"))
	log.Printf("login: OK ip=%s role=%s name=%q ua=%q", ip, role, name, r.UserAgent())
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.token(role),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
	})
	if name != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     nameCookie,
			Value:    url.QueryEscape(name),
			Path:     "/",
			HttpOnly: false, // viewable by JS so the player can show "you" in the roster
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().Add(365 * 24 * time.Hour),
		})
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// HandleLogout clears both the auth session cookie and the display
// name cookie, then redirects back to /login. Safe to call when not
// logged in — Set-Cookie with MaxAge: -1 is a no-op against missing
// cookies and the redirect still does the right thing.
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{sessionCookie, nameCookie} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			SameSite: http.SameSiteLaxMode,
		})
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// adminTokenFor mints the admin cookie value for the given email.
// Format: "<email>:<hmac(email)>" — email visible so logs/UI can show
// "Signed in as foo@bar.com" without re-decoding anything, but the
// HMAC is what makes the cookie unforgeable.
func (a *Auth) adminTokenFor(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("admin:"))
	mac.Write([]byte(email))
	return email + ":" + hex.EncodeToString(mac.Sum(nil))
}

// AdminEmail returns the verified admin email for the request, or ""
// if no valid admin cookie is present. Constant-time HMAC compare so
// timing leaks don't reveal which characters of the email match.
func (a *Auth) AdminEmail(r *http.Request) string {
	c, err := r.Cookie(adminCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	email, _, ok := strings.Cut(c.Value, ":")
	if !ok || email == "" {
		return ""
	}
	want := a.adminTokenFor(email)
	if subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) != 1 {
		return ""
	}
	return email
}

// SetAdminCookie writes the signed admin session cookie for the given
// email. Called by the OAuth callback after Google has confirmed the
// email and the allowlist check has passed.
func (a *Auth) SetAdminCookie(w http.ResponseWriter, email string) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    a.adminTokenFor(email),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(adminCookieTTL),
	})
}

// ClearAdminCookie expires the admin cookie. Safe to call when no
// cookie is present.
func (a *Auth) ClearAdminCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		SameSite: http.SameSiteLaxMode,
	})
}

// RequireAdmin wraps a handler so only requests with a valid admin
// cookie pass through. Browser navigations get a 303 to /admin/login
// (the Google sign-in landing page); API calls get a 401 so the
// admin panel's JS can detect expiry and re-auth.
func (a *Auth) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.AdminEmail(r) != "" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin/api/") {
			http.Error(w, "admin only", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	})
}
