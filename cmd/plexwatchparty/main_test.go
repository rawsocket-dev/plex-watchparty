package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A user hitting "/" should be sent to the player (not the library) when a
// movie is playing and they are NOT the active host — host-eligibility
// alone doesn't entitle them to the library, since only the single active
// host can pick. The active host always sees the library so they can
// re-pick.
func TestLandOnWatch(t *testing.T) {
	cases := []struct {
		name            string
		playing, active bool
		want            bool
	}{
		{"no movie, not active host", false, false, false},
		{"no movie, active host", false, true, false},
		{"movie playing, active host stays on library", true, true, false},
		{"movie playing, non-active-host routes to watch", true, false, true},
	}
	for _, c := range cases {
		if got := landOnWatch(c.playing, c.active); got != c.want {
			t.Errorf("%s: landOnWatch(playing=%v, active=%v) = %v, want %v",
				c.name, c.playing, c.active, got, c.want)
		}
	}
}

// Finding-3: the public server must bound the request-header read phase
// (slowloris protection) while leaving Read/Write timeouts unset so that
// long-lived SSE (/events) and HLS streaming responses aren't severed.
func TestNewServerTimeouts(t *testing.T) {
	srv := newServer(":0", http.NewServeMux())
	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want > 0", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want > 0", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (SSE/HLS are long-lived)", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 (SSE/HLS are long-lived)", srv.ReadTimeout)
	}
}

// segTestFixture wires a segHandler against the hub fixture's mock Plex.
// encode() mints a real segment URL for a context pointing wherever the
// test wants (usually at the mock, sometimes at a dead port).
type segTestFixture struct {
	*hubTestFixture
	codec   *segCodec
	handler http.HandlerFunc
	bw      *bwTracker
}

func newSegTestFixture(t *testing.T) *segTestFixture {
	t.Helper()
	f := newHubTestFixture(t)
	codec, err := newSegCodec("tok")
	if err != nil {
		t.Fatalf("newSegCodec: %v", err)
	}
	bw := newBwTracker()
	return &segTestFixture{
		hubTestFixture: f,
		codec:          codec,
		handler:        segHandler(codec, f.cache, f.hub.session, f.hub, bw),
		bw:             bw,
	}
}

func (f *segTestFixture) get(t *testing.T, ctx segCtx) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/hls/seg/"+f.codec.encode(ctx)+".ts", nil)
	w := httptest.NewRecorder()
	f.handler(w, r)
	return w
}

