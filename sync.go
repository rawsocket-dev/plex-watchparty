package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is the single source of truth for the watch party. Position is only
// authoritative at UpdatedAtMs; while Playing, clients extrapolate from there.
type State struct {
	RatingKey   string  `json:"ratingKey"`
	Title       string  `json:"title"`
	Playing     bool    `json:"playing"`
	PositionSec float64 `json:"positionSec"`
	// DurationSec is the source-of-truth length of the movie, sourced
	// from Plex metadata at load time. We broadcast it so the player
	// can drive its scrub bar without depending on v.duration (which
	// is Infinity with an event-type HLS playlist).
	DurationSec float64 `json:"durationSec"`
	UpdatedAtMs int64   `json:"updatedAtMs"`
	// SessionToken bumps every time the Plex transcode session restarts
	// (load action, or forward-seek-past-edge). Clients compare against
	// the last token they've seen — if changed, they destroy their
	// hls.js instance and attach a fresh one with the new playlist URL.
	SessionToken int64 `json:"sessionToken"`
	// CachedRanges is the union of all currently-cached segment time
	// ranges for the active movie, in seconds. The scrub bar renders
	// these as the "lighter band" so users can see which scrub targets
	// are instant vs which will trigger a Plex restart.
	CachedRanges [][2]float64 `json:"cachedRanges,omitempty"`
	// Viewers is the current connected-viewer list (name + role).
	// Refreshed on every broadcast so the player's "who's here" tooltip
	// can update as people join / leave.
	Viewers []ViewerInfo `json:"viewers,omitempty"`
	// ActiveHostName is the display name of the single user currently
	// elected to drive playback. Empty string means no one is driving.
	ActiveHostName string `json:"activeHostName,omitempty"`
	// Resume is the persisted "last known" playback state, surfaced
	// only when there is NO active session (RatingKey == ""). The
	// waiting room and library render it as a "Resume where you left
	// off?" affordance after a container restart or idle shutdown.
	// Populated by snapshot() / broadcast() from Hub.lastKnown.
	Resume *ResumeHint `json:"resume,omitempty"`
}

// ViewerInfo is the per-connection identity surfaced to clients in
// the SSE state stream. Name comes from the wp_name cookie set at
// login; Host mirrors the connection's effective role.
// ID is the per-connection handle used by the hand-off picker so
// the active host can pass control to a specific viewer.
type ViewerInfo struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	Host bool   `json:"host"` // true only for the single ACTIVE host, not host-eligibility
}

// clientEntry is the per-connection record we keep on Hub.clients.
// The channel carries pre-marshaled JSON so broadcast can serialize
// State once and fan it out byte-identical to every connected viewer
// — at 8 viewers that's ~7 fewer json.Marshal calls per tick.
//
// id is the short random handle the admin panel uses to reference a
// specific connection (kick). ip / connectedAt are stamped at join
// time for the admin roster API. kill is the per-entry abort
// channel; closing it unblocks the HandleEvents select loop and
// terminates that one connection without touching the others.
type clientEntry struct {
	id          string
	email       string
	host        bool
	name        string
	ip          string
	connectedAt time.Time
	send        chan []byte
	kill        chan struct{}

	// Heartbeat state — last position + playing flag the client
	// reported via /api/heartbeat. Updated under Hub.mu so admin
	// roster reads are race-free. lastHeartbeatAt is unix-nano; zero
	// means "we've never heard from this client" (browser hasn't
	// landed the heartbeat post yet).
	lastPosSec      float64
	lastPaused      bool
	lastHeartbeatAt int64
}

// AdminViewer is the per-connection record surfaced to the admin
// panel. Includes the kick handle + the IP + when the connection
// joined — fields we deliberately don't put on the public ViewerInfo.
// Kbps is each viewer's current rolling-window throughput, joined in
// by IP from bwTracker so the panel can show "who's buffering."
type AdminViewer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host bool   `json:"host"`
	IP   string `json:"ip"`
	// Email is the verified identity for this person — admin-only data
	// (admins already see emails in the audit log), used by the Viewers-
	// row "set alias" shortcut. "" for connections with no verified email.
	Email string `json:"email"`
	// Conns is how many live SSE connections this person holds (tabs,
	// reloads, or proxy-held ghosts). The roster shows one row per
	// identity, not per socket.
	Conns        int   `json:"conns"`
	ConnectedSec int64 `json:"connectedSec"`
	Kbps         int64 `json:"kbps"`
	// Heartbeat fields — what the viewer's player reported on its
	// last /api/heartbeat. HeartbeatAgeSec is -1 if no heartbeat has
	// arrived yet (fresh tab, no js running, etc).
	PosSec          float64 `json:"posSec"`
	Paused          bool    `json:"paused"`
	HeartbeatAgeSec float64 `json:"heartbeatAgeSec"`
	// IsActiveHost is true when this connection's email matches the
	// current active host. Displayed in the admin roster as a marker.
	IsActiveHost bool `json:"isActiveHost"`
}

type Hub struct {
	plex    *Plex
	session *PlexSession
	cache   *SegmentCache
	recent  *RecentMovies
	store   *StateStore
	audit   *AuditLog

	// aliases maps a verified email to an admin-assigned display name
	// that overrides the viewer's Google-profile name in every roster.
	// Resolved at roster-build time via displayName. Own lock; never
	// calls back into Hub (preserves Hub.mu → AliasStore.mu ordering).
	aliases *AliasStore

	// hostStore persists the active host email across restarts; broadcast
	// writes it on change, NewHub loads it on boot. lastSavedHost is the
	// last value written, so broadcast only persists on an actual change.
	hostStore     *HostStore
	lastSavedHost string

	// activeHost is the email of the single user currently allowed to
	// drive playback. "" means no one. Recomputed by electHostLocked on
	// every client-count change; set directly (to any connected user) by
	// admin override / hand-off. Persisted via hostStore.
	activeHost string

	mu    sync.Mutex
	state State
	// lastKnown is the persisted resume hint. Populated from disk at
	// startup; refreshed on every broadcast that has a live RatingKey
	// (so it's always current). Surfaced as State.Resume whenever
	// h.state.RatingKey is empty. Cleared by SendEveryoneToLobby.
	lastKnown *ResumeHint
	// clients keyed by the per-connection send channel so disconnect
	// lookups stay O(1). Role tracking pauses the room when the last
	// host leaves; name tracking populates the "who's here" roster.
	clients map[*clientEntry]struct{}

	// broadcastWake fires whenever the broadcast loop should re-tick:
	// first viewer joins, state changes, idle threshold crossed. The
	// loop pauses (blocks on this channel) when there are no clients
	// so an empty room doesn't burn cycles broadcasting to nobody.
	broadcastWake chan struct{}

	// Idle shutdown: when the last SSE viewer disconnects, start a
	// grace timer; if no one rejoins by the time it fires, stop plex session
	// so we don't keep transcoding for nobody.
	idleTimer *time.Timer

	// Host-exit pause: when the last host disconnects mid-playback,
	// start a short grace timer. If no host returns by then, broadcast
	// pause so viewers stop where the host did. Reconnects during the
	// grace window (page refresh, network blip) cancel the pause.
	hostExitTimer *time.Timer

	// Host-reassign grace: when the ACTIVE host's connection drops, we
	// hold their slot for hostExitGrace before passing the remote to
	// another eligible user. Without this, a brief blip (tab switch,
	// bfcache pagehide, navigation, network hiccup) silently hands
	// control to a second eligible viewer who never asked for it — and
	// the original host never gets it back. Reconnecting within the
	// window cancels the timer and keeps them driving.
	hostReassignTimer *time.Timer

	// Shutdown plumbing. done is closed once (guarded by closeOnce) to
	// signal broadcastLoop + timelineReportLoop to exit; loopsWG lets
	// Close wait for them to return. See Close.
	done      chan struct{}
	closeOnce sync.Once
	loopsWG   sync.WaitGroup
}

