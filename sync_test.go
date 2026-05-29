package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

const testHostEmail = "tester@x.com"

// hubTestFixture spins up a Hub backed by a mock Plex server, an in-
// memory SegmentCache, and a tempdir RecentMovies. The mock answers
// /decision + /start.m3u8 + /library/* with the minimum we need to
// drive a load → play → seek → stop cycle.
type hubTestFixture struct {
	hub   *Hub
	mock  *httptest.Server
	cache *SegmentCache
	dir   string
}

func newHubTestFixture(t *testing.T) *hubTestFixture {
	t.Helper()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/video/:/transcode/universal/decision":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{}}`))
		case r.URL.Path == "/video/:/transcode/universal/start.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte("#EXTM3U\n#EXTINF:6,\nseg-0.ts\n"))
		case r.URL.Path == "/video/:/transcode/universal/stop":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/library/metadata/rk1":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"title":"Test Movie","duration":600000,"Media":[{"videoCodec":"h264","width":1920,"height":1080,"bitrate":12000,"Part":[{"key":"/p"}]}]}]}}`))
		case r.URL.Path == "/library/sections":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie"}]}}`))
		case r.URL.Path == "/library/sections/1/all":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"rk1","title":"Test Movie","year":2024}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)
	dir := t.TempDir()
	audit := NewAuditLog(filepath.Join(dir, "audit.jsonl"), auditCap)
	plex := NewPlex(mock.URL, "tok", filepath.Join(dir, "lib.json"), audit)
	cache := NewSegmentCache(filepath.Join(dir, "cache"), 1<<30)
	recent := NewRecentMovies(filepath.Join(dir, "recent.json"))
	store := NewStateStore(filepath.Join(dir, "state.json"))
	session := NewPlexSession(plex, 12000)
	hub := NewHub(plex, session, cache, recent, store, audit)
	hub.mu.Lock()
	hub.clients[&clientEntry{id: "t", email: testHostEmail, host: false, name: "Tester", send: make(chan []byte, 8), kill: make(chan struct{})}] = struct{}{}
	hub.activeHost = testHostEmail
	hub.mu.Unlock()
	// Stop the Hub's background loops/timers and drain pending state
	// writes before t.TempDir's RemoveAll runs — cleanups are LIFO, so
	// registering this after t.TempDir() makes it run first. Without it
	// a late Save recreates a file (its MkdirAll even recreates the dir)
	// mid-removal → "directory not empty". Close subsumes store.Wait().
	t.Cleanup(hub.Close)
	return &hubTestFixture{hub: hub, mock: mock, cache: cache, dir: dir}
}

func (f *hubTestFixture) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/control", bytes.NewBufferString(body))
	r = r.WithContext(context.WithValue(r.Context(), actorCtxKey{}, testHostEmail))
	w := httptest.NewRecorder()
	f.hub.HandleControl(w, r)
	return w
}

func TestClampSeekTarget(t *testing.T) {
	cases := []struct {
		in, dur, want float64
		ok            bool
	}{
		{100, 600, 100, true},
		{-5, 600, 0, true},      // negative clamps to 0
		{99999, 600, 600, true}, // past the end clamps to duration
		{300, 0, 300, true},     // unknown duration: no upper clamp
		{math.NaN(), 600, 0, false},
		{math.Inf(1), 600, 0, false},
		{math.Inf(-1), 600, 0, false},
	}
	for _, c := range cases {
		got, ok := clampSeekTarget(c.in, c.dur)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("clampSeekTarget(%v, %v) = (%v, %v), want (%v, %v)",
				c.in, c.dur, got, ok, c.want, c.ok)
		}
	}
}

func TestHubControlRejectsActionsWithoutActiveMovie(t *testing.T) {
	f := newHubTestFixture(t)
	for _, action := range []string{"play", "pause", "seek"} {
		w := f.post(t, `{"action":"`+action+`"}`)
		if w.Code != http.StatusConflict {
			t.Errorf("%s with no active movie = %d, want 409", action, w.Code)
		}
	}
}

