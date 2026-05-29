package main

import (
	"net/http"
	"testing"
)

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