// idleGrace is how long we keep the plex session alive after the last viewer
// leaves. Long enough to forgive a tab refresh or short reconnect,
// short enough that a forgotten session doesn't burn CPU all night.
const idleGrace = 60 * time.Second

// hostExitGrace forgives a quick host reconnect (refresh, brief network
// blip). Shorter than idleGrace because the cost of a wrong pause is
// just "host clicks play again," not "plex session killed."
const hostExitGrace = 10 * time.Second

// sseWriteTimeout bounds a single SSE write. Healthy clients drain the
// socket instantly so this never bites; a dead/wedged connection trips it
// once its send buffer fills, letting us reap the ghost. Generous so a
// merely slow client (bad mobile link) isn't dropped mid-stream.
const sseWriteTimeout = 30 * time.Second

func NewHub(plex *Plex, session *PlexSession, cache *SegmentCache, recent *RecentMovies, store *StateStore, hostStore *HostStore, audit *AuditLog, aliases *AliasStore) *Hub {
	h := &Hub{
		plex:          plex,
		session:       session,
		cache:         cache,
		recent:        recent,
		store:         store,
		hostStore:     hostStore,
		audit:         audit,
		aliases:       aliases,
		clients:       make(map[*clientEntry]struct{}),
		broadcastWake: make(chan struct{}, 1),
		done:          make(chan struct{}),
	}
	if hostStore != nil {
		// Restore the active host from the prior process. They reclaim it
		// the moment their browser reconnects (reconcileHostLocked keeps a
		// connected active host); if they never return, the reassign grace
		// hands it on. lastSavedHost is primed so we don't immediately
		// re-persist the value we just read.
		if host := hostStore.Load(); host != "" {
			h.activeHost = host
			h.lastSavedHost = host
			log.Printf("host: restored active host %q from disk", host)
		}
	}
	if store != nil {
		// Resume hint from prior process. Snapshot includes it in
		// every State broadcast until a fresh load overwrites it.
		if hint := store.Load(); hint != nil {
			h.lastKnown = hint
			log.Printf("state: loaded resume hint %q @ %.2fs (saved %s ago)",
				hint.Title, hint.PositionSec,
				time.Since(time.Unix(hint.SavedAtUnix, 0)).Round(time.Second))
		}
	}
	// Periodic state re-broadcast. Two goals: (1) Safari's EventSource
	// closes the SSE if it goes ~10–20 s without an actual data event
	// (comment-only heartbeats don't count), and (2) every viewer
	// gets a fresh extrapolated position on a steady cadence so the
	// drift correction in the player has up-to-date state even
	// between play/pause/seek changes.
	h.loopsWG.Add(2)
	go h.broadcastLoop()
	// Plex timeline reporting. Makes us look like a normal Plex
	// client (web app, iOS, etc.) by POSTing our position + state
	// every ~5s. Plex uses these reports to transcode ahead of the
	// playhead and not evict nearby segments — without them we get
	// 404s on segments hls.js races ahead to ask for.
	go h.timelineReportLoop()
	return h
}

// Close stops the Hub's background loops and one-shot timers and drains
// any in-flight state persistence. Idempotent and safe to call from any
// goroutine.
//
// Ordering respects the documented lock hierarchy (Hub.mu →
// PlexSession.mu / SegmentCache.mu, which never call back into Hub): it
// closes the done channel and waits on the loop WaitGroup with no lock
// held — so a loop mid-Hub.mu-section can finish and exit without
// deadlocking — then takes Hub.mu only to stop the timers (no nested
// locks), then drains the store.
//
// Close is a barrier for the long-lived loops, not for every goroutine
// the Hub ever spawned: an AfterFunc timer callback that has already
// fired (and may be blocked on Hub.mu) can still complete after Close
// returns, and one-shot reportTimelineNow goroutines are not awaited.
// Both only touch state under Hub.mu or the network — neither writes
// the store — so they can't outlive-write the on-disk state dir.
func (h *Hub) Close() {
	h.closeOnce.Do(func() { close(h.done) })
	h.loopsWG.Wait()
	h.mu.Lock()
	if h.idleTimer != nil {
		h.idleTimer.Stop()
		h.idleTimer = nil
	}
	if h.hostExitTimer != nil {
		h.hostExitTimer.Stop()
		h.hostExitTimer = nil
	}
	if h.hostReassignTimer != nil {
		h.hostReassignTimer.Stop()
		h.hostReassignTimer = nil
	}
	h.mu.Unlock()
	if h.store != nil {
		h.store.Wait()
	}
	if h.hostStore != nil {
		h.hostStore.Wait()
	}
}

func (h *Hub) broadcastLoop() {
	defer h.loopsWG.Done()
	for {
		h.mu.Lock()
		hasClients := len(h.clients) > 0
		h.mu.Unlock()
		if !hasClients {
			// Empty room — block until someone joins (or shutdown). No
			// point re-broadcasting state to nobody every 3s.
			select {
			case <-h.broadcastWake:
				continue
			case <-h.done:
				return
			}
		}
		select {
		case <-time.After(3 * time.Second):
		case <-h.broadcastWake:
		case <-h.done:
			return
		}
		h.mu.Lock()
		// Auto-pause at the end of the movie. Without this, a host
		// who leaves a tab open in 'playing' state past the credits
		// has the server extrapolating forever — overnight idles
		// have produced PositionSec values in the tens of thousands
		// of seconds, then the player can't seek there because it's
		// outside the HLS playlist. Bake the clamp into h.state
		// (not just snapshot()) so subsequent /control actions see
		// the corrected position.
		if h.state.Playing && h.state.DurationSec > 0 {
			extrapolated := h.state.PositionSec + float64(nowMs()-h.state.UpdatedAtMs)/1000.0
			if extrapolated >= h.state.DurationSec {
				log.Printf("auto-pause: %q reached end at %.2fs (was %.2fs, dur %.2fs)",
					h.state.Title, h.state.DurationSec, h.state.PositionSec, h.state.DurationSec)
				h.state.PositionSec = h.state.DurationSec
				h.state.Playing = false
				h.state.UpdatedAtMs = nowMs()
			}
		}
		if len(h.clients) > 0 {
			h.broadcast()
		}
		h.mu.Unlock()
	}
}

// timelineReportInterval matches the cadence Plex's official web /
// mobile clients use. Faster than this is noisy on the Plex side;
// slower invites stale-segment eviction.
const timelineReportInterval = 5 * time.Second

