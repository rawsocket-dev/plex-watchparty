package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"BG", "BG"},
		{"  Alice  ", "Alice"},
		{"", ""},
		{"\x00BG\x07", "BG"},               // strip controls
		{"Émile", "mile"},                  // non-ASCII letters dropped
		{"thisnameiswaytoolongforouroption", "thisnameiswaytoo"}, // capped to 16 runes
	}
	for _, c := range cases {
		got := sanitizeName(c.in)
		// truncation length check is what we actually care about
		if len(got) > maxViewerName {
			t.Errorf("sanitizeName(%q) = %q, longer than cap %d", c.in, got, maxViewerName)
		}
		if c.want != "" && got != c.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
		if c.want == "" && got != "" {
			t.Errorf("sanitizeName(%q) = %q, want empty", c.in, got)
		}
	}
}

func testAuth() *Auth {
	// allowed: alice, bob, op; hosts: op only; admins: op only.
	return NewAuth("secret-xyz", "alice@x.com,bob@x.com,op@x.com", "op@x.com", "op@x.com")
}

func reqWithSession(a *Auth, email string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.token(email)})
	return r
}

func TestAuthRoleResolution(t *testing.T) {
	a := testAuth()
	cases := []struct {
		email string
		want  Role
	}{
		{"op@x.com", RoleHost},
		{"alice@x.com", RoleViewer},
		{"OP@X.COM", RoleHost},
		{"nobody@x.com", RoleAnon},
	}
	for _, c := range cases {
		if got := a.Role(reqWithSession(a, c.email)); got != c.want {
			t.Errorf("Role(%q) = %v, want %v", c.email, got, c.want)
		}
	}
}

func TestAuthEmptyHostListMakesEveryoneHost(t *testing.T) {
	a := NewAuth("s", "alice@x.com,bob@x.com", "", "")
	if got := a.Role(reqWithSession(a, "alice@x.com")); got != RoleHost {
		t.Errorf("empty HOST_EMAILS: Role = %v, want RoleHost", got)
	}
}

func TestAuthIsAdmin(t *testing.T) {
	a := testAuth()
	if !a.IsAdmin(reqWithSession(a, "op@x.com")) {
		t.Error("op should be admin")
	}
	if a.IsAdmin(reqWithSession(a, "alice@x.com")) {
		t.Error("alice should not be admin")
	}
}

func TestAuthRejectsTamperedCookie(t *testing.T) {
	a := testAuth()
	r := httptest.NewRequest("GET", "/", nil)
	parts := strings.SplitN(a.token("alice@x.com"), ":", 2)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "op@x.com:" + parts[1]})
	if got := a.Role(r); got != RoleAnon {
		t.Errorf("tampered cookie accepted as %v, want RoleAnon", got)
	}
}

func TestAuthRejectsExpiredCookie(t *testing.T) {
	a := testAuth()
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.tokenWithExpiry("op@x.com", time.Now().Add(-time.Hour).Unix())})
	if got := a.Role(r); got != RoleAnon {
		t.Errorf("expired cookie accepted as %v, want RoleAnon", got)
	}
	if email := a.Email(r); email != "" {
		t.Errorf("expired cookie yielded email %q, want empty", email)
	}
}

func TestAuthAcceptsUnexpiredCookie(t *testing.T) {
	a := testAuth()
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.tokenWithExpiry("op@x.com", time.Now().Add(time.Hour).Unix())})
	if got := a.Role(r); got != RoleHost {
		t.Errorf("unexpired cookie = %v, want RoleHost", got)
	}
}

func TestAuthRejectsTamperedExpiry(t *testing.T) {
	// Extending exp without re-signing must fail the HMAC check — a client
	// can't grant itself a longer session.
	a := testAuth()
	expired := a.tokenWithExpiry("op@x.com", time.Now().Add(-time.Hour).Unix())
	parts := strings.SplitN(expired, ":", 3) // email:exp:hmac
	forged := parts[0] + ":" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + ":" + parts[2]
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: forged})
	if got := a.Role(r); got != RoleAnon {
		t.Errorf("forged-expiry cookie accepted as %v, want RoleAnon", got)
	}
}

func TestAuthRevocationByRemoval(t *testing.T) {
	signer := NewAuth("s", "ghost@x.com", "", "")
	tok := signer.token("ghost@x.com")
	after := NewAuth("s", "alice@x.com", "", "")
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	if got := after.Role(r); got != RoleAnon {
		t.Errorf("removed email still resolves to %v, want RoleAnon", got)
	}
}

func TestOAuthHandleLoginNoLoopForRevoked(t *testing.T) {
	// A user with a validly-signed cookie whose email is NOT on the
	// allowlist must NOT be redirected to "/" (Guard would bounce them
	// back, looping). HandleLogin should render the sign-in page (200).
	signer := NewAuth("s", "ghost@x.com", "", "") // ghost was allowed when signed
	a := NewAuth("s", "alice@x.com", "", "")       // ghost since removed; same secret
	o := NewOAuth("id", "secret", "https://h/oauth/callback", a, nil)
	r := httptest.NewRequest("GET", "/login", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: signer.token("ghost@x.com")})
	w := httptest.NewRecorder()
	o.HandleLogin(w, r)
	if w.Code == http.StatusSeeOther {
		t.Fatalf("HandleLogin redirected (%d, Location=%q) a revoked user — loop risk; want 200 sign-in page",
			w.Code, w.Header().Get("Location"))
	}
}

