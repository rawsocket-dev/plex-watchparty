package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

// landOnWatch decides whether a user hitting "/" should be sent to the
// player instead of the library. When a movie is playing, only the single
// active host has anything to do on the library (pick / re-pick); everyone
// else — plain viewers AND host-eligible users who aren't the active host —
// belongs in /watch. Host-eligibility is NOT the same as being the active
// host, which is why this keys off isActiveHost rather than role.
func landOnWatch(moviePlaying, isActiveHost bool) bool {
	return moviePlaying && !isActiveHost
}

// newServer builds the public HTTP server. ReadHeaderTimeout bounds the
// request-header read phase so a slow client can't hold a connection open
// indefinitely (slowloris). Read/Write timeouts are deliberately left
// unset: /events (SSE) and /hls/* are long-lived streaming responses that
// a WriteTimeout would sever mid-stream. IdleTimeout caps idle keep-alives.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// segFlight collapses concurrent cold-misses for the same segment
// (rk, startMs, endMs) into a single upstream fetch. N viewers all
// landing on the same fresh segment trigger one Plex round-trip,
// one cache write, and N copies of the in-memory bytes — instead of
// N parallel Plex fetches racing each other into the same Put.
var segFlight singleflight.Group

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// fetchOrRecoverSegment is the cold-miss path: try Plex → on failure
// try the overlap-cache fallback → on failure try server-side
// recovery → on failure return the original Plex error. Wrapped in
// singleflight by the /hls/seg handler so N viewers cold-missing the
// same segment make ONE upstream call. On success, the segment is
// cached and the bytes returned for the caller (and any followers)
// to serve.
//
// Reads the full Plex body into memory before caching: the previous
// tee-via-pipe pattern committed a truncated cache entry whenever a
// client TCP-closed mid-stream (io.MultiWriter halts on first writer
// error, pipe closes, partial .tmp got renamed in). Buffering ~1–3
// MB per segment on a LAN is cheap and gives us an atomic "I have
// all the bytes" decision point.
func fetchOrRecoverSegment(
	ctx *segCtx,
	key cacheKey,
	plexSession *PlexSession,
	segCache *SegmentCache,
	hub *Hub,
) ([]byte, error) {
	// Plex first.
	if body, err := plexSession.FetchSegment(ctx.PlexURL); err == nil {
		defer body.Close()
		data, rerr := io.ReadAll(body)
		if rerr != nil {
			log.Printf("seg: read from plex aborted: %v", rerr)
			return nil, fmt.Errorf("plex read: %w", rerr)
		}
		if _, perr := segCache.Put(key, bytes.NewReader(data)); perr != nil {
			log.Printf("seg: cache write failed: %v (segment still served)", perr)
		}
		return data, nil
	} else {
		// Plex 4xx/5xx. Try cheaper recoveries before kicking off a
		// full server-side Restart.
		plexErr := err
		// Overlap-cache fallback: Plex sometimes 404s a segment it
		// already produced, or segment boundaries drift by a few ms
		// across sessions so the exact (startMs, endMs) key misses
		// while a near-identical segment is on disk.
		if fallback, fs, fe, ok := segCache.FindOverlapping(ctx.Rating, ctx.StartMs, ctx.EndMs); ok {
			log.Printf("seg: plex failed (%v); serving overlapping cache entry [%d,%d] for request [%d,%d]",
				plexErr, fs, fe, ctx.StartMs, ctx.EndMs)
			if data, ferr := os.ReadFile(fallback); ferr == nil {
				return data, nil
			} else {
				log.Printf("seg: overlap file read failed: %v", ferr)
			}
		}
		log.Printf("seg: fetch from plex failed: %v", plexErr)
		// Server-side recovery: we control the stream, Plex 404'd a
		// segment we need, restart the transcode at this segment's
		// time and serve a substitute from the new session.
		if data, rerr := hub.RecoverSegmentForRange(ctx.StartMs, ctx.EndMs); rerr == nil {
			if _, perr := segCache.Put(key, bytes.NewReader(data)); perr != nil {
				log.Printf("recover: cache write failed: %v (segment still served)", perr)
			}
			return data, nil
		} else {
			log.Printf("seg: server-side recovery failed: %v", rerr)
		}
		return nil, plexErr
	}
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

// version is the build's commit, injected at build time via
// -ldflags "-X main.version=<sha>" (see Dockerfile + .the registry-ci.yml).
// Defaults to "dev" for local `go build`.
var version = "dev"

func main() {
	log.Printf("plex-watchparty starting — build %s", version)
	plexURL := os.Getenv("PLEX_BASE_URL")
	plexTok := os.Getenv("PLEX_TOKEN")
	if plexURL == "" || plexTok == "" {
		log.Fatal("set PLEX_BASE_URL and PLEX_TOKEN")
	}
	googleID := os.Getenv("GOOGLE_CLIENT_ID")
	googleSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	googleRedirect := os.Getenv("GOOGLE_REDIRECT_URL")
	if googleID == "" || googleSecret == "" || googleRedirect == "" {
		log.Fatal("set GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET and GOOGLE_REDIRECT_URL")
	}
	allowedEmails := os.Getenv("ALLOWED_EMAILS")
	if strings.TrimSpace(allowedEmails) == "" {
		log.Fatal("set ALLOWED_EMAILS (comma-separated) — at least one address may sign in")
	}
	listen := env("LISTEN_ADDR", ":8080")
	// Forwarded-header trust: by default only LAN-side peers (loopback +
	// private ranges) may set X-Forwarded-For / X-Real-IP. Operators whose
	// proxy sits on a public address can widen this.
	if v := os.Getenv("TRUSTED_PROXY_CIDRS"); v != "" {
		trustedProxyNets = mustParseCIDRs(strings.Split(v, ","))
		log.Printf("net: trusting forwarded headers from %d configured proxy CIDR(s)", len(trustedProxyNets))
	}
	workDir := env("WORK_DIR", filepath.Join(os.TempDir(), "plexwatchparty"))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		log.Fatal(err)
	}

	// Persist the library cache next to (not inside) the sessions dir so
	// the prune sweep never touches it.
	libraryCache := filepath.Join(filepath.Dir(workDir), "library-cache.json")
	audit := NewAuditLog(filepath.Join(filepath.Dir(workDir), "audit.jsonl"), auditCap)
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
	plex := NewPlex(plexURL, plexTok, libraryCache, audit)

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
	cs := segCache.Stats()
	log.Printf("cache: %d entries loaded, %d MB on disk, cap %d GB",
		cs.Entries, cs.TotalBytes/1024/1024, cacheGB)

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

	// Resume hint store: small JSON file capturing the last known
	// playback state, refreshed on every broadcast. Lets us offer
	// "Resume where you left off?" after a container restart or
	// idle shutdown. Sits next to recent.json.
	statePath := filepath.Join(filepath.Dir(workDir), "state.json")
	stateStore := NewStateStore(statePath)

	hub := NewHub(plex, plexSession, segCache, recent, stateStore, audit)
	auth := NewAuth(googleSecret, allowedEmails, os.Getenv("HOST_EMAILS"), os.Getenv("ADMIN_EMAILS"))
	oauth := NewOAuth(googleID, googleSecret, googleRedirect, auth, audit)
	bw := newBwTracker()
	if len(auth.hosts) == 0 {
		log.Printf("auth: HOST_EMAILS empty — every allowed user can drive playback")
	}

	// Segment-context codec — encrypts the per-segment context (upstream
	// Plex URL + token) into the opaque /hls/seg/<blob>.ts blob. Keyed off
	// the Plex token: stable across restarts, already secret, never sent
	// to clients. See playlist.go.
	codec, err := newSegCodec(plexTok)
	if err != nil {
		log.Fatalf("seg codec init: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/login", oauth.HandleLogin)
	mux.HandleFunc("/logout", auth.HandleLogout)
	mux.HandleFunc("/oauth/start", oauth.HandleStart)
	mux.HandleFunc("/oauth/callback", oauth.HandleCallback)

	// Admin maintenance panel — same identity, gated on ADMIN_EMAILS.
	registerAdminRoutes(mux, auth, plex, segCache, plexSession, hub, bw, audit)

	// Everything else is behind the Google-identity Guard.
	protected := http.NewServeMux()

	protected.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// If someone who isn't the ACTIVE host lands here while a movie is
		// already loaded, skip the library — they can't pick anything from
		// it anyway — and route them straight to /watch where they get the
		// player (or the "take your seat" waiting room if the session has
		// since cleared). This includes host-ELIGIBLE users who aren't the
		// one currently driving: a second host logging in mid-movie joins
		// as a viewer instead of getting the library.
		if landOnWatch(plexSession.RatingKey() != "", hub.IsActiveHost(auth.Email(r))) {
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
		hub.HandleEvents(w, r, auth.Role(r) == RoleHost, auth.Email(r))
	})
	// /control is gated inside HandleControl on being the active host
	// (eligibility alone is no longer sufficient — admin/hand-off can
	// promote a non-eligible user). WithActor stashes the email; the
	// protected Guard still requires an authenticated, allow-listed user.
	protected.Handle("/control", auth.WithActor(http.HandlerFunc(hub.HandleControl)))

	protected.HandleFunc("/api/host/handoff", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if err := hub.Handoff(auth.Email(r), id); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	protected.HandleFunc("/api/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"email":        auth.Email(r),
			"role":         auth.Role(r).String(),
			"isAdmin":      auth.IsAdmin(r),
			"isActiveHost": hub.IsActiveHost(auth.Email(r)),
			"name":         viewerNameFromRequest(r),
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
		rewritten, segs, err := rewritePlaylist(codec, raw, baseURL, plexSession.OffsetMs(), ratingKey)
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
		ctx, err := codec.decode(name)
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
			segCache.RecordHit()
			http.ServeFile(cw, r, path)
			plexSession.RecordSegmentSuccess()
			return
		}
		segCache.RecordMiss()
		// Cache miss: singleflight'd Plex fetch + recovery cascade.
		// Multiple viewers cold-missing the same segment collapse to
		// one upstream request; followers reuse the leader's bytes.
		flightKey := fmt.Sprintf("%s:%d:%d", ctx.Rating, ctx.StartMs, ctx.EndMs)
		v, ferr, _ := segFlight.Do(flightKey, func() (interface{}, error) {
			return fetchOrRecoverSegment(ctx, key, plexSession, segCache, hub)
		})
		if ferr != nil {
			log.Printf("seg: cold-miss path failed: %v", ferr)
			// Recovery itself failed. Track the streak; the existing
			// safety net handles "Plex is fundamentally wedged" by
			// running its own restart at the current play position
			// after segFailureThreshold consecutive misses.
			if plexSession.RecordSegmentFailure() {
				go func() {
					defer plexSession.ClearAutoRestart()
					if !plexSession.AutoRestartShouldProceed() {
						log.Printf("auto-restart: superseded by host action, skipping")
						return
					}
					if err := hub.RestartAtCurrentPosition(RestartByAuto); err != nil {
						log.Printf("auto-restart failed: %v", err)
					}
				}()
			}
			http.Error(cw, "plex segment: "+ferr.Error(), http.StatusBadGateway)
			return
		}
		plexSession.RecordSegmentSuccess()
		_, _ = cw.Write(v.([]byte))
	})

	// Lightweight per-connection telemetry: the player POSTs its
	// currentTime + paused state every few seconds so the admin
	// roster can show "what's each viewer actually seeing right now?"
	// Cookie-gated (any authenticated viewer can heartbeat themselves);
	// the clientId comes from the SSE init message.
	protected.HandleFunc("/api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		var req struct {
			ClientID    string  `json:"clientId"`
			PositionSec float64 `json:"positionSec"`
			Paused      bool    `json:"paused"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.ClientID == "" {
			http.Error(w, "clientId required", http.StatusBadRequest)
			return
		}
		// Stale id (post-disconnect) is a 204 not a 404 — the client's
		// next SSE reconnect mints a fresh id and the heartbeats catch
		// up. Surfacing a 404 would just spam the player logs.
		hub.RecordHeartbeat(req.ClientID, req.PositionSec, req.Paused)
		w.WriteHeader(http.StatusNoContent)
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
	log.Fatal(newServer(listen, mux).ListenAndServe())
}
