package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	plexURL := os.Getenv("PLEX_BASE_URL")
	plexTok := os.Getenv("PLEX_TOKEN")
	password := os.Getenv("WATCH_PASSWORD")
	if plexURL == "" || plexTok == "" || password == "" {
		log.Fatal("set PLEX_BASE_URL, PLEX_TOKEN and WATCH_PASSWORD")
	}
	listen := env("LISTEN_ADDR", ":8080")
	workDir := env("WORK_DIR", filepath.Join(os.TempDir(), "plexwatchparty"))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		log.Fatal(err)
	}

	plex := NewPlex(plexURL, plexTok)
	rx := NewRemuxer(workDir)
	hub := NewHub(plex, rx)
	auth := NewAuth(password)

	mux := http.NewServeMux()

	// Public: only the login page.
	mux.HandleFunc("/login", auth.HandleLogin)

	// Everything else is behind the shared password.
	protected := http.NewServeMux()

	protected.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	protected.HandleFunc("/watch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(playerHTML)
	})

	protected.HandleFunc("/api/movies", func(w http.ResponseWriter, r *http.Request) {
		movies, err := plex.ListMovies()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(movies)
	})

	protected.HandleFunc("/events", hub.HandleEvents)
	protected.HandleFunc("/control", hub.HandleControl)

	// HLS playlist + segments come from the active remux session dir.
	// Clients only ever touch this — never Plex, never the token.
	protected.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
		dir := rx.SessionDir()
		if dir == "" {
			http.Error(w, "no active stream", http.StatusServiceUnavailable)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/hls/")
		if name == "" || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(dir, filepath.Base(name)))
	})

	mux.Handle("/", auth.Guard(protected))

	log.Printf("watch party on %s (workdir %s)", listen, workDir)
	log.Fatal(http.ListenAndServe(listen, mux))
}
