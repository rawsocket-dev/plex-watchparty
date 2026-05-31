package main

import (
	"net/http"
	"testing"
)

// A user hitting "/" should be sent to the player (not the library) when a
// movie is playing and they are NOT the active host — host-eligibility
// alone doesn't entitle them to the library, since only the single active
// host can pick. The active host always sees the library so they can
// re-pick.
func TestLandOnWatch(t *testing.T) {
	cases := []struct {
		name            string
		playing, active bool
		want            bool
	}{
		{"no movie, not active host", false, false, false},
		{"no movie, active host", false, true, false},
		{"movie playing, active host stays on library", true, true, false},
		{"movie playing, non-active-host routes to watch", true, false, true},
	}
	for _, c := range cases {
		if got := landOnWatch(c.playing, c.active); got != c.want {
			t.Errorf("%s: landOnWatch(playing=%v, active=%v) = %v, want %v",
				c.name, c.playing, c.active, got, c.want)
		}
	}
}

// Finding-3: the public server must bound the request-header read phase
// (slowloris protection) while leaving Read/Write timeouts unset so that
// long-lived SSE (/events) and HLS streaming responses aren't severed.
func TestNewServerTimeouts(t *testing.T) {
	srv := newServer(":0", http.NewServeMux())
	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want > 0", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want > 0", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (SSE/HLS are long-lived)", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 (SSE/HLS are long-lived)", srv.ReadTimeout)
	}
}