func TestHubSeekClampsToDuration(t *testing.T) {
	f := newHubTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`) // duration 600s
	w := f.post(t, `{"action":"seek","positionSec":99999}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("seek status = %d: %s", w.Code, w.Body.String())
	}
	if got := f.hub.Snapshot().PositionSec; got != 600 {
		t.Errorf("seek to 99999 clamped to %v, want 600 (duration)", got)
	}
}

func TestViewerListMarksOnlyActiveHost(t *testing.T) {
	f := newHubTestFixture(t)
	// Two connected, BOTH host-eligible (host:true), but only one is the
	// active host. The roster must mark exactly that one as host — mere
	// eligibility must not show up as a second "host".
	f.hub.mu.Lock()
	f.hub.clients = map[*clientEntry]struct{}{}
	f.hub.clients[&clientEntry{id: "a", email: "alice@x.com", host: true, name: "Alice", send: make(chan []byte, 8), kill: make(chan struct{})}] = struct{}{}
	f.hub.clients[&clientEntry{id: "b", email: "bob@x.com", host: true, name: "Bob", send: make(chan []byte, 8), kill: make(chan struct{})}] = struct{}{}
	f.hub.activeHost = "alice@x.com"
	list := f.hub.viewerList()
	f.hub.mu.Unlock()

	var hosts []string
	for _, v := range list {
		if v.Host {
			hosts = append(hosts, v.Name)
		}
	}
	if len(hosts) != 1 || hosts[0] != "Alice" {
		t.Errorf("roster hosts = %v, want exactly [Alice]; both are host-eligible but only the active host drives", hosts)
	}
}

func TestViewerListDedupesByIdentity(t *testing.T) {
	f := newHubTestFixture(t)
	h := f.hub
	h.mu.Lock()
	h.clients = map[*clientEntry]struct{}{}
	h.activeHost = "alice@x.com"
	for _, c := range []*clientEntry{
		{id: "a1", email: "alice@x.com", name: "Alice"},
		{id: "a2", email: "alice@x.com", name: "Alice"}, // same person, 2nd tab
		{id: "b1", email: "bob@x.com", name: "Bob"},
	} {
		h.clients[c] = struct{}{}
	}
	list := h.viewerList()
	h.mu.Unlock()

	if len(list) != 2 {
		t.Fatalf("viewerList has %d entries, want 2 (one per person): %+v", len(list), list)
	}
	var hosts int
	for _, v := range list {
		if v.Host {
			hosts++
		}
	}
	if hosts != 1 {
		t.Errorf("want exactly one host in the deduped roster, got %d", hosts)
	}
}

func TestActiveHostStaysWhenSecondEligibleJoins(t *testing.T) {
	f := newHubTestFixture(t)
	f.hub.mu.Lock()
	f.hub.activeHost = ""
	f.hub.mu.Unlock()
	f.addConn("alice@x.com", true)
	if f.active() != "alice@x.com" {
		t.Fatalf("first eligible should become active host, got %q", f.active())
	}
	f.addConn("bob@x.com", true)
	if f.active() != "alice@x.com" {
		t.Errorf("active host changed to %q when a second eligible joined; must stay alice", f.active())
	}
}

func TestActiveHostReclaimsOnReconnect(t *testing.T) {
	f := newHubTestFixture(t)
	f.hub.mu.Lock()
	f.hub.activeHost = ""
	f.hub.mu.Unlock()
	alice := f.addConn("alice@x.com", true)
	f.addConn("bob@x.com", true)
	// Alice blips out — her slot is held during the grace window.
	f.removeConn(alice)
	if f.active() != "alice@x.com" {
		t.Fatalf("slot not held during grace: active=%q", f.active())
	}
	// She reconnects within the window → keeps the remote, timer cancelled.
	f.addConn("alice@x.com", true)
	if f.active() != "alice@x.com" {
		t.Errorf("active host = %q after alice reconnected; should remain alice", f.active())
	}
	f.hub.mu.Lock()
	timerLive := f.hub.hostReassignTimer != nil
	f.hub.mu.Unlock()
	if timerLive {
		t.Error("reassign timer should be cancelled once the active host reconnects")
	}
}

