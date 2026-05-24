package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	rx.PruneOlderThan(7 * 24 * time.Hour)
	hub := NewHub(plex, rx)
	auth := NewAuth(password)
	bw := newBwTracker()

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
	// We wrap the ResponseWriter so the bandwidth tracker sees every
	// byte streamed, keyed by client IP.
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
		// Go's mime.TypeByExtension doesn't know .m4s and inconsistently
		// handles .m3u8. Set them explicitly so strict browsers / MSE
		// don't refuse a segment served as application/octet-stream.
		switch filepath.Ext(name) {
		case ".m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		case ".m4s":
			w.Header().Set("Content-Type", "video/iso.segment")
		case ".mp4":
			w.Header().Set("Content-Type", "video/mp4")
		}
		w.Header().Set("Cache-Control", "no-cache")
		cw := &countingResponseWriter{ResponseWriter: w}
		http.ServeFile(cw, r, filepath.Join(dir, filepath.Base(name)))
		bw.record(clientIP(r), cw.n)
	})

	protected.HandleFunc("/api/bandwidth", func(w http.ResponseWriter, r *http.Request) {
		mine, total, viewers := bw.snapshot(clientIP(r))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"mineKbps":  mine,
			"totalKbps": total,
			"viewers":   int64(viewers),
		})
	})

	mux.Handle("/", auth.Guard(protected))

	log.Printf("watch party on %s (workdir %s)", listen, workDir)
	log.Fatal(http.ListenAndServe(listen, mux))
}
