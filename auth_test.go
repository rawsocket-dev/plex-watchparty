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

func TestAuthTokenRoundTrip(t *testing.T) {
	a := NewAuth("watchpw", "hostpw")
	for _, role := range []Role{RoleHost, RoleViewer} {
		tok := a.token(role)
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		got := a.Role(r)
		if got != role {
			t.Errorf("role round-trip: got %v, want %v", got, role)
		}
	}
}

func TestAuthRoleAnonOnTamper(t *testing.T) {
	a := NewAuth("watchpw", "hostpw")
	tok := a.token(RoleHost)
	// Flip the role portion to "viewer" while keeping the host HMAC —
	// the HMAC verification should reject the swap.
	parts := strings.SplitN(tok, ":", 2)
	tampered := "viewer:" + parts[1]
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tampered})
	if got := a.Role(r); got != RoleAnon {
		t.Errorf("tampered token accepted as %v, want RoleAnon", got)
	}
}

func TestAuthEffectiveRoleUpgradesWhenNoHostPassword(t *testing.T) {
	a := NewAuth("watchpw", "") // no host password
	tok := a.token(RoleViewer)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	if got := a.EffectiveRole(r); got != RoleHost {
		t.Errorf("EffectiveRole without HOST_PASSWORD = %v, want RoleHost", got)
	}
}

func TestAuthHandleLoginSetsCookies(t *testing.T) {
	a := NewAuth("watchpw", "hostpw")
	form := "password=hostpw&name=Alice"
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.HandleLogin(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	var gotSession, gotName bool
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case sessionCookie:
			gotSession = c.Value != ""
		case nameCookie:
			gotName = strings.Contains(c.Value, "Alice")
		}
	}
	if !gotSession {
		t.Error("missing session cookie")
	}
	if !gotName {
		t.Error("missing or empty name cookie")
	}
}

func TestAuthHandleLogoutClears(t *testing.T) {
	a := NewAuth("watchpw", "hostpw")
	r := httptest.NewRequest("POST", "/logout", nil)
	w := httptest.NewRecorder()
	a.HandleLogout(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie || c.Name == nameCookie {
			if c.MaxAge >= 0 {
				t.Errorf("cookie %s MaxAge = %d, want negative (expire)", c.Name, c.MaxAge)
			}
		}
	}
}

func TestAdminCookieRoundTrip(t *testing.T) {
	a := NewAuth("watchpw", "hostpw")
	w := httptest.NewRecorder()
	a.SetAdminCookie(w, "Op@Example.COM")
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == adminCookie {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("admin cookie not set")
	}
	r := httptest.NewRequest("GET", "/admin", nil)
	r.AddCookie(cookie)
	if got := a.AdminEmail(r); got != "op@example.com" {
		t.Errorf("AdminEmail = %q, want normalized lowercase", got)
	}
}

func TestAdminCookieRejectsTamper(t *testing.T) {
	a := NewAuth("watchpw", "hostpw")
	w := httptest.NewRecorder()
	a.SetAdminCookie(w, "ok@example.com")
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == adminCookie {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("admin cookie not set")
	}
	// Swap the email but keep the HMAC; the HMAC should refuse to verify.
	parts := strings.SplitN(cookie.Value, ":", 2)
	tampered := "evil@example.com:" + parts[1]
	r := httptest.NewRequest("GET", "/admin", nil)
	r.AddCookie(&http.Cookie{Name: adminCookie, Value: tampered})
	if got := a.AdminEmail(r); got != "" {
		t.Errorf("tampered cookie accepted as %q, want empty", got)
	}
}

func TestRequireAdminGatesAccess(t *testing.T) {
	a := NewAuth("watchpw", "hostpw")
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	// No cookie → redirect for /admin, 401 for /admin/api/.
	r := httptest.NewRequest("GET", "/admin", nil)
	w := httptest.NewRecorder()
	a.RequireAdmin(next).ServeHTTP(w, r)
	if called {
		t.Error("RequireAdmin passed through without cookie")
	}
	if w.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", w.Code)
	}

	r = httptest.NewRequest("GET", "/admin/api/stats", nil)
	w = httptest.NewRecorder()
	a.RequireAdmin(next).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("api status = %d, want 401", w.Code)
	}

	// With a valid cookie, passes through.
	called = false
	w = httptest.NewRecorder()
	a.SetAdminCookie(w, "ok@example.com")
	cookies := w.Result().Cookies()
	r = httptest.NewRequest("GET", "/admin", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	a.RequireAdmin(next).ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Error("RequireAdmin blocked a valid admin cookie")
	}
}
