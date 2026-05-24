package main

import (
	"bytes"
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
	hostPassword := os.Getenv("HOST_PASSWORD")
	if plexURL == "" || plexTok == "" || password == "" {
		log.Fatal("set PLEX_BASE_URL, PLEX_TOKEN and WATCH_PASSWORD")
	}
	listen := env("LISTEN_ADDR", ":8080")
	workDir := env("WORK_DIR", filepath.Join(os.TempDir(), "plexwatchparty"))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		log.Fatal(err)
	}

	// Persist the library cache next to (not inside) the sessions dir so
	// the prune sweep never touches it.
	libraryCache := filepath.Join(filepath.Dir(workDir), "library-cache.json")
	plex := NewPlex(plexURL, plexTok, libraryCache)
	rx := NewRemuxer(workDir)
	rx.PruneOlderThan(7 * 24 * time.Hour)
	hub := NewHub(plex, rx)
	auth := NewAuth(password, hostPassword)
	bw := newBwTracker()
	if auth.HostEnabled() {
		log.Printf("auth: host role enabled — viewers cannot pick / drive playback")
	} else {
		log.Printf("auth: no HOST_PASSWORD configured — any authenticated viewer can drive playback")
	}

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
	// /control is host-gated. RequireHost is a no-op when HOST_PASSWORD
	// isn't configured (preserves "any-friend-can-drive" default).
	protected.Handle("/control", auth.RequireHost(http.HandlerFunc(hub.HandleControl)))

	protected.HandleFunc("/api/whoami", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"role":        auth.EffectiveRole(r).String(),
			"hostEnabled": auth.HostEnabled(),
		})
	})

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
		path := filepath.Join(dir, filepath.Base(name))
		w.Header().Set("Cache-Control", "no-cache")
		cw := &countingResponseWriter{ResponseWriter: w}
		defer func() { bw.record(clientIP(r), cw.n) }()

		switch filepath.Ext(name) {
		case ".m3u8":
			// hls.js (and Safari native HLS) treats a playlist tagged
			// PLAYLIST-TYPE:EVENT as a live stream — which gives it a
			// tiny sliding seekable window and makes scrub-back fail
			// silently. ffmpeg's vod playlist type DOESN'T write the
			// .m3u8 incrementally, so we can't just tell ffmpeg to use
			// it. Compromise: ffmpeg keeps writing an EVENT playlist
			// on disk, but we transform it to VOD on every response.
			// Players get the full seekable range; ffmpeg keeps
			// appending segments and the next playlist fetch picks
			// them up.
			content, err := os.ReadFile(path)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			content = bytes.ReplaceAll(content,
				[]byte("#EXT-X-PLAYLIST-TYPE:EVENT"),
				[]byte("#EXT-X-PLAYLIST-TYPE:VOD"))
			cw.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			cw.Write(content)
			return
		case ".m4s":
			w.Header().Set("Content-Type", "video/iso.segment")
		case ".mp4":
			w.Header().Set("Content-Type", "video/mp4")
		}
		http.ServeFile(cw, r, path)
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
