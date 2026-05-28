package main

import (
	"log"
	"net/http"
	"strconv"
	"time"
)

// registerAdminRoutes wires the /admin/* tree onto the public mux.
// Login + OAuth callback are reachable without auth; everything else
// is wrapped in RequireAdmin so a valid wp_admin cookie is required.
func registerAdminRoutes(
	mux *http.ServeMux,
	oauth *OAuth,
	auth *Auth,
	plex *Plex,
	segCache *SegmentCache,
	plexSession *PlexSession,
	hub *Hub,
	bw *bwTracker,
) {
	// --- public sign-in surface ---
	mux.HandleFunc("/admin/login", oauth.HandleLogin)
	mux.HandleFunc("/admin/oauth/start", oauth.HandleStart)
	mux.HandleFunc("/admin/oauth/callback", oauth.HandleCallback)
	mux.HandleFunc("/admin/logout", oauth.HandleLogout)

	// --- gated admin panel + API ---
	gated := auth.RequireAdmin

	mux.Handle("/admin", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(adminHTML)
	})))
	mux.Handle("/admin/", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})))

	// Static assets — short-cached but not immutable so admin tweaks
	// don't require a hard reload at the browser.
	mux.Handle("/admin/static/admin.css", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(adminCSS)
	})))
	mux.Handle("/admin/static/admin.js", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(adminJS)
	})))
	// Same common.js as the player serves under /static/common.js —
	// re-served under the admin tree so the admin panel doesn't need a
	// watch-password session to fetch shared helpers (escapeHTML, etc).
	mux.Handle("/admin/static/common.js", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(commonJS)
	})))

	// --- JSON API ---
	mux.Handle("/admin/api/whoami", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"email": auth.AdminEmail(r)})
	})))

	mux.Handle("/admin/api/stats", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := adminSnapshot(plex, segCache, plexSession, hub, bw)
		writeJSON(w, st)
	})))

	mux.Handle("/admin/api/cache/clear", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		rk := r.URL.Query().Get("ratingKey")
		var entries int
		var bytes int64
		if rk != "" {
			entries, bytes = segCache.ClearMovie(rk)
			log.Printf("admin: %s cleared cache for ratingKey=%s (%d entries, %d bytes)",
				auth.AdminEmail(r), rk, entries, bytes)
		} else {
			entries, bytes = segCache.Clear()
			log.Printf("admin: %s cleared entire cache (%d entries, %d bytes)",
				auth.AdminEmail(r), entries, bytes)
		}
		writeJSON(w, map[string]any{
			"entriesRemoved": entries,
			"bytesRemoved":   bytes,
		})
	})))

	mux.Handle("/admin/api/cache/prune", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if days <= 0 {
			http.Error(w, "days must be positive", http.StatusBadRequest)
			return
		}
		entries, bytes := segCache.Prune(time.Duration(days) * 24 * time.Hour)
		log.Printf("admin: %s pruned cache older than %d days (%d entries, %d bytes)",
			auth.AdminEmail(r), days, entries, bytes)
		writeJSON(w, map[string]any{
			"entriesRemoved": entries,
			"bytesRemoved":   bytes,
		})
	})))

	mux.Handle("/admin/api/library/refresh", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		plex.RefreshLibrary()
		// Kick a fetch synchronously so the response carries the
		// fresh title count; if Plex is slow, the request hangs for
		// that fetch (admin-only, infrequent — OK).
		movies, err := plex.ListMovies()
		if err != nil {
			log.Printf("admin: %s library refresh failed: %v", auth.AdminEmail(r), err)
			http.Error(w, "refresh failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("admin: %s refreshed library (%d titles)", auth.AdminEmail(r), len(movies))
		writeJSON(w, map[string]any{"titles": len(movies)})
	})))

	mux.Handle("/admin/api/session/restart", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if plexSession.RatingKey() == "" {
			http.Error(w, "no active session", http.StatusBadRequest)
			return
		}
		log.Printf("admin: %s manual session restart at current position", auth.AdminEmail(r))
		// Suppress any in-flight auto-restart racing in from the seg
		// proxy — admin intent wins.
		plexSession.SuppressAutoRestart()
		if err := hub.RestartAtCurrentPosition(RestartByAdmin); err != nil {
			http.Error(w, "restart failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))

	mux.Handle("/admin/api/bandwidth/history", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"samples": bw.History(),
		})
	})))

	mux.Handle("/admin/api/session/stop", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if plexSession.RatingKey() == "" {
			http.Error(w, "no active session", http.StatusBadRequest)
			return
		}
		log.Printf("admin: %s sent room to lobby", auth.AdminEmail(r))
		// Final timeline report before the session goes away, then
		// tear down. The hub's state.RatingKey going blank triggers
		// every connected /watch page to reload into the waiting room.
		hub.SendEveryoneToLobby()
		w.WriteHeader(http.StatusNoContent)
	})))

	mux.Handle("/admin/api/viewers/kick", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		ok := hub.KickClient(id)
		if !ok {
			http.Error(w, "no such connection", http.StatusNotFound)
			return
		}
		log.Printf("admin: %s kicked connection id=%s", auth.AdminEmail(r), id)
		w.WriteHeader(http.StatusNoContent)
	})))
}

// AdminSnapshot bundles the data sources the admin panel displays in
// one round-trip.
type AdminSnapshot struct {
	Cache     CacheStats            `json:"cache"`
	Library   LibraryStats          `json:"library"`
	Session   SessionSummary        `json:"session"`
	Lifecycle SessionLifecycleStats `json:"lifecycle"`
	Viewers   []AdminViewer         `json:"viewers"`
}

// SessionSummary is the slice of Plex session state surfaced to the
// admin panel (read from the Hub's view, not PlexSession directly,
// so it includes title + duration).
type SessionSummary struct {
	RatingKey    string  `json:"ratingKey"`
	Title        string  `json:"title"`
	Playing      bool    `json:"playing"`
	PositionSec  float64 `json:"positionSec"`
	DurationSec  float64 `json:"durationSec"`
	SessionToken int64   `json:"sessionToken"`
}

func adminSnapshot(plex *Plex, cache *SegmentCache, sess *PlexSession, hub *Hub, bw *bwTracker) AdminSnapshot {
	state := hub.Snapshot()
	return AdminSnapshot{
		Cache:   cache.Stats(),
		Library: plex.Stats(),
		Session: SessionSummary{
			RatingKey:    state.RatingKey,
			Title:        state.Title,
			Playing:      state.Playing,
			PositionSec:  state.PositionSec,
			DurationSec:  state.DurationSec,
			SessionToken: state.SessionToken,
		},
		Lifecycle: sess.LifecycleStats(),
		Viewers:   hub.AdminRoster(bw),
	}
}