// timelineReportLoop POSTs /:/timeline every timelineReportInterval
// while a Plex session is active. Snapshot extrapolates the position
// so each report carries the actual playhead, not a stale value
// from the last play/pause.
func (h *Hub) timelineReportLoop() {
	defer h.loopsWG.Done()
	t := time.NewTicker(timelineReportInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			h.reportTimelineNow()
		case <-h.done:
			return
		}
	}
}

// reportTimelineNow snapshots state and fires off a single timeline
// report. Called from the periodic ticker AND from state-changing
// /control actions (load, play, pause, seek) so Plex learns about
// changes promptly instead of waiting up to 5 seconds for the next
// tick. Safe to call when no session is active — ReportTimeline
// is a no-op in that case.
func (h *Hub) reportTimelineNow() {
	h.mu.Lock()
	if h.state.RatingKey == "" {
		h.mu.Unlock()
		return
	}
	s := h.snapshot()
	h.mu.Unlock()
	plexState := "paused"
	if s.Playing {
		plexState = "playing"
	}
	if err := h.session.ReportTimeline(
		plexState,
		s.RatingKey,
		int64(s.PositionSec*1000),
		int64(s.DurationSec*1000),
	); err != nil {
		log.Printf("timeline: report failed: %v", err)
	}
}

// wakeBroadcast is a non-blocking nudge to the broadcast loop. Used
// when a client joins (so the loop unblocks from its idle wait) and
// when state changes between ticks. The single-slot buffered channel
// coalesces — if multiple wakes arrive before the loop reads, only
// one fires.
func (h *Hub) wakeBroadcast() {
	select {
	case h.broadcastWake <- struct{}{}:
	default:
	}
}

// Lock ordering — touching this invariant breaks the room.
//
// The three mutexes in play are Hub.mu, PlexSession.mu, and
// SegmentCache.mu. They MUST be acquired in that order if any two
// are held simultaneously:
//
//	Hub.mu → PlexSession.mu (HandleControl seek/restart, idleShutdown)
//	Hub.mu → SegmentCache.mu (broadcast → cache.RangesFor)
//
// Neither PlexSession nor SegmentCache ever calls back into Hub, so
// the reverse direction is impossible by construction. The cache
// methods also never call into PlexSession and vice versa, so those
// two are never held together. If any future refactor introduces a
// cache or session callback that needs hub state, take it OUT of
// the lock and pass values, don't add an acquire.

// hostCount returns the number of currently-connected hosts. Must be
// called with h.mu held.
func (h *Hub) hostCount() int {
	n := 0
	for c := range h.clients {
		if c.host {
			n++
		}
	}
	return n
}

func (h *Hub) hasConnectionLocked(email string) bool {
	if email == "" {
		return false
	}
	for c := range h.clients {
		if c.email == email {
			return true
		}
	}
	return false
}

// electHostLocked recomputes the active host. Keeps the current one if
// still connected (even an admin-promoted, non-eligible user); otherwise
// elects a random host-ELIGIBLE connected user, or "" if none.
//
// Returns a non-empty audit detail string when the active host CHANGED,
// so the caller can Record it AFTER releasing h.mu (auditing does a file
// write — never under the room lock). Caller holds h.mu.
func (h *Hub) electHostLocked() string {
	prev := h.activeHost
	if h.activeHost != "" && h.hasConnectionLocked(h.activeHost) {
		return ""
	}
	seen := map[string]bool{}
	var candidates []string
	for c := range h.clients {
		if c.host && c.email != "" && !seen[c.email] {
			seen[c.email] = true
			candidates = append(candidates, c.email)
		}
	}
	if len(candidates) == 0 {
		h.activeHost = ""
	} else {
		h.activeHost = candidates[rand.Intn(len(candidates))]
	}
	if h.activeHost == prev {
		return ""
	}
	if h.activeHost == "" {
		return "no active host (none eligible connected)"
	}
	return fmt.Sprintf("active host is now %q", h.nameForEmailLocked(h.activeHost))
}

// displayName resolves the name shown for a connection: an admin-set
// alias for the connection's email wins over the Google-profile name in
// c.name. Empty-email connections (no identity) have no alias and keep
// c.name. Callers hold h.mu; AliasStore takes its own lock, preserving
// the Hub.mu → AliasStore.mu order (AliasStore never calls into Hub).
func (h *Hub) displayName(c *clientEntry) string {
	if h.aliases != nil && c.email != "" {
		if a := h.aliases.Get(c.email); a != "" {
			return a
		}
	}
	return c.name
}

func (h *Hub) nameForEmailLocked(email string) string {
	for c := range h.clients {
		if c.email == email {
			if n := h.displayName(c); n != "" {
				return n
			}
		}
	}
	return email
}

func (h *Hub) activeHostNameLocked() string {
	if h.activeHost == "" {
		return ""
	}
	for c := range h.clients {
		if c.email == h.activeHost {
			if n := h.displayName(c); n != "" {
				return n
			}
		}
	}
	return "" // no named connection for the active host — don't leak an email
}