func TestAdminRosterDedupesByIdentity(t *testing.T) {
	f := newHubTestFixture(t)
	h := f.hub
	h.mu.Lock()
	h.clients = map[*clientEntry]struct{}{}
	h.activeHost = "alice@x.com"
	mk := func(id, email, name, ip string) {
		h.clients[&clientEntry{id: id, email: email, name: name, ip: ip, connectedAt: time.Now(), send: make(chan []byte, 8), kill: make(chan struct{})}] = struct{}{}
	}
	mk("a1", "alice@x.com", "Alice", "10.0.0.1") // alice: 3 connections
	mk("a2", "alice@x.com", "Alice", "10.0.0.1")
	mk("a3", "alice@x.com", "Alice", "10.0.0.1")
	mk("b1", "bob@x.com", "Bob", "10.0.0.2") // bob: 1
	h.mu.Unlock()

	roster := h.AdminRoster(nil)
	if len(roster) != 2 {
		t.Fatalf("roster has %d rows, want 2 (one per person)", len(roster))
	}
	var alice, bob *AdminViewer
	for i := range roster {
		switch roster[i].Name {
		case "Alice":
			alice = &roster[i]
		case "Bob":
			bob = &roster[i]
		}
	}
	if alice == nil || bob == nil {
		t.Fatalf("missing rows: %+v", roster)
	}
	if alice.Conns != 3 {
		t.Errorf("alice conns=%d, want 3", alice.Conns)
	}
	if bob.Conns != 1 {
		t.Errorf("bob conns=%d, want 1", bob.Conns)
	}
	if !alice.IsActiveHost || bob.IsActiveHost {
		t.Errorf("active host wrong: alice=%v bob=%v", alice.IsActiveHost, bob.IsActiveHost)
	}
}

