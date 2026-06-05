package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestPosterStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/metadata/55":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"title":"M","thumb":"/library/metadata/55/thumb/123","Media":[{"Part":[{"key":"/p"}]}]}]}}`))
		case r.URL.Path == "/library/metadata/55/thumb/123":
			if r.URL.Query().Get("X-Plex-Token") != "tok" {
				t.Errorf("token missing on thumb fetch")
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("JPEGBYTES"))
		case r.URL.Path == "/library/metadata/77":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"title":"NoThumb"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)

	body, ct, err := p.PosterStream("55")
	if err != nil {
		t.Fatalf("PosterStream: %v", err)
	}
	defer body.Close()
	if ct != "image/jpeg" {
		t.Errorf("content-type = %q", ct)
	}
	b, _ := io.ReadAll(body)
	if string(b) != "JPEGBYTES" {
		t.Errorf("body = %q", b)
	}

	if _, _, err := p.PosterStream("77"); err != errNoPoster {
		t.Errorf("no-thumb err = %v, want errNoPoster", err)
	}
}

func TestPosterStreamThumbStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/99":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"thumb":"/library/metadata/99/thumb/1"}]}}`))
		case "/library/metadata/99/thumb/1":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)
	body, _, err := p.PosterStream("99")
	if err == nil {
		if body != nil {
			body.Close()
		}
		t.Fatal("expected error on non-200 thumb fetch, got nil")
	}
}