// viewerList returns the current connected-viewer roster sorted host-
// first then alphabetical. Must be called with h.mu held.
func (h *Hub) viewerList() []ViewerInfo {
	// One entry per PERSON (identity = email), so a user holding several
	// connections (tabs / reloads / proxy-held ghosts) shows once and the
	// viewer COUNT reflects people, not sockets. Empty-email connections
	// stay separate. Host marks the single ACTIVE host (who's driving), NOT
	// mere host-eligibility. The representative id is one of the person's
	// connections — hand-off resolves it back to the email, so any works.
	byKey := make(map[string]*ViewerInfo)
	var order []string
	for c := range h.clients {
		key := c.email
		if key == "" {
			key = "conn:" + c.id
		}
		isActive := c.email != "" && c.email == h.activeHost
		if v, ok := byKey[key]; ok {
			if isActive { // prefer the active host's own connection as representative
				v.ID, v.Host = c.id, true
			}
			continue
		}
		byKey[key] = &ViewerInfo{ID: c.id, Name: h.displayName(c), Host: isActive}
		order = append(order, key)
	}
	out := make([]ViewerInfo, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// onClientCountChange must be called with h.mu held whenever a client
// is added to or removed from h.clients. Manages two timers:
//   - idle shutdown: plex session stops when ALL viewers leave
//   - host-exit pause: room pauses when the last HOST leaves mid-playback
//
// Returns a non-empty audit detail string when the active host changed,
// so the caller can Record it AFTER releasing h.mu.
func (h *Hub) onClientCountChange() string {
	// --- idle shutdown (any-viewer) ---
	if len(h.clients) > 0 {
		if h.idleTimer != nil {
			h.idleTimer.Stop()
			h.idleTimer = nil
		}
	} else if h.state.RatingKey != "" && h.idleTimer == nil {
		rk := h.state.RatingKey
		h.idleTimer = time.AfterFunc(idleGrace, func() { h.idleShutdown(rk) })
		log.Printf("idle: no viewers, will stop plex session in %s if nobody returns", idleGrace)
	}

	// --- single active host: keep while connected, hold the slot through
	// a brief disconnect, only elect a replacement when truly host-less ---
	hostAudit := h.reconcileHostLocked()

	// --- pause when nobody is driving ---
	h.maybeArmHostExitPauseLocked()
	return hostAudit
}

// reconcileHostLocked keeps the active host while they're connected, holds
// their slot for hostExitGrace on a brief disconnect (so a tab switch /
// bfcache / network blip doesn't hand the remote to another eligible
// viewer), and elects a fresh host only when there genuinely isn't one.
// Returns an audit detail when the active host changes. Caller holds h.mu.
func (h *Hub) reconcileHostLocked() string {
	// Active host still connected → they keep it; cancel any pending
	// reassignment (they're here, or just reconnected).
	if h.activeHost != "" && h.hasConnectionLocked(h.activeHost) {
		if h.hostReassignTimer != nil {
			h.hostReassignTimer.Stop()
			h.hostReassignTimer = nil
		}
		return ""
	}
	// Active host set but momentarily gone → hold their slot for the grace
	// window before passing control on. Nobody else drives meanwhile
	// (IsActiveHost stays false for everyone but the absent host).
	if h.activeHost != "" {
		if h.hostReassignTimer == nil {
			gone := h.activeHost
			h.hostReassignTimer = time.AfterFunc(hostExitGrace, func() { h.reassignHostAfterGrace(gone) })
			log.Printf("host: active host %q disconnected; holding the remote for %s", gone, hostExitGrace)
		}
		return ""
	}
	// Genuinely no active host → elect one now from connected eligible.
	return h.electHostLocked()
}

// reassignHostAfterGrace fires when a disconnected active host hasn't
// returned within the grace window. If they're still gone, the remote
// passes to a random remaining eligible viewer (or to nobody, which arms
// the host-exit pause). Acquires h.mu itself.
func (h *Hub) reassignHostAfterGrace(gone string) {
	h.mu.Lock()
	h.hostReassignTimer = nil
	// Bail if the host returned, or control already moved (handoff/admin).
	if h.activeHost != gone || h.hasConnectionLocked(h.activeHost) {
		h.mu.Unlock()
		return
	}
	h.activeHost = "" // clear so electHostLocked picks a fresh eligible
	detail := h.electHostLocked()
	h.maybeArmHostExitPauseLocked()
	h.broadcast()
	h.mu.Unlock()
	h.wakeBroadcast()
	if detail != "" {
		h.audit.Record(AuditEvent{Type: "host", Email: "system", Detail: detail})
	}
}

// maybeArmHostExitPauseLocked pauses the room (after a grace window) when
// no one is driving mid-playback; a live active host cancels any pending
// pause. Caller holds h.mu.
func (h *Hub) maybeArmHostExitPauseLocked() {
	if h.activeHost != "" {
		if h.hostExitTimer != nil {
			h.hostExitTimer.Stop()
			h.hostExitTimer = nil
		}
		return
	}
	if !h.state.Playing || h.state.RatingKey == "" {
		return
	}
	if h.hostExitTimer != nil {
		return
	}
	rk := h.state.RatingKey
	h.hostExitTimer = time.AfterFunc(hostExitGrace, func() { h.hostExitPause(rk) })
	log.Printf("host-exit: no active host, will pause in %s if none returns", hostExitGrace)
}

// hostExitPause fires when the host-exit grace timer expires. Pauses
// the room so viewers don't keep watching without the host.
func (h *Hub) hostExitPause(forRatingKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hostExitTimer = nil
	if h.activeHost != "" {
		return // a host took over
	}
	if h.state.RatingKey != forRatingKey || !h.state.Playing {
		return // session changed or already paused
	}
	cur := h.snapshot() // capture extrapolated position before pausing
	cur.Playing = false
	cur.UpdatedAtMs = nowMs()
	h.state = cur
	log.Printf("host-exit: pausing %q at %.2fs (no host returned in %s)",
		h.state.Title, h.state.PositionSec, hostExitGrace)
	h.broadcast()
}

// idleShutdown fires when the grace timer expires. Guards against the
// race where a viewer rejoins during the grace window.
func (h *Hub) idleShutdown(forRatingKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.idleTimer = nil
	if len(h.clients) > 0 {
		return // someone rejoined
	}
	if h.state.RatingKey != forRatingKey {
		return // session changed during the grace window
	}
	if h.state.RatingKey == "" {
		return // already cleared
	}
	log.Printf("idle: stopping plex session %q after %s with no viewers",
		h.state.Title, idleGrace)
	// Recording (a file write) under Hub.mu is normally avoided, but it's
	// safe here: the len(h.clients) guard above means there are zero
	// viewers, so there's no SSE writer to stall. Don't copy this pattern
	// to a path that can run with viewers connected.
	h.audit.Record(AuditEvent{Type: "plex", Email: "system", Detail: fmt.Sprintf("idle: stopped %q after %s with no viewers", h.state.Title, idleGrace)})
	h.session.Stop()
	h.state = State{UpdatedAtMs: nowMs()}
	h.broadcast()
}

func nowMs() int64 { return time.Now().UnixMilli() }

// nameCookie is the cookie that carries the viewer's chosen display
// name. Separate from sessionCookie so the name can be JS-readable
// without exposing the auth credential.
const nameCookie = "wp_name"

// newNameCookie builds the display-name cookie. It's HttpOnly (no client
// JS reads wp_name — the name is set from the verified Google profile and
// read back server-side) and Secure over HTTPS. The value is still
// re-sanitized on every read, so the flags are hardening, not the boundary.
func newNameCookie(name string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     nameCookie,
		Value:    url.QueryEscape(name),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(365 * 24 * time.Hour),
	}
}

// maxViewerName is the trim length applied to viewer names. Short
// enough to keep the top-bar tooltip readable, long enough for real
// nicknames.
const maxViewerName = 16

// sanitizeName trims, drops anything outside printable ASCII (so
// emoji, control chars, and non-Latin scripts don't sneak in), and
// caps the result at maxViewerName runes. Returns "" if nothing
// usable remains.
func sanitizeName(s string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 0x20 && r <= 0x7e {
			return r
		}
		return -1
	}, s)
	clean = strings.TrimSpace(clean)
	if len(clean) > maxViewerName {
		clean = clean[:maxViewerName]
	}
	return clean
}