func TestKickRemovesAllOfPersonsConnections(t *testing.T) {
	f := newHubTestFixture(t)
	h := f.hub
	h.mu.Lock()
	h.clients = map[*clientEntry]struct{}{}
	a1 := &clientEntry{id: "a1", email: "alice@x.com", name: "Alice", kill: make(chan struct{}), send: make(chan []byte, 8)}
	a2 := &clientEntry{id: "a2", email: "alice@x.com", name: "Alice", kill: make(chan struct{}), send: make(chan []byte, 8)}
	b1 := &clientEntry{id: "b1", email: "bob@x.com", name: "Bob", kill: make(chan struct{}), send: make(chan []byte, 8)}
	for _, c := range []*clientEntry{a1, a2, b1} {
		h.clients[c] = struct{}{}
	}
	h.mu.Unlock()

	if !h.KickClient("a1") {
		t.Fatal("KickClient returned false")
	}
	closed := func(ch chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	if !closed(a1.kill) || !closed(a2.kill) {
		t.Error("kicking one of alice's connections should kick all of them")
	}
	if closed(b1.kill) {
		t.Error("bob must not be kicked")
	}
}

func TestHubLoadSetsState(t *testing.T) {
	f := newHubTestFixture(t)
	w := f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	s := f.hub.Snapshot()
	if s.RatingKey != "rk1" {
		t.Errorf("RatingKey = %q, want rk1", s.RatingKey)
	}
	if s.Title != "Test Movie" {
		t.Errorf("Title = %q, want Test Movie", s.Title)
	}
	if s.DurationSec != 600 {
		t.Errorf("DurationSec = %v, want 600", s.DurationSec)
	}
	if s.SessionToken == 0 {
		t.Error("SessionToken not bumped after load")
	}
}

func TestHubLoadSameMovieReusesSession(t *testing.T) {
	f := newHubTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	tok1 := f.hub.Snapshot().SessionToken

	// Second load of the same ratingKey should NOT bump the token —
	// the session-reuse short-circuit kicks in.
	w := f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("second load: status = %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if reused, _ := resp["reused"].(bool); !reused {
		t.Errorf("response.reused = %v, want true", resp["reused"])
	}
	if got := f.hub.Snapshot().SessionToken; got != tok1 {
		t.Errorf("SessionToken bumped on same-movie reuse: %d -> %d", tok1, got)
	}
}

func TestHubLoadSameMovieWithRestartForcesStart(t *testing.T) {
	f := newHubTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	tok1 := f.hub.Snapshot().SessionToken
	w := f.post(t, `{"action":"load","ratingKey":"rk1","restart":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("restart load: status = %d", w.Code)
	}
	if got := f.hub.Snapshot().SessionToken; got <= tok1 {
		t.Errorf("SessionToken did not bump with restart=true: %d -> %d", tok1, got)
	}
}

func TestHubLoadAutoplayControlsInitialPlaying(t *testing.T) {
	// The resume banners (library + waiting room) send autoplay:true so
	// the room starts playing instead of paused. A plain load (movie
	// pick) omits it and stays paused. Lock both halves of the contract.
	f := newHubTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1","autoplay":true}`)
	if !f.hub.Snapshot().Playing {
		t.Error("load with autoplay:true → Playing=false, want true")
	}

	g := newHubTestFixture(t)
	g.post(t, `{"action":"load","ratingKey":"rk1"}`)
	if g.hub.Snapshot().Playing {
		t.Error("load without autoplay → Playing=true, want false")
	}
}

func TestHubPlayPauseFlipsState(t *testing.T) {
	f := newHubTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	f.post(t, `{"action":"play"}`)
	if !f.hub.Snapshot().Playing {
		t.Error("after play: Playing = false, want true")
	}
	f.post(t, `{"action":"pause"}`)
	if f.hub.Snapshot().Playing {
		t.Error("after pause: Playing = true, want false")
	}
}

func TestHubStopClearsState(t *testing.T) {
	f := newHubTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	w := f.post(t, `{"action":"stop"}`)
	if w.Code != http.StatusNoContent {
		t.Errorf("stop status = %d, want 204", w.Code)
	}
	s := f.hub.Snapshot()
	if s.RatingKey != "" || s.Title != "" {
		t.Errorf("after stop: RatingKey=%q Title=%q, want empty", s.RatingKey, s.Title)
	}
}

func TestHubControlRejectsOversizedBody(t *testing.T) {
	f := newHubTestFixture(t)
	// 5 KiB of garbage, just over the 4 KiB cap.
	big := bytes.Repeat([]byte("x"), 5<<10)
	r := httptest.NewRequest("POST", "/control", bytes.NewReader(big))
	w := httptest.NewRecorder()
	f.hub.HandleControl(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("oversized body: status = %d, want 400", w.Code)
	}
}

func TestHubControlRejectsUnknownAction(t *testing.T) {
	f := newHubTestFixture(t)
	w := f.post(t, `{"action":"explode"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown action: status = %d, want 400", w.Code)
	}
}

func TestHubLoadPopulatesRecent(t *testing.T) {
	f := newHubTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	got := f.hub.recent.List()
	if len(got) != 1 {
		t.Fatalf("recent.List len = %d, want 1", len(got))
	}
	if got[0].RatingKey != "rk1" || got[0].Title != "Test Movie" || got[0].Year != 2024 {
		t.Errorf("recent[0] = %+v, want rk1/Test Movie/2024", got[0])
	}
}

func TestHubCloseStopsLoops(t *testing.T) {
	f := newHubTestFixture(t)

	// Close must return promptly: both background loops have to observe
	// the done signal and exit, and the WaitGroup wait must not deadlock.
	done := make(chan struct{})
	go func() { f.hub.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s — a loop failed to observe done")
	}

	// Idempotent: a second Close must not panic (double channel close)
	// or hang. The fixture's t.Cleanup also calls Close, exercising a
	// third call.
	f.hub.Close()
}

func TestHubHandleEventsReportsViewer(t *testing.T) {
	f := newHubTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	// Drop the fixture's pre-seeded connection so the one we open below is
	// the only connection for testHostEmail — otherwise the roster dedupe
	// (one entry per identity) would merge them and keep the first name.
	f.hub.mu.Lock()
	f.hub.clients = map[*clientEntry]struct{}{}
	f.hub.mu.Unlock()

	// Spin a goroutine that connects, reads the initial state, and
	// exits. The main goroutine inspects the viewer list afterwards.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hub.HandleEvents(w, r, true, testHostEmail)
	}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.AddCookie(&http.Cookie{Name: nameCookie, Value: "Alice"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	// Read just the initial "data: { ... }\n\n" frame so we can
	// observe the registered viewer; then bail.
	buf := make([]byte, 4096)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := f.hub.Snapshot()
		if len(s.Viewers) > 0 {
			if s.Viewers[0].Name != "Alice" {
				t.Errorf("viewer name = %q, want Alice", s.Viewers[0].Name)
			}
			if !s.Viewers[0].Host {
				t.Errorf("viewer.Host = false, want true (passed isHost=true)")
			}
			_, _ = io.ReadFull(resp.Body, buf[:0])
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no viewer registered within deadline")
}

func (f *hubTestFixture) addConn(email string, eligible bool) *clientEntry {
	e := &clientEntry{id: randomHex(4), email: email, host: eligible, name: email, send: make(chan []byte, 8), kill: make(chan struct{})}
	f.hub.mu.Lock()
	f.hub.clients[e] = struct{}{}
	f.hub.onClientCountChange()
	f.hub.mu.Unlock()
	return e
}
func (f *hubTestFixture) removeConn(e *clientEntry) {
	f.hub.mu.Lock()
	delete(f.hub.clients, e)
	f.hub.onClientCountChange()
	f.hub.mu.Unlock()
}
func (f *hubTestFixture) active() string {
	f.hub.mu.Lock()
	defer f.hub.mu.Unlock()
	return f.hub.activeHost
}

// fireHostReassign drives the host-reassign grace path deterministically:
// it cancels the real pending timer (so it can't double-fire later) and
// runs the reassignment as if the grace window had just expired.
func (f *hubTestFixture) fireHostReassign(gone string) {
	f.hub.mu.Lock()
	if f.hub.hostReassignTimer != nil {
		f.hub.hostReassignTimer.Stop()
		f.hub.hostReassignTimer = nil
	}
	f.hub.mu.Unlock()
	f.hub.reassignHostAfterGrace(gone)
}

func TestHostFirstEligibleWins(t *testing.T) {
	f := newHubTestFixture(t)
	f.hub.mu.Lock(); f.hub.activeHost = ""; f.hub.mu.Unlock()
	f.addConn("ineligible@x.com", false)
	if f.active() != "" {
		t.Fatalf("ineligible-only connect elected %q, want none", f.active())
	}
	f.addConn("alice@x.com", true)
	if f.active() != "alice@x.com" {
		t.Errorf("first eligible: active=%q, want alice@x.com", f.active())
	}
	f.addConn("bob@x.com", true)
	if f.active() != "alice@x.com" {
		t.Errorf("second eligible must not steal: active=%q, want alice@x.com", f.active())
	}
}

func TestHostReassignsOnDisconnect(t *testing.T) {
	f := newHubTestFixture(t)
	f.hub.mu.Lock(); f.hub.activeHost = ""; f.hub.mu.Unlock()
	a := f.addConn("alice@x.com", true)
	f.addConn("bob@x.com", true)
	if f.active() != "alice@x.com" {
		t.Fatalf("setup: active=%q", f.active())
	}
	// A disconnect HOLDS the remote for a grace window — a transient blip
	// must not instantly hand control to bob.
	f.removeConn(a)
	if f.active() != "alice@x.com" {
		t.Errorf("right after disconnect: active=%q, want alice held during grace", f.active())
	}
	// Grace expires with alice still gone → control passes to the remaining
	// eligible viewer.
	f.fireHostReassign("alice@x.com")
	if f.active() != "bob@x.com" {
		t.Errorf("after grace: active=%q, want bob@x.com", f.active())
	}
	// Everyone leaves; after grace there's no one eligible → no active host.
	f.hub.mu.Lock()
	for e := range f.hub.clients {
		delete(f.hub.clients, e)
	}
	f.hub.onClientCountChange()
	f.hub.mu.Unlock()
	f.fireHostReassign("bob@x.com")
	if f.active() != "" {
		t.Errorf("no connections after grace: active=%q, want empty", f.active())
	}
}

func TestHostMultiTabRetainsSlot(t *testing.T) {
	f := newHubTestFixture(t)
	f.hub.mu.Lock(); f.hub.activeHost = ""; f.hub.mu.Unlock()
	tab1 := f.addConn("alice@x.com", true)
	_ = f.addConn("alice@x.com", true)
	f.removeConn(tab1)
	if f.active() != "alice@x.com" {
		t.Errorf("slot lost on one-tab close: active=%q, want alice@x.com", f.active())
	}
}

func TestControlRejectsNonActiveHost(t *testing.T) {
	f := newHubTestFixture(t) // activeHost = testHostEmail
	r := httptest.NewRequest("POST", "/control", bytes.NewBufferString(`{"action":"pause"}`))
	r = r.WithContext(context.WithValue(r.Context(), actorCtxKey{}, "someone-else@x.com"))
	w := httptest.NewRecorder()
	f.hub.HandleControl(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-active-host /control = %d, want 403", w.Code)
	}
}

func TestControlAllowsActiveHost(t *testing.T) {
	f := newHubTestFixture(t)
	w := f.post(t, `{"action":"play"}`) // post() acts as testHostEmail (active host)
	if w.Code == http.StatusForbidden {
		t.Errorf("active host /control got 403, want allowed")
	}
}

func TestIsActiveHost(t *testing.T) {
	f := newHubTestFixture(t)
	if !f.hub.IsActiveHost("tester@x.com") {
		t.Error("IsActiveHost(active) = false, want true")
	}
	if f.hub.IsActiveHost("nope@x.com") {
		t.Error("IsActiveHost(other) = true, want false")
	}
}

func TestSetActiveHostByConn(t *testing.T) {
	f := newHubTestFixture(t)
	e := f.addConn("guest@x.com", false) // non-eligible, but admin can promote
	if err := f.hub.SetActiveHostByConn(e.id); err != nil {
		t.Fatalf("SetActiveHostByConn: %v", err)
	}
	if f.active() != "guest@x.com" {
		t.Errorf("admin set: active=%q, want guest@x.com", f.active())
	}
	if err := f.hub.SetActiveHostByConn("no-such-id"); err == nil {
		t.Error("SetActiveHostByConn(bad id) = nil err, want error")
	}
}

func TestHandoffByActiveHost(t *testing.T) {
	f := newHubTestFixture(t)
	e := f.addConn("buddy@x.com", true)
	if err := f.hub.Handoff("tester@x.com", e.id); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	if f.active() != "buddy@x.com" {
		t.Errorf("after handoff: active=%q, want buddy@x.com", f.active())
	}
	if err := f.hub.Handoff("someone@x.com", e.id); err == nil {
		t.Error("Handoff by non-active-host = nil err, want error")
	}
}