func TestCSRFGuard(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	guard := csrfGuard(next)
	call := func(method, host, origin, referer string) int {
		r := httptest.NewRequest(method, "https://"+host+"/control", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		w := httptest.NewRecorder()
		guard.ServeHTTP(w, r)
		return w.Code
	}
	cases := []struct {
		name                    string
		method, host, orig, ref string
		want                    int
	}{
		{"same-origin POST passes", "POST", "watch.example.com", "https://watch.example.com", "", 299},
		{"cross-origin POST blocked", "POST", "watch.example.com", "https://evil.example.com", "", http.StatusForbidden},
		{"no origin/referer POST passes", "POST", "watch.example.com", "", "", 299},
		{"referer fallback same-host passes", "POST", "watch.example.com", "", "https://watch.example.com/watch", 299},
		{"referer fallback cross-host blocked", "POST", "watch.example.com", "", "https://evil.example.com/x", http.StatusForbidden},
		{"GET ignores cross-origin", "GET", "watch.example.com", "https://evil.example.com", "", 299},
	}
	for _, c := range cases {
		if got := call(c.method, c.host, c.orig, c.ref); got != c.want {
			t.Errorf("%s: status=%d, want %d", c.name, got, c.want)
		}
	}
}

func TestCSRFGuardHonorsForwardedHost(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	guard := csrfGuard(next)
	// Proxy forwards upstream Host=app:8080 but tags the real public host.
	r := httptest.NewRequest("POST", "http://app:8080/control", nil)
	r.Host = "app:8080"
	r.Header.Set("X-Forwarded-Host", "watch.example.com")
	r.Header.Set("Origin", "https://watch.example.com")
	w := httptest.NewRecorder()
	guard.ServeHTTP(w, r)
	if w.Code != 299 {
		t.Errorf("origin matching X-Forwarded-Host was blocked (status=%d); legitimate proxied POST must pass", w.Code)
	}
}

func TestNameCookieHardened(t *testing.T) {
	c := newNameCookie("Raw Socket", true)
	if c.Name != nameCookie {
		t.Errorf("cookie name = %q, want %q", c.Name, nameCookie)
	}
	if !c.HttpOnly {
		t.Error("name cookie should be HttpOnly (no client JS reads it)")
	}
	if !c.Secure {
		t.Error("name cookie should be Secure when requested")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Value != "Raw+Socket" {
		t.Errorf("value = %q, want url-escaped 'Raw+Socket'", c.Value)
	}
	if got := newNameCookie("x", false); got.Secure {
		t.Error("Secure should be false when not requested (plain HTTP)")
	}
}

func TestWithActorStashesEmail(t *testing.T) {
	a := testAuth()
	var seen string
	h := a.WithActor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = actorEmail(r)
	}))
	h.ServeHTTP(httptest.NewRecorder(), reqWithSession(a, "op@x.com"))
	if seen != "op@x.com" {
		t.Errorf("actorEmail = %q, want op@x.com", seen)
	}
	if got := actorEmail(httptest.NewRequest("GET", "/", nil)); got != "" {
		t.Errorf("actorEmail without middleware = %q, want empty", got)
	}
}

func TestGuardAndRequireHostAndAdmin(t *testing.T) {
	a := testAuth()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })

	w := httptest.NewRecorder()
	a.Guard(next).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusSeeOther {
		t.Errorf("Guard anon = %d, want 303", w.Code)
	}
	w = httptest.NewRecorder()
	a.Guard(next).ServeHTTP(w, reqWithSession(a, "alice@x.com"))
	if w.Code != 299 {
		t.Errorf("Guard allowed = %d, want passthrough", w.Code)
	}

	w = httptest.NewRecorder()
	a.RequireHost(next).ServeHTTP(w, reqWithSession(a, "alice@x.com"))
	if w.Code != http.StatusForbidden {
		t.Errorf("RequireHost viewer = %d, want 403", w.Code)
	}
	w = httptest.NewRecorder()
	a.RequireHost(next).ServeHTTP(w, reqWithSession(a, "op@x.com"))
	if w.Code != 299 {
		t.Errorf("RequireHost host = %d, want passthrough", w.Code)
	}

	w = httptest.NewRecorder()
	r := reqWithSession(a, "alice@x.com")
	r.URL.Path = "/admin/api/stats"
	a.RequireAdmin(next).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("RequireAdmin non-admin api = %d, want 401", w.Code)
	}
	w = httptest.NewRecorder()
	a.RequireAdmin(next).ServeHTTP(w, reqWithSession(a, "op@x.com"))
	if w.Code != 299 {
		t.Errorf("RequireAdmin admin = %d, want passthrough", w.Code)
	}
}
