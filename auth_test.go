package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