// viewerNameFromRequest pulls the display name out of the wp_name
// cookie, percent-decodes it, then runs it through sanitizeName.
// Empty or missing cookies fall back to "guest" so the UI always has
// something to render.
func viewerNameFromRequest(r *http.Request) string {
	c, err := r.Cookie(nameCookie)
	if err != nil {
		return "guest"
	}
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		v = c.Value
	}
	if clean := sanitizeName(v); clean != "" {
		return clean
	}
	return "guest"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fmtDurationMs(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	d := time.Duration(ms) * time.Millisecond
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func (h *Hub) snapshot() State {
	s := h.state
	if s.Playing {
		s.PositionSec += float64(nowMs()-s.UpdatedAtMs) / 1000.0
		s.UpdatedAtMs = nowMs()
		// Clamp at the end of the movie. Without this, a host who
		// leaves a tab open in 'playing' state past the credits keeps
		// the server extrapolating forever — we've seen positions in
		// the tens of thousands of seconds after an overnight idle.
		// Subsequent /control actions then record that nonsense and
		// the player can't seek to a position outside the playlist.
		if s.DurationSec > 0 && s.PositionSec > s.DurationSec {
			s.PositionSec = s.DurationSec
		}
	}
	return s
}

// RestartAtCurrentPosition restarts the active Plex session at the
// host's current play position. Called by the segment proxy's auto-
// restart goroutine (after N consecutive Plex 4xx/5xx fetches) and
// by the admin "Restart Plex session" button. Reuses the same
// Hub.mu → release → session.RestartFor → Hub.mu dance as the seek-
// with-restart path; bumps SessionToken so connected clients destroy
// their hls.js and reattach with the freshly-rewritten playlist.
//
// reason classifies the call site for the admin lifecycle counters.
func (h *Hub) RestartAtCurrentPosition(reason RestartReason) error {
	h.mu.Lock()
	if h.state.RatingKey == "" {
		h.mu.Unlock()
		return fmt.Errorf("no active session")
	}
	cur := h.snapshot()
	h.mu.Unlock()
	log.Printf("restart: %v — restarting Plex at %.2fs", reason, cur.PositionSec)
	if err := h.session.RestartFor(reason, cur.PositionSec); err != nil {
		return fmt.Errorf("plex Restart: %w", err)
	}
	h.mu.Lock()
	h.state.SessionToken = h.session.SessionToken()
	h.state.UpdatedAtMs = nowMs()
	h.broadcast()
	h.mu.Unlock()
	go h.reportTimelineNow()
	return nil
}

// RecoverSegmentForRange asks the session to substitute a segment
// covering the requested movie-time window after Plex 404'd the
// original. Internally the session may restart Plex at that offset
// (bumping SessionToken) — on success we stamp the new token onto
// h.state and broadcast so connected clients reattach with the
// fresh playlist for SUBSEQUENT segments. The bytes returned here
// satisfy the in-flight segment request without the client ever
// seeing the failure.
func (h *Hub) RecoverSegmentForRange(startMs, endMs int64) ([]byte, error) {
	data, err := h.session.RecoverSegmentBytes(startMs, endMs)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.state.SessionToken = h.session.SessionToken()
	h.state.UpdatedAtMs = nowMs()
	h.broadcast()
	h.mu.Unlock()
	go h.reportTimelineNow()
	return data, nil
}

// Snapshot is the locked, public counterpart of snapshot(). Used by
// HTTP handlers that need a one-shot view of current state (e.g. the
// /api/state endpoint that drives the library's "Resume?" prompt).
// Includes the current viewer roster + Resume hint so callers don't
// see a stale list between broadcast ticks.
func (h *Hub) Snapshot() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.snapshot()
	s.Viewers = h.viewerList()
	s.ActiveHostName = h.activeHostNameLocked()
	if s.RatingKey == "" && h.lastKnown != nil {
		s.Resume = h.lastKnown
	}
	return s
}

// IsActiveHost reports whether email currently holds the controls.
func (h *Hub) IsActiveHost(email string) bool {
	if email == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.activeHost == email
}

// broadcast marshals the current snapshot once and fans the bytes out
// to every connected viewer. Stays out of h.state (CachedRanges,
// Viewers, Resume are computed per-broadcast and stamped onto the
// snapshot only — the authoritative h.state is unchanged).
//
// Side effect: persists the resume hint whenever there's an active
// RatingKey. The persist is fire-and-forget on a goroutine so the
// broadcast doesn't block on a slow filesystem.
func (h *Hub) broadcast() {
	s := h.snapshot()
	if s.RatingKey != "" {
		if h.cache != nil {
			s.CachedRanges = h.cache.RangesFor(s.RatingKey)
		}
		// Refresh the in-memory hint AND persist. snapshot() already
		// extrapolated PositionSec, so a crash here loses at most
		// the last broadcast tick (~3 s) of advance.
		hint := ResumeHint{
			RatingKey:   s.RatingKey,
			Title:       s.Title,
			PositionSec: s.PositionSec,
			DurationSec: s.DurationSec,
		}
		h.lastKnown = &hint
		if h.store != nil {
			h.store.SaveAsync(hint)
		}
	} else if h.lastKnown != nil {
		// No active session — surface the resume hint so waiting
		// room / library can offer "Resume where you left off?"
		s.Resume = h.lastKnown
	}
	s.Viewers = h.viewerList()
	s.ActiveHostName = h.activeHostNameLocked()
	// Persist the active host across restarts, but only when it actually
	// changes (election / hand-off / admin), off the room lock.
	if h.hostStore != nil && h.activeHost != h.lastSavedHost {
		h.lastSavedHost = h.activeHost
		h.hostStore.SaveAsync(h.activeHost)
	}
	b, err := json.Marshal(s)
	if err != nil {
		log.Printf("broadcast: marshal: %v", err)
		return
	}
	for c := range h.clients {
		select {
		case c.send <- b:
		default: // drop for slow clients; next event re-syncs them
		}
	}
}

// HandleEvents is the SSE stream: initial state on connect, then every change,
// plus a heartbeat so proxies don't kill idle connections.
// isHost is the connection's effective role — needed so the hub can pause
// the room when the last host leaves. email is the verified identity of the
// connecting user, used for active-host election.
func (h *Hub) HandleEvents(w http.ResponseWriter, r *http.Request, isHost bool, email string) {
	name := viewerNameFromRequest(r)
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default and that breaks SSE
	// (events get held back instead of flushed event-by-event). This
	// header tells nginx + GCP HTTPS LB to bypass response buffering
	// for this stream without requiring per-location config on the
	// proxy side. Other proxies (Caddy, Traefik) ignore it harmlessly.
	w.Header().Set("X-Accel-Buffering", "no")

	ip := clientIP(r)
	connectedAt := time.Now()
	entry := &clientEntry{
		id:          randomHex(8),
		email:       email,
		host:        isHost,
		name:        name,
		ip:          ip,
		connectedAt: connectedAt,
		send:        make(chan []byte, 8),
		kill:        make(chan struct{}),
	}
	h.mu.Lock()
	wasEmpty := len(h.clients) == 0
	h.clients[entry] = struct{}{}
	n := len(h.clients)
	hosts := h.hostCount()
	hostAudit := h.onClientCountChange()
	if wasEmpty {
		// First viewer in an idle room — unblock the broadcast loop.
		h.wakeBroadcast()
	}
	init := h.snapshot()
	if init.RatingKey != "" && h.cache != nil {
		init.CachedRanges = h.cache.RangesFor(init.RatingKey)
	} else if init.RatingKey == "" && h.lastKnown != nil {
		init.Resume = h.lastKnown
	}
	// snapshot() doesn't refresh Viewers — that only happens in
	// broadcast(). For a freshly-connected client this matters:
	// without this, the new viewer would see a stale (or empty)
	// roster on join and have to wait up to 3 s for the next
	// broadcastLoop tick. Stamp the current roster onto the init.
	init.Viewers = h.viewerList()
	init.ActiveHostName = h.activeHostNameLocked()
	initBytes, _ := json.Marshal(init)
	h.mu.Unlock()
	if hostAudit != "" {
		h.audit.Record(AuditEvent{Type: "host", Email: "system", Detail: hostAudit})
	}
	role := "viewer"
	if isHost {
		role = "host"
	}
	log.Printf("sse: connect ip=%s role=%s viewers=%d hosts=%d", ip, role, n, hosts)
	defer func() {
		h.mu.Lock()
		delete(h.clients, entry)
		left := len(h.clients)
		leftHosts := h.hostCount()
		hostAudit := h.onClientCountChange()
		h.mu.Unlock()
		if hostAudit != "" {
			h.audit.Record(AuditEvent{Type: "host", Email: "system", Detail: hostAudit})
		}
		log.Printf("sse: disconnect ip=%s role=%s viewers=%d hosts=%d after=%s",
			ip, role, left, leftHosts, time.Since(connectedAt).Round(time.Second))
	}()

	// Each write gets a deadline via the ResponseController. A healthy
	// client drains the socket immediately, so writes finish in
	// microseconds; a dead/wedged client (gone, but the TCP or proxy
	// connection lingers) blocks once the send buffer fills and trips the
	// deadline — we return, the defer reaps the entry, and it stops being
	// counted as a viewer. A write error (peer closed) does the same.
	rc := http.NewResponseController(w)
	write := func(b []byte) error {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
		if _, err := w.Write(b); err != nil {
			return err
		}
		return rc.Flush()
	}
	writeBytes := func(b []byte) error {
		if err := write([]byte("data: ")); err != nil {
			return err
		}
		if err := write(b); err != nil {
			return err
		}
		return write([]byte("\n\n"))
	}
	// Per-connection handshake: the client needs to know its own id
	// so it can stamp heartbeat POSTs with it. Sent only to this
	// connection (the rest of the room never sees a clientId field
	// in their state stream). The carry-along _clientId on the init
	// state body avoids a second framing protocol.
	hello, _ := json.Marshal(map[string]any{"clientId": entry.id})
	if writeBytes(hello) != nil || writeBytes(initBytes) != nil {
		return
	}

	// 5 s heartbeat: also doubles as our "is the client still there?"
	// probe — a failed/timed-out write returns and reaps the connection,
	// complementing Go's own r.Context() cancellation on a clean close.
	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-entry.kill:
			log.Printf("sse: kicked ip=%s role=%s after=%s",
				ip, role, time.Since(connectedAt).Round(time.Second))
			return
		case b := <-entry.send:
			if writeBytes(b) != nil {
				return
			}
		case <-heartbeat.C:
			if write([]byte(": ping\n\n")) != nil {
				return
			}
		}
	}
}

