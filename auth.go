package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"time"
)

const sessionCookie = "wp_session"

// Auth is a single-shared-password gate. Friends need only this password;
// they never see Plex. The signing secret is derived from the password so
// sessions survive restarts without extra config.
type Auth struct {
	password string
	secret   []byte
}

func NewAuth(password string) *Auth {
	mac := hmac.New(sha256.New, []byte("plexwatchparty-v1"))
	mac.Write([]byte(password))
	return &Auth{password: password, secret: mac.Sum(nil)}
}

func (a *Auth) token() string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte("authenticated"))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *Auth) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	want := a.token()
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

// Guard wraps a handler, redirecting unauthenticated browsers to /login and
// rejecting unauthenticated API/segment requests with 401.
func (a *Auth) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Accept") == "text/event-stream" ||
			r.URL.Path == "/control" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(loginHTML)
		return
	}
	if subtle.ConstantTimeCompare(
		[]byte(r.FormValue("password")), []byte(a.password)) != 1 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(loginHTML)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.token(),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
