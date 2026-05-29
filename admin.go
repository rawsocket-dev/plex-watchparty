package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// registerAdminRoutes wires the /admin/* tree onto the public mux.
// Everything is wrapped in RequireAdmin so a valid identity cookie is required.
func registerAdminRoutes(
	mux *http.ServeMux,
	auth *Auth,
	plex *Plex,
	segCache *SegmentCache,
	plexSession *PlexSession,
	hub *Hub,
	bw *bwTracker,
	audit *AuditLog,
) {
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
		writeJSON(w, map[string]any{"email": auth.Email(r)})
	})))

	mux.Handle("/admin/api/stats", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := adminSnapshot(plex, segCache, plexSession, hub, bw)
		writeJSON(w, st)
	})))

	mux.Handle("/admin/api/audit", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, audit.List())
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
				auth.Email(r), rk, entries, bytes)
			audit.Record(AuditEvent{Type: "admin", Email: auth.Email(r), Role: "admin", IP: clientIP(r),
				Detail: fmt.Sprintf("cleared cache for ratingKey=%s (%d entries)", rk, entries)})
		} else {
			entries, bytes = segCache.Clear()
			log.Printf("admin: %s cleared entire cache (%d entries, %d bytes)",
				auth.Email(r), entries, bytes)
			audit.Record(AuditEvent{Type: "admin", Email: auth.Email(r), Role: "admin", IP: clientIP(r),
				Detail: fmt.Sprintf("cleared entire cache (%d entries)", entries)})
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
			auth.Email(r), days, entries, bytes)
		audit.Record(AuditEvent{Type: "admin", Email: auth.Email(r), Role: "admin", IP: clientIP(r),
			Detail: fmt.Sprintf("pruned cache older than %d days (%d entries)", days, entries)})
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
			log.Printf("admin: %s library refresh failed: %v", auth.Email(r), err)
			http.Error(w, "refresh failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("admin: %s refreshed library (%d titles)", auth.Email(r), len(movies))
		audit.Record(AuditEvent{Type: "admin", Email: auth.Email(r), Role: "admin", IP: clientIP(r),
			Detail: fmt.Sprintf("refreshed library (%d titles)", len(movies))})
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
		log.Printf("admin: %s manual session restart at current position", auth.Email(r))
		// Suppress any in-flight auto-restart racing in from the seg
		// proxy — admin intent wins.
		plexSession.SuppressAutoRestart()
		if err := hub.RestartAtCurrentPosition(RestartByAdmin); err != nil {
			http.Error(w, "restart failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		// Record only after the restart actually succeeded, so the audit
		// trail never shows a restart that errored out.
		audit.Record(AuditEvent{Type: "admin", Email: auth.Email(r), Role: "admin", IP: clientIP(r),
			Detail: "restarted plex session at current position"})
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
		log.Printf("admin: %s sent room to lobby", auth.Email(r))
		audit.Record(AuditEvent{Type: "admin", Email: auth.Email(r), Role: "admin", IP: clientIP(r),
			Detail: "sent everyone to lobby"})
		// Final timeline report before the session goes away, then
		// tear down. The hub's state.RatingKey going blank triggers
		// every connected /watch page to reload into the waiting room.
		hub.SendEveryoneToLobby()
		w.WriteHeader(http.StatusNoContent)
	})))

	mux.Handle("/admin/api/host/set", gated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if err := hub.SetActiveHostByConn(id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		log.Printf("admin: %s set active host (conn id=%s)", auth.Email(r), id)
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
		log.Printf("admin: %s kicked connection id=%s", auth.Email(r), id)
		audit.Record(AuditEvent{Type: "admin", Email: auth.Email(r), Role: "admin", IP: clientIP(r),
			Detail: fmt.Sprintf("kicked connection id=%s", id)})
		w.WriteHeader(http.StatusNoContent)
	})))
}

// AdminSnapshot bundles the data sources the admin panel displays in
// one round-trip.
type AdminSnapshot struct {
	Version   string                `json:"version"`
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
		Version: version,
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
