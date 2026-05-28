package main

import (
	"bytes"
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

// writeJSON is the standard one-shot JSON response: application/json
// + no-store cache header + Encode. Used by every /api/* and the
// inline JSON responses from /control. Encode errors are logged but
// not propagated — by the time Encode runs, the client has already
// seen 200 OK so there's nothing to surface.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: encode failed: %v", err)
	}
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
	// Plex Universal Transcoder target bitrate, in kbps. There's no
	// "direct stream" mode any more — every play goes through Plex's
	// transcoder (we proxy + cache its HLS output). 12000 is a
	// reasonable default for h264 1080p at high quality.
	transcodeKbps := 12000
	if v := os.Getenv("PLEX_TRANSCODE_BITRATE_KBPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Fatalf("PLEX_TRANSCODE_BITRATE_KBPS must be a positive integer, got %q", v)
		}
		transcodeKbps = n
	}
	log.Printf("plex: transcode target → 1920x1080 h264 @ %d kbps", transcodeKbps)
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

	hub := NewHub(plex, plexSession, segCache, recent)
	auth := NewAuth(password, hostPassword)
	bw := newBwTracker()

	// Optional Google OAuth gate for /admin. All four env vars must
	// be set for the admin surface to wire up; with any missing the
	// /admin routes are simply not registered and the panel is 404.
	oauth := NewOAuth(
		os.Getenv("ADMIN_GOOGLE_CLIENT_ID"),
		os.Getenv("ADMIN_GOOGLE_CLIENT_SECRET"),
		os.Getenv("ADMIN_GOOGLE_REDIRECT_URL"),
		os.Getenv("ADMIN_GOOGLE_ALLOWED_EMAILS"),
		auth,
	)
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

	// Admin surface — Google OAuth gate, separate cookie. Opt-in via
	// env vars; if not configured, /admin routes simply don't exist.
	if oauth.Configured() {
		registerAdminRoutes(mux, oauth, auth, plex, segCache, plexSession, hub)
	}

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

	// Static assets are content-addressed via their embedded bytes —
	// versions only change at build time, so a far-future immutable
	// cache header is safe and avoids re-fetches on every page load.
	protected.HandleFunc("/static/common.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Write(commonJS)
	})
	protected.HandleFunc("/static/player.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Write(playerCSS)
	})
	protected.HandleFunc("/static/player.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Write(playerJS)
	})
	protected.HandleFunc("/static/index.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Write(indexCSS)
	})
	protected.HandleFunc("/static/index.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Write(indexJS)
	})

	protected.HandleFunc("/api/movies", func(w http.ResponseWriter, r *http.Request) {
		movies, err := plex.ListMovies()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, movies)
	})

	protected.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		hub.HandleEvents(w, r, auth.EffectiveRole(r) == RoleHost)
	})
	// /control is host-gated. RequireHost is a no-op when HOST_PASSWORD
	// isn't configured (preserves "any-friend-can-drive" default).
	protected.Handle("/control", auth.RequireHost(http.HandlerFunc(hub.HandleControl)))

	protected.HandleFunc("/api/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"role":        auth.EffectiveRole(r).String(),
			"hostEnabled": auth.HostEnabled(),
			// Server-side name resolution (cookie → sanitize → "guest"
			// fallback) so the client never has to duplicate the
			// guest-fallback logic across player/library/waiting pages.
			"name": viewerNameFromRequest(r),
		})
	})

	// One-shot view of current playback state. Used by the library so
	// it can detect "the movie you just clicked is already loaded" and
	// offer Resume / Start over before issuing the /control load.
	protected.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, hub.Snapshot())
	})

	// Recently-played list, newest first. Used by the waiting-room
	// page so the host (or anyone) can re-pick a recent movie with
	// one click instead of going through the full library.
	protected.HandleFunc("/api/recent", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, recent.List())
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
		// Segment URLs are content-addressed via the base64 segCtx in
		// the path, so the bytes for a given URL never change. Let the
		// browser cache them aggressively — backward seek into a
		// previously-fetched range skips a server round-trip entirely.
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		cw := &countingResponseWriter{ResponseWriter: w}
		defer func() { bw.record(clientIP(r), cw.n) }()

		// Cache hit: sendfile-fast path.
		if path, ok := segCache.Get(key); ok {
			http.ServeFile(cw, r, path)
			plexSession.RecordSegmentSuccess()
			return
		}
		// Cache miss: fetch from Plex, tee to cache + client.
		body, err := plexSession.FetchSegment(ctx.PlexURL)
		if err != nil {
			// Last-resort: Plex sometimes 404s a segment it already
			// produced (in-session cache eviction), and segment
			// boundaries can drift by a few ms across sessions so
			// the exact (startMs, endMs) key misses while a near-
			// identical segment is on disk. Check for any cached
			// segment whose window overlaps the requested range and
			// serve that instead of forwarding the 404.
			if fallback, fs, fe, ok := segCache.FindOverlapping(ctx.Rating, ctx.StartMs, ctx.EndMs); ok {
				log.Printf("seg: plex failed (%v); serving overlapping cache entry [%d,%d] for request [%d,%d]",
					err, fs, fe, ctx.StartMs, ctx.EndMs)
				http.ServeFile(cw, r, fallback)
				plexSession.RecordSegmentSuccess()
				return
			}
			log.Printf("seg: fetch from plex failed: %v", err)
			// Track consecutive failures. After N in a row, trigger
			// an auto-restart at the current play position — covers
			// "Plex's session got into a weird state we can't fetch
			// out of" without the user having to manually intervene.
			if plexSession.RecordSegmentFailure() {
				go func() {
					defer plexSession.ClearAutoRestart()
					// A host seek-with-restart can flip autoRestart
					// inactive between RecordSegmentFailure and us
					// running. If so, bail — the host's intent wins.
					if !plexSession.AutoRestartShouldProceed() {
						log.Printf("auto-restart: superseded by host action, skipping")
						return
					}
					if err := hub.AutoRestartAtCurrentPosition(); err != nil {
						log.Printf("auto-restart failed: %v", err)
					}
				}()
			}
			http.Error(cw, "plex segment: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer body.Close()
		// Read the full Plex segment into memory before serving OR
		// caching. The previous tee-via-pipe pattern would commit a
		// truncated cache entry whenever the client TCP-closed mid-
		// stream: io.MultiWriter halts on the first writer error, the
		// pipe goroutine closes the pipe early, and Put renamed a
		// partial .tmp into the cache forever. Buffering ~1-3 MB per
		// segment on a LAN is cheap (sub-100ms on gigabit) and gives
		// us an atomic "I have all the bytes" decision point.
		data, err := io.ReadAll(body)
		if err != nil {
			log.Printf("seg: read from plex aborted: %v", err)
			if plexSession.RecordSegmentFailure() {
				go func() {
					defer plexSession.ClearAutoRestart()
					if !plexSession.AutoRestartShouldProceed() {
						log.Printf("auto-restart: superseded by host action, skipping")
						return
					}
					if err := hub.AutoRestartAtCurrentPosition(); err != nil {
						log.Printf("auto-restart failed: %v", err)
					}
				}()
			}
			http.Error(cw, "plex segment read: "+err.Error(), http.StatusBadGateway)
			return
		}
		plexSession.RecordSegmentSuccess()
		// Cache first — quick local disk write — so a slow client
		// disconnect during cw.Write can't keep the entry out of
		// cache. The cache is the value here; serving the byte to
		// THIS client is incidental.
		if _, err := segCache.Put(key, bytes.NewReader(data)); err != nil {
			log.Printf("seg: cache write failed: %v (segment still served)", err)
		}
		_, _ = cw.Write(data)
	})

	protected.HandleFunc("/api/bandwidth", func(w http.ResponseWriter, r *http.Request) {
		mine, total, viewers := bw.snapshot(clientIP(r))
		writeJSON(w, map[string]int64{
			"mineKbps":  mine,
			"totalKbps": total,
			"viewers":   int64(viewers),
		})
	})

	mux.Handle("/", auth.Guard(protected))

	log.Printf("watch party on %s (workdir %s)", listen, workDir)
	log.Fatal(http.ListenAndServe(listen, mux))
}
