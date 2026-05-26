package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	plex := NewPlex(plexURL, plexTok, libraryCache)

	// Disk cache for HLS segments. Sized by CACHE_MAX_GB (default 20 GB).
	// Survives container restarts so previously-watched ranges of a
	// movie are instant-seekable even after a reboot.
	cacheGB := 20
	if v := os.Getenv("CACHE_MAX_GB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Fatalf("CACHE_MAX_GB must be a positive integer, got %q", v)
		}
		cacheGB = n
	}
	cacheDir := filepath.Join(filepath.Dir(workDir), "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Fatalf("create cache dir: %v", err)
	}
	segCache := NewSegmentCache(cacheDir, int64(cacheGB)*1024*1024*1024)
	if err := segCache.LoadFromDisk(); err != nil {
		log.Printf("cache: LoadFromDisk warning: %v", err)
	}
	log.Printf("cache: %d entries loaded, %d MB on disk, cap %d GB",
		len(segCache.entries), segCache.totalBytes/1024/1024, cacheGB)

	plexSession := NewPlexSession(plex, transcodeKbps)
	// Startup health check. On success we log the friendly identity;
	// on failure we hand off to Plex's own recovery loop, which polls
	// in the background until reachability returns. Mid-run drops use
	// the same loop — any failing call inside Plex.Do trips it.
	if id, err := plex.Ping(); err == nil {
		log.Printf("plex: connected to %q (version %s, %s %s, machine %s)",
			id.FriendlyName, id.Version, id.Platform, id.PlatformVersion, id.MachineIdentifier)
	} else {
		plex.MarkUnhealthy(err)
	}
	// Recent-played list shown on the waiting room. Persisted in the
	// same dir as the library cache (one level above WORK_DIR) so a
	// cache prune doesn't wipe it.
	recentPath := filepath.Join(filepath.Dir(workDir), "recent.json")
	recent := NewRecentMovies(recentPath)
	recent.Load()

	hub := NewHub(plex, plexSession, segCache)
	hub.recent = recent
	auth := NewAuth(password, hostPassword)
	bw := newBwTracker()
	if auth.HostEnabled() {
		log.Printf("auth: host role enabled — viewers cannot pick / drive playback")
	} else {
		log.Printf("auth: no HOST_PASSWORD configured — any authenticated viewer can drive playback")
	}

	mux := http.NewServeMux()

	// Public: only the login + logout pages. Logout is public so
	// clicking it from a stale page (cookie already invalid) still
	// lands cleanly on /login instead of bouncing through the Guard.
	mux.HandleFunc("/login", auth.HandleLogin)
	mux.HandleFunc("/logout", auth.HandleLogout)

	// Everything else is behind the shared password.
	protected := http.NewServeMux()

	protected.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// If a viewer (non-host) lands here while a movie is already
		// loaded, skip the library — they can't pick anything from it
		// anyway — and route them straight to /watch where they get
		// the player (or the "take your seat" waiting room if the
		// session has since cleared).
		if auth.EffectiveRole(r) != RoleHost && plexSession.RatingKey() != "" {
			http.Redirect(w, r, "/watch", http.StatusSeeOther)
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
		if plexSession.RatingKey() == "" {
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

	// One-shot view of current playback state. Used by the library so
	// it can detect "the movie you just clicked is already loaded" and
	// offer Resume / Start over before issuing the /control load.
	protected.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(hub.Snapshot())
	})

	// Recently-played list, newest first. Used by the waiting-room
	// page so the host (or anyone) can re-pick a recent movie with
	// one click instead of going through the full library.
	protected.HandleFunc("/api/recent", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(recent.List())
	})

	// /hls/index.m3u8 — fetch Plex's playlist, rewrite segment URLs to
	// point through us, return to client. Plex's segment URLs are kept
	// internal (never exposed to viewers); they pass through our proxy
	// where we can cache and re-serve.
	protected.HandleFunc("/hls/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		ratingKey := plexSession.RatingKey()
		if ratingKey == "" {
			http.Error(w, "no active stream", http.StatusServiceUnavailable)
			return
		}
		raw, baseURL, err := plexSession.FetchPlaylist()
		if err != nil {
			log.Printf("playlist: fetch failed: %v", err)
			http.Error(w, "playlist fetch: "+err.Error(), http.StatusBadGateway)
			return
		}
		rewritten, segs, err := rewritePlaylist(raw, baseURL, plexSession.OffsetMs(), ratingKey)
		if err != nil {
			log.Printf("playlist: parse failed: %v", err)
			http.Error(w, "playlist parse: "+err.Error(), http.StatusBadGateway)
			return
		}
		// Update edge tracker so seek-forward-past-edge detection is accurate.
		if len(segs) > 0 {
			plexSession.UpdateEdge(segs[len(segs)-1].EndMs)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		cw := &countingResponseWriter{ResponseWriter: w}
		cw.Write(rewritten)
		bw.record(clientIP(r), cw.n)
	})

	// /hls/seg/<encoded>.ts — decode the segment context, serve from
	// cache if present, otherwise proxy from Plex while tee-writing to
	// the cache for future requests.
	protected.HandleFunc("/hls/seg/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/hls/seg/")
		name = strings.TrimSuffix(name, ".ts")
		ctx, err := decodeSegCtx(name)
		if err != nil {
			log.Printf("seg: decode failed name=%q: %v", name, err)
			http.NotFound(w, r)
			return
		}
		key := cacheKey{ratingKey: ctx.Rating, startMs: ctx.StartMs, endMs: ctx.EndMs}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "no-cache")
		cw := &countingResponseWriter{ResponseWriter: w}
		defer func() { bw.record(clientIP(r), cw.n) }()

		// Cache hit: sendfile-fast path.
		if path, ok := segCache.Get(key); ok {
			http.ServeFile(cw, r, path)
			return
		}
		// Cache miss: fetch from Plex, tee to cache + client.
		body, err := plexSession.FetchSegment(ctx.PlexURL)
		if err != nil {
			log.Printf("seg: fetch from plex failed: %v", err)
			http.Error(cw, "plex segment: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer body.Close()
		// pipe: write into cache while streaming to client.
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_, _ = io.Copy(io.MultiWriter(pw, cw), body)
		}()
		if _, err := segCache.Put(key, pr); err != nil {
			log.Printf("seg: cache write failed: %v (segment still served)", err)
		}
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