type controlReq struct {
	Action      string  `json:"action"` // load | play | pause | seek
	RatingKey   string  `json:"ratingKey"`
	PositionSec float64 `json:"positionSec"`
	// Restart, when set on a 'load', forces a fresh Plex Start even
	// when the requested movie is already the active session. Used by
	// the library's "Start over" prompt to discard the current
	// position and begin from offset 0.
	Restart bool `json:"restart"`
	// Autoplay, when set on a 'load', starts the room in Playing state
	// instead of paused. The resume banner uses it so resuming lands on
	// /watch already playing rather than requiring a separate Run press.
	Autoplay bool `json:"autoplay"`
}

// fmtClock renders a position in seconds as H:MM:SS (or M:SS under an
// hour) for human-readable audit detail strings.
func fmtClock(sec float64) string {
	if sec < 0 || !isFiniteF(sec) {
		sec = 0
	}
	s := int(sec)
	h, m, ss := s/3600, (s%3600)/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, ss)
	}
	return fmt.Sprintf("%d:%02d", m, ss)
}

func isFiniteF(f float64) bool { return f == f && f-f == 0 }

// HandleControl applies an action from any authenticated friend and rebroadcasts.
// clampSeekTarget rejects non-finite seek targets (ok=false → 400) and
// clamps valid ones to [0, durationSec]. A duration of 0 means "unknown"
// (metadata not loaded yet), so only the lower bound is enforced. Without
// this a host could seek to a negative or absurd position and push the
// room into a state the player can't represent or that triggers a pointless
// Plex restart at a nonsense offset.
func clampSeekTarget(target, durationSec float64) (float64, bool) {
	if math.IsNaN(target) || math.IsInf(target, 0) {
		return 0, false
	}
	if target < 0 {
		target = 0
	}
	if durationSec > 0 && target > durationSec {
		target = durationSec
	}
	return target, true
}

