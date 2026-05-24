package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	// Optional on-the-fly transcode through Plex's Universal Transcoder.
	// Empty / 0 keeps the legacy direct-stream behavior; any positive
	// integer targets 1080p h264 at that kbps (12000 is a sensible value
	// for high-quality watch-party streaming).
	transcodeKbps := 0
	if v := os.Getenv("PLEX_TRANSCODE_BITRATE_KBPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			log.Fatalf("PLEX_TRANSCODE_BITRATE_KBPS must be a non-negative integer, got %q", v)
		}
		transcodeKbps = n
	}
	if transcodeKbps > 0 {
		log.Printf("plex: on-the-fly transcode enabled → 1920x1080 h264 @ %d kbps", transcodeKbps)
	} else {
		log.Printf("plex: direct-stream mode (no transcode); set PLEX_TRANSCODE_BITRATE_KBPS to enable")
	}
	plex := NewPlex(plexURL, plexTok, libraryCache, transcodeKbps)
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
		// Disable bfcache + cached responses so a back-button arrival
		// always re-evaluates whether a movie is loaded (and reconnects
		// the SSE for the live state). Without this Chrome will happily
		// serve a stale /watch from history.
		w.Header().Set("Cache-Control", "no-store")
		// When no movie is loaded, route everyone to the waiting room
		// instead of the empty player. The host gets copy nudging them
		// back to the lobby; viewers get a "house lights are up" hold
		// screen that auto-redirects here once the host picks something.
		if rx.CurrentKey() == "" {
			w.Write(waitingHTML)
			return
		}
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

	protected.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		hub.HandleEvents(w, r, auth.EffectiveRole(r) == RoleHost)
	})
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