// Failure responses must NOT go out with the immutable cache header —
// explicit freshness makes even a 502/404 cacheable, and a browser that
// pins the failure keeps replaying it to hls.js's retries for a day.
func TestSegHandlerErrorNotCacheable(t *testing.T) {
	f := newSegTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	sess := f.hub.session.SessionID()
	// Plex goes away entirely: the current-session fetch AND the
	// server-side recovery both fail -> 502.
	f.mock.Close()
	w := f.get(t, segCtx{PlexURL: f.mock.URL + "/seg.ts", SessionID: sess, StartMs: 0, EndMs: 6000, Rating: "rk1"})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") || strings.Contains(cc, "max-age") {
		t.Errorf("502 carries cacheable Cache-Control %q; must not be cacheable", cc)
	}
	// The stale-session 404 must be uncacheable too.
	w = f.get(t, segCtx{PlexURL: f.mock.URL + "/seg.ts", SessionID: "watchparty-superseded", StartMs: 0, EndMs: 6000, Rating: "rk1"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("stale status = %d, want 404", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") || strings.Contains(cc, "max-age") {
		t.Errorf("stale 404 carries cacheable Cache-Control %q; must not be cacheable", cc)
	}
}

// The success paths keep the aggressive immutable caching.
func TestSegHandlerCacheHitServesImmutable(t *testing.T) {
	f := newSegTestFixture(t)
	key := cacheKey{ratingKey: "rk1", startMs: 0, endMs: 6000}
	if _, err := f.cache.Put(key, strings.NewReader("cached-bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	w := f.get(t, segCtx{PlexURL: f.mock.URL + "/ignored.ts", StartMs: 0, EndMs: 6000, Rating: "rk1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "cached-bytes" {
		t.Errorf("body = %q, want cached bytes", got)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("cache hit Cache-Control = %q, want immutable", cc)
	}
}

// A cache entry whose file was evicted between the index lookup and the
// read must fall through to the normal fetch path — not serve a 404
// (previously: a 404 stamped immutable-cacheable).
func TestSegHandlerEvictedFileFallsThroughToFetch(t *testing.T) {
	f := newSegTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	key := cacheKey{ratingKey: "rk1", startMs: 0, endMs: 6000}
	path, err := f.cache.Put(key, strings.NewReader("stale"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.Remove(path); err != nil { // evict behind the index's back
		t.Fatalf("remove: %v", err)
	}
	// The upstream URL serves real bytes via the mock's playlist path.
	w := f.get(t, segCtx{PlexURL: f.mock.URL + "/video/:/transcode/universal/start.m3u8", SessionID: f.hub.session.SessionID(), StartMs: 0, EndMs: 6000, Rating: "rk1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fall through to Plex fetch)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "#EXTM3U") {
		t.Errorf("body = %q, want upstream bytes", w.Body.String())
	}
}

// A segment request minted from a SUPERSEDED session must not trigger a
// recovery restart (it would kill the live session — the churn where one
// offset seek cost ~3 Plex restarts) and must not count toward the
// auto-restart failure streak. It 404s; the client is reattaching anyway.
func TestSegHandlerStaleSessionNoRecoveryRestart(t *testing.T) {
	f := newSegTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	tok := f.hub.Snapshot().SessionToken

	for i, rng := range [][2]int64{{1000, 2000}, {2000, 3000}, {3000, 4000}} {
		w := f.get(t, segCtx{
			PlexURL: f.mock.URL + "/no-such-segment.ts", SessionID: "watchparty-superseded",
			StartMs: rng[0], EndMs: rng[1], Rating: "rk1",
		})
		if w.Code != http.StatusNotFound {
			t.Errorf("stale req %d: status = %d, want 404", i, w.Code)
		}
	}
	if got := f.hub.Snapshot().SessionToken; got != tok {
		t.Errorf("stale requests restarted the session: token %d -> %d", tok, got)
	}
	if n := f.hub.session.LifecycleStats().RestartsByRecover; n != 0 {
		t.Errorf("restartsByRecover = %d, want 0 (stale requests must not recover)", n)
	}
	f.hub.session.failMu.Lock()
	fails := f.hub.session.consecutiveSegFails
	f.hub.session.failMu.Unlock()
	if fails != 0 {
		t.Errorf("consecutiveSegFails = %d, want 0 (stale failures must not feed the auto-restart streak)", fails)
	}
}

// A stale-session request whose range IS in the segment cache still gets
// served — cached ranges are absolute movie times, valid across sessions.
func TestSegHandlerStaleSessionServedFromOverlapCache(t *testing.T) {
	f := newSegTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	if _, err := f.cache.Put(cacheKey{ratingKey: "rk1", startMs: 0, endMs: 6000}, strings.NewReader("cached-bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	w := f.get(t, segCtx{
		PlexURL: f.mock.URL + "/no-such-segment.ts", SessionID: "watchparty-superseded",
		StartMs: 1000, EndMs: 2000, Rating: "rk1",
	})
	if w.Code != http.StatusOK || w.Body.String() != "cached-bytes" {
		t.Errorf("stale+cached: status=%d body=%q, want 200 with cached bytes", w.Code, w.Body.String())
	}
}

// A CURRENT-session 404 must still go through server-side recovery —
// that's the real "Plex evicted/hasn't produced a segment" rescue.
func TestSegHandlerCurrentSessionStillRecovers(t *testing.T) {
	f := newSegTestFixture(t)
	f.post(t, `{"action":"load","ratingKey":"rk1"}`)
	w := f.get(t, segCtx{
		PlexURL: f.mock.URL + "/no-such-segment.ts", SessionID: f.hub.session.SessionID(),
		StartMs: 0, EndMs: 6000, Rating: "rk1",
	})
	if w.Code != http.StatusOK || w.Body.String() != "SEGDATA" {
		t.Errorf("current-session recovery: status=%d body=%q, want 200 SEGDATA", w.Code, w.Body.String())
	}
	if n := f.hub.session.LifecycleStats().RestartsByRecover; n != 1 {
		t.Errorf("restartsByRecover = %d, want 1 (recovery must still run for the live session)", n)
	}
}