func (h *Hub) HandleControl(w http.ResponseWriter, r *http.Request) {
	// Cap the request body — /control is the host's command channel,
	// not a file upload. 4 KiB is generous for the JSON we accept.
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req controlReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("control: bad request ip=%s err=%v", clientIP(r), err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	log.Printf("control: %s ip=%s action=%s ratingKey=%s pos=%.2f",
		"received", clientIP(r), req.Action, req.RatingKey, req.PositionSec)

	// Attribute this action to the signed-in host (WithActor stashed the
	// email); fall back to "system" if absent. The deferred Record runs
	// AFTER the handler returns — i.e. after Hub.mu has been released —
	// so the audit file write never happens under the room lock. Each
	// action branch sets auditDetail; empty detail records nothing.
	// Role is recorded as "host" because the gate below rejects anyone
	// who isn't the active host, so a recorded play action is always the
	// active host's.
	actor := actorEmail(r)
	if actor == "" {
		actor = "system"
	}
	var auditDetail string
	defer func() {
		if auditDetail != "" {
			h.audit.Record(AuditEvent{Type: "play", Email: actor, Role: "host", IP: clientIP(r), Detail: auditDetail})
		}
	}()

	if !h.IsActiveHost(actor) {
		log.Printf("control: 403 not-active-host ip=%s actor=%q action=%s", clientIP(r), actor, req.Action)
		http.Error(w, "not the active host", http.StatusForbidden)
		return
	}

	if req.Action == "load" {
		t0 := time.Now()
		// If the same movie is already loaded and Plex still has a live
		// session for it, skip the Stop+Start dance entirely. Plex's
		// universal-transcoder /stop is unreliable: it often returns
		// 200 without actually freeing the slot, and the next /start
		// then 400s with a bare HTML body. Reusing the existing session
		// avoids that whole class of failure when the user clicks the
		// same movie twice (back + forward, refresh, etc.). The
		// library prompts "Resume / Start over" for this case; Start
		// over sends restart=true to force the full path.
		if h.session.RatingKey() == req.RatingKey && !req.Restart {
			log.Printf("load %q: same movie already loaded, reusing session", req.RatingKey)
			h.mu.Lock()
			cur := h.state
			h.broadcast()
			h.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"reused":      true,
				"durationSec": cur.DurationSec,
			})
			// Reuse (refresh / re-click of the current movie) is intentionally
			// not audited — it's not a new session start. auditDetail stays "".
			return
		}

		si, err := h.plex.Resolve(req.RatingKey)
		if err != nil {
			log.Printf("control: resolve failed ip=%s ratingKey=%s err=%v",
				clientIP(r), req.RatingKey, err)
			http.Error(w, "resolve: "+err.Error(), http.StatusBadGateway)
			return
		}
		resolveMs := time.Since(t0).Milliseconds()

		tList := time.Now()
		// Ensure the library cache is warm so MovieByKey can resolve
		// the title without a Plex round-trip. ListMovies is cheap on
		// a warm cache; on cold start it populates the index for us.
		_, _ = h.plex.ListMovies()
		title := req.RatingKey
		year := 0
		if m, ok := h.plex.MovieByKey(req.RatingKey); ok {
			title = m.Title
			year = m.Year
		}
		listMs := time.Since(tList).Milliseconds()

		tStart := time.Now()
		// Resume target: respect req.PositionSec when the caller sent
		// one (waiting-room "Resume?" button, library Resume modal).
		// Clamp to [0, duration-2s] so we don't try to start the
		// transcoder past the end of the file — Plex returns 4xx for
		// out-of-range offsets.
		offsetSec := req.PositionSec
		if offsetSec < 0 {
			offsetSec = 0
		}
		if dur := float64(si.Duration) / 1000.0; dur > 0 && offsetSec > dur-2 {
			offsetSec = 0
		}
		// A host-initiated load wins over any in-flight auto-restart —
		// signal the auto-restart goroutine to abort before we call
		// Start. Without this both could call /stop+/start in quick
		// succession and Plex would see overlapping sessions.
		h.session.SuppressAutoRestart()
		if err := h.session.Start(req.RatingKey, offsetSec); err != nil {
			log.Printf("control: plex Start failed ip=%s ratingKey=%s err=%v",
				clientIP(r), req.RatingKey, err)
			http.Error(w, "plex start: "+err.Error(), http.StatusBadGateway)
			return
		}
		startMs := time.Since(tStart).Milliseconds()
		totalMs := time.Since(t0).Milliseconds()

		log.Printf("load %q: resolve=%dms list=%dms plexStart=%dms total=%dms offset=%.2fs",
			title, resolveMs, listMs, startMs, totalMs, offsetSec)
		log.Printf("media %q: %s %s · %dx%d @ %s · %d kbps · %s",
			title, orDash(si.VideoCodec), orDash(si.VideoProfile),
			si.Width, si.Height, orDash(si.FrameRate), si.Bitrate,
			fmtDurationMs(si.Duration),
		)

		h.mu.Lock()
		h.state = State{
			RatingKey:    req.RatingKey,
			Title:        title,
			Playing:      req.Autoplay,
			PositionSec:  offsetSec,
			DurationSec:  float64(si.Duration) / 1000.0,
			SessionToken: h.session.SessionToken(),
			UpdatedAtMs:  nowMs(),
		}
		h.broadcast()
		h.mu.Unlock()
		auditDetail = fmt.Sprintf("started %q at %s", title, fmtClock(offsetSec))
		// Tell Plex about the new session immediately so it knows our
		// starting position. The 5s ticker would catch it eventually
		// but the first segments hls.js requests would race ahead of
		// Plex's transcoder otherwise.
		go h.reportTimelineNow()
		if h.recent != nil {
			h.recent.Touch(req.RatingKey, title, year)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int64{
			"plexResolveMs": resolveMs,
			"plexListMs":    listMs,
			"plexStartMs":   startMs,
			"totalMs":       totalMs,
		})
		return
	}

	if req.Action == "stop" {
		// Host requested an explicit end of the watch session. Tear
		// down the Plex transcoder, clear our state, and broadcast
		// the empty State so every connected client navigates to the
		// waiting room or the library. Used by the player's "←
		// library" link so navigating away kills playback instead of
		// leaving the transcoder running until the idle timer fires.
		log.Printf("control: stop ip=%s title=%q", clientIP(r), h.state.Title)
		// Send a final "stopped" timeline before tearing down the
		// session — after session.Stop() the sessionID is cleared and
		// ReportTimeline becomes a no-op.
		h.mu.Lock()
		rk := h.state.RatingKey
		stoppedTitle := h.state.Title
		posMs := int64(h.snapshot().PositionSec * 1000)
		durMs := int64(h.state.DurationSec * 1000)
		h.mu.Unlock()
		if rk != "" {
			auditDetail = fmt.Sprintf("stopped %q", stoppedTitle)
			if err := h.session.ReportTimeline("stopped", rk, posMs, durMs); err != nil {
				log.Printf("timeline: stop report failed: %v", err)
			}
		}
		h.session.Stop()
		h.mu.Lock()
		h.state = State{UpdatedAtMs: nowMs()}
		h.broadcast()
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if req.Action != "play" && req.Action != "pause" && req.Action != "seek" {
		log.Printf("control: unknown action ip=%s action=%q", clientIP(r), req.Action)
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.snapshot()
	// play / pause / seek only make sense against an active movie. (load
	// and stop are handled above and return before here.)
	if cur.RatingKey == "" {
		http.Error(w, "no active movie", http.StatusConflict)
		return
	}
	switch req.Action {
	case "play":
		cur.Playing = true
		log.Printf("state: play  ip=%s title=%q at=%.2f", clientIP(r), cur.Title, cur.PositionSec)
		auditDetail = fmt.Sprintf("resumed %q at %s", cur.Title, fmtClock(cur.PositionSec))
	case "pause":
		cur.Playing = false
		log.Printf("state: pause ip=%s title=%q at=%.2f", clientIP(r), cur.Title, cur.PositionSec)
		auditDetail = fmt.Sprintf("paused %q at %s", cur.Title, fmtClock(cur.PositionSec))
	case "seek":
		log.Printf("state: seek  ip=%s title=%q from=%.2f to=%.2f",
			clientIP(r), cur.Title, cur.PositionSec, req.PositionSec)
		// Reject non-finite targets and clamp to [0, duration] before we
		// decide on a restart, so a bad position can't drive Plex to a
		// nonsense offset or leave the room un-seekable.
		target, ok := clampSeekTarget(req.PositionSec, cur.DurationSec)
		if !ok {
			http.Error(w, "invalid seek position", http.StatusBadRequest)
			return
		}
		// Decide whether the seek target is reachable from cache alone
		// (no Plex restart) or whether we need to restart Plex at the
		// new offset. Cached ranges + the current session's edge define
		// what's instantly seekable.
		needRestart := target > h.session.EdgeSec()+0.5
		if needRestart {
			// Also check cache — backward seeks into a previously-watched
			// range (perhaps before a prior Restart) don't need a restart.
			for _, rng := range h.cache.RangesFor(cur.RatingKey) {
				if target >= rng[0] && target <= rng[1] {
					needRestart = false
					break
				}
			}
		}
		if needRestart {
			// We must drop h.mu before calling session.Restart — it
			// holds PlexSession.mu internally for the duration of the
			// HTTP round-trip to Plex (decision + start, ~hundreds of
			// ms). Holding Hub.mu through that would stall every
			// connected viewer's SSE writes. Re-snapshot state after
			// reacquiring since other handlers (play/pause echoes)
			// can have mutated h.state during the gap; the seek
			// target overrides whatever PositionSec they wrote, but
			// the rest of the state must be current.
			h.mu.Unlock()
			log.Printf("seek: restart needed (target=%.2f > edge=%.2f, no cache hit)",
				target, h.session.EdgeSec())
			// Host action wins over any in-flight auto-restart.
			h.session.SuppressAutoRestart()
			if err := h.session.RestartFor(RestartBySeek, target); err != nil {
				log.Printf("seek: plex Restart failed: %v", err)
				h.mu.Lock()
				http.Error(w, "plex restart: "+err.Error(), http.StatusBadGateway)
				return
			}
			h.mu.Lock()
			cur = h.snapshot()
			cur.SessionToken = h.session.SessionToken()
		}
		cur.PositionSec = target
		auditDetail = fmt.Sprintf("seeked %q to %s", cur.Title, fmtClock(target))
	}
	cur.UpdatedAtMs = nowMs()
	h.state = cur
	h.broadcast()
	// Promptly tell Plex about play/pause/seek so it can adjust its
	// transcode lookahead and segment retention without waiting for
	// the next 5s tick.
	go h.reportTimelineNow()
	w.WriteHeader(http.StatusNoContent)
}

// SendEveryoneToLobby tears down the active Plex session and clears
// hub state. Every connected /watch page sees the empty State on the
// next SSE tick and reloads to land on waiting.html. Same code path
// as a host clicking "← library", just admin-triggered.
func (h *Hub) SendEveryoneToLobby() {
	h.mu.Lock()
	rk := h.state.RatingKey
	posMs := int64(h.snapshot().PositionSec * 1000)
	durMs := int64(h.state.DurationSec * 1000)
	title := h.state.Title
	h.mu.Unlock()
	if rk != "" {
		if err := h.session.ReportTimeline("stopped", rk, posMs, durMs); err != nil {
			log.Printf("timeline: stop report failed: %v", err)
		}
	}
	h.session.Stop()
	h.mu.Lock()
	h.state = State{UpdatedAtMs: nowMs()}
	// Admin "Send to lobby" is the explicit "we're done with this
	// movie" signal — wipe the resume hint so the next viewer
	// landing on /watch isn't offered to pick it back up.
	h.lastKnown = nil
	if h.store != nil {
		h.store.Clear()
	}
	h.broadcast()
	h.mu.Unlock()
	log.Printf("admin lobby: tore down session %q", title)
}

// AdminRoster returns a snapshot of currently connected SSE clients
// with the fields the admin panel needs (id for kick, ip, name, role,
// connected-since seconds, current kbps). Sorted host-first then by
// IP for a stable display order. bw is used to join in each viewer's
// rolling-window throughput; pass nil if bandwidth is unavailable.
func (h *Hub) AdminRoster(bw *bwTracker) []AdminViewer {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	// One row per PERSON (identity = email), collapsing a user's multiple
	// connections — tabs, reloads, or proxy-held ghosts — into a single
	// entry with a connection count. The representative connection is the
	// active host's (if any), else the one with the freshest heartbeat (an
	// actually-live tab). Empty-email connections aren't merged.
	byKey := make(map[string]*AdminViewer)
	chosenActive := make(map[string]bool)
	bestFresh := make(map[string]float64)
	var order []string
	for c := range h.clients {
		key := c.email
		if key == "" {
			key = "conn:" + c.id
		}
		hbAge := -1.0
		if c.lastHeartbeatAt != 0 {
			hbAge = time.Since(time.Unix(0, c.lastHeartbeatAt)).Seconds()
		}
		av, ok := byKey[key]
		if !ok {
			av = &AdminViewer{}
			byKey[key] = av
			bestFresh[key] = math.Inf(1)
			order = append(order, key)
		}
		av.Conns++
		if cs := int64(now.Sub(c.connectedAt).Seconds()); cs > av.ConnectedSec {
			av.ConnectedSec = cs // show the longest-lived connection's uptime
		}
		isActive := c.email != "" && c.email == h.activeHost
		if isActive {
			av.IsActiveHost = true
		}
		// Pick the representative connection: active host wins outright;
		// otherwise the freshest heartbeat (never-heartbeated ranks last).
		fresh := hbAge
		if fresh < 0 {
			fresh = math.Inf(1)
		}
		take := false
		if isActive && !chosenActive[key] {
			take, chosenActive[key] = true, true
		} else if !chosenActive[key] && fresh <= bestFresh[key] {
			take = true
		}
		if take {
			bestFresh[key] = fresh
			av.ID, av.Name, av.Host, av.IP = c.id, h.displayName(c), c.host, c.ip
			av.Email = c.email
			av.PosSec, av.Paused, av.HeartbeatAgeSec = c.lastPosSec, c.lastPaused, hbAge
			if bw != nil {
				av.Kbps = bw.KbpsForIP(c.ip)
			}
		}
	}
	out := make([]AdminViewer, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsActiveHost != out[j].IsActiveHost {
			return out[i].IsActiveHost // active host first
		}
		return out[i].IP < out[j].IP
	})
	return out
}

// RecordHeartbeat updates the per-client position + paused fields the
// admin roster surfaces. id is the per-connection handle the client
// learned from its SSE init payload. Returns false if no matching
// connection is found (stale id, post-disconnect).
func (h *Hub) RecordHeartbeat(id string, posSec float64, paused bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.id == id {
			c.lastPosSec = posSec
			c.lastPaused = paused
			c.lastHeartbeatAt = time.Now().UnixNano()
			return true
		}
	}
	return false
}

// emailForConnLocked returns the email of the connection with the given
// id, and whether found. Caller holds h.mu.
func (h *Hub) emailForConnLocked(id string) (string, bool) {
	for c := range h.clients {
		if c.id == id {
			return c.email, true
		}
	}
	return "", false
}

// SetActiveHostByConn makes the user behind connection id the active host
// — any connected user, eligible or not (admin override). Errors if id
// isn't a current connection.
func (h *Hub) SetActiveHostByConn(id string) error {
	h.mu.Lock()
	email, ok := h.emailForConnLocked(id)
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("no such connection")
	}
	detail := ""
	if h.activeHost != email {
		detail = fmt.Sprintf("admin set active host to %q", h.nameForEmailLocked(email))
	}
	h.activeHost = email
	h.broadcast()
	h.mu.Unlock()
	h.wakeBroadcast()
	if detail != "" {
		h.audit.Record(AuditEvent{Type: "host", Email: "admin", Detail: detail})
	}
	return nil
}

// Handoff lets the current active host (byEmail) pass control to the user
// behind connection targetID. Errors if byEmail isn't the active host or
// the target isn't connected.
func (h *Hub) Handoff(byEmail, targetID string) error {
	h.mu.Lock()
	if h.activeHost != byEmail {
		h.mu.Unlock()
		return fmt.Errorf("not the active host")
	}
	email, ok := h.emailForConnLocked(targetID)
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("no such connection")
	}
	detail := ""
	if h.activeHost != email {
		detail = fmt.Sprintf("handed control to %q", h.nameForEmailLocked(email))
	}
	h.activeHost = email
	h.broadcast()
	h.mu.Unlock()
	h.wakeBroadcast()
	if detail != "" {
		h.audit.Record(AuditEvent{Type: "host", Email: byEmail, Detail: detail})
	}
	return nil
}

// KickClient closes the kill channel of the matching connection so its
// HandleEvents writer-loop returns. Returns true if a connection was
// found and signalled. The viewer's browser will reconnect almost
// immediately via EventSource auto-retry — kick is "drop this socket,"
// not "ban this user."
func (h *Hub) KickClient(id string) bool {
	h.mu.Lock()
	// Resolve the identity behind id, then kick EVERY connection that
	// identity holds — the admin roster shows one row per person, so a
	// single "Kick" should clear all their tabs / reloads / ghosts. An
	// empty-email (unauthenticated) connection is kicked on its own.
	var email string
	for c := range h.clients {
		if c.id == id {
			email = c.email
			break
		}
	}
	var targets []*clientEntry
	for c := range h.clients {
		if c.id == id || (email != "" && c.email == email) {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()
	for _, t := range targets {
		select {
		case <-t.kill:
			// already closed
		default:
			close(t.kill)
		}
	}
	return len(targets) > 0
}
