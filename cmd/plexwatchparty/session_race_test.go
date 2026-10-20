package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type hookTransport func(*http.Request) (*http.Response, error)

func (f hookTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// hookPlex routes the fixture's Plex traffic through hook first; hook
// returns (nil, nil) to pass a request on to the mock unchanged. Install
// it before the first /control call: load spawns goroutines that use the
// client, so swapping the transport later is itself a data race.
func hookPlex(f *hubTestFixture, hook func(*http.Request) (*http.Response, error)) {
	next := f.hub.plex.http.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	f.hub.plex.http.Transport = hookTransport(func(r *http.Request) (*http.Response, error) {
		if resp, err := hook(r); resp != nil || err != nil {
			return resp, err
		}
		return next.RoundTrip(r)
	})
}

func plexStatus(code int) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("x")), Header: make(http.Header)}
}

// A playlist poll that is still waiting on Plex when the host switches
// titles must come back stamped with the session it was fetched for. It
// used to read the rating key before the fetch and the session id after
// it, minting contexts for the OLD title's segments that claimed the NEW
// session — so a failed fetch "recovered" by restarting the new title and
// cached its bytes under the old title's key — and it pushed the old
// playlist's edge onto the new session.
func TestPlaylistPollStampedWithItsOwnSession(t *testing.T) {
	f := segFixtureFor(t, newHubTestFixture(t))
	var held atomic.Value // session id whose next playlist poll hangs
	held.Store("")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hookPlex(f.hubTestFixture, func(r *http.Request) (*http.Response, error) {
		if sid := held.Load().(string); sid != "" && r.URL.Path == "/video/:/transcode/universal/start.m3u8" && r.URL.Query().Get("session") == sid {
			held := false
			once.Do(func() { held = true; close(entered); <-release })
			if held {
				return &http.Response{StatusCode: 200, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXTINF:6,\nold.ts\n#EXTINF:6,\nold2.ts\n"))}, nil
			}
		}
		return nil, nil
	})
	if w := f.post(t, `{"action":"load","ratingKey":"rk1"}`); w.Code != http.StatusOK {
		t.Fatalf("load rk1: %d %s", w.Code, w.Body.String())
	}
	oldSession := f.hub.session.SessionID()
	held.Store(oldSession)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		playlistHandler(f.codec, f.hub.session, f.bw)(w, httptest.NewRequest("GET", "/hls/index.m3u8", nil))
		done <- w
	}()
	<-entered
	if err := f.hub.session.Start("rk2", 0); err != nil {
		t.Fatalf("start rk2: %v", err)
	}
	newSession := f.hub.session.SessionID()
	close(release)
	w := <-done
	if w.Code != http.StatusOK {
		t.Fatalf("playlist: %d %s", w.Code, w.Body.String())
	}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "/hls/seg/") {
			continue
		}
		c, err := f.codec.decode(strings.TrimSuffix(strings.TrimPrefix(line, "/hls/seg/"), ".ts"))
		if err != nil {
			t.Fatal(err)
		}
		if c.Rating != "rk1" || c.SessionID != oldSession {
			t.Errorf("old playlist context = {Rating:%s Session:%s}, want {rk1 %s} (new session is %s)",
				c.Rating, c.SessionID, oldSession, newSession)
		}
	}
	if edge := f.hub.session.EdgeSec(); edge != 0 {
		t.Errorf("new session's edge = %.0fs after the old playlist landed, want 0", edge)
	}
}

// A segment request from the session a seek just replaced fails at Plex
// (the old transcode is gone). It must not "recover" by restarting Plex
// at its stale time: that tore down the session the seek had just started
// and left the room playing from the wrong offset.
func TestSegmentFromReplacedSessionDoesNotRestartPlex(t *testing.T) {
	f := newSegTestFixture(t)
	entered, stopSeen := make(chan struct{}), make(chan struct{})
	var once, onceStop sync.Once
	hookPlex(f.hubTestFixture, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/old.ts":
			// The fetch is already in flight when the seek lands;
			// Plex answers 404 once it has torn the old session down.
			once.Do(func() { close(entered); <-stopSeen })
			return plexStatus(http.StatusNotFound), nil
		case strings.HasSuffix(r.URL.Path, "/transcode/universal/stop"):
			onceStop.Do(func() { close(stopSeen) })
		}
		return nil, nil
	})
	if w := f.post(t, `{"action":"load","ratingKey":"rk1"}`); w.Code != http.StatusOK {
		t.Fatalf("load: %d %s", w.Code, w.Body.String())
	}
	c := segCtx{PlexURL: f.mock.URL + "/old.ts", SessionID: f.hub.session.SessionID(), Rating: "rk1", StartMs: 0, EndMs: 6000}
	done := make(chan int, 1)
	go func() { done <- f.get(t, c).Code }()
	<-entered
	if w := f.post(t, `{"action":"seek","positionSec":120}`); w.Code != http.StatusNoContent {
		t.Fatalf("seek: %d %s", w.Code, w.Body.String())
	}
	<-done
	if n := f.hub.session.LifecycleStats().RestartsByRecover; n != 0 {
		t.Errorf("recover restarts = %d, want 0", n)
	}
	if off := f.hub.session.OffsetMs(); off != 120000 {
		t.Errorf("session offset = %dms after the stale segment, want the seek's 120000", off)
	}
}

// If Plex refuses the replacement session (start over, a seek restart,
// an auto/admin restart, or a segment recovery), the old transcode is
// already gone. The room
// used to keep advertising the title anyway: viewers sat on a dead
// player, and the admin's restart / send-to-lobby both refused because
// Plex had no session. It now falls back to the lobby with a resume hint.
func TestFailedSessionReplacementReturnsRoomToLobby(t *testing.T) {
	viaControl := func(body string) func(*hubTestFixture) error {
		return func(f *hubTestFixture) error {
			if w := f.post(t, body); w.Code != http.StatusBadGateway {
				t.Fatalf("%s: %d %s, want 502", body, w.Code, w.Body.String())
			}
			return nil
		}
	}
	for _, tc := range []struct {
		name string
		do   func(*hubTestFixture) error
	}{
		{"start over", viaControl(`{"action":"load","ratingKey":"rk1","restart":true}`)},
		{"seek restart", viaControl(`{"action":"seek","positionSec":300}`)},
		{"auto or admin restart", func(f *hubTestFixture) error { return f.hub.RestartAtCurrentPosition(RestartByAdmin) }},
		{"segment recovery", func(f *hubTestFixture) error {
			_, err := f.hub.RecoverSegmentForRange(f.hub.session.SessionID(), 0, 6000)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHubTestFixture(t)
			var failing atomic.Bool
			hookPlex(f, func(r *http.Request) (*http.Response, error) {
				if failing.Load() && strings.HasSuffix(r.URL.Path, "/decision") {
					return plexStatus(http.StatusServiceUnavailable), nil
				}
				return nil, nil
			})
			if w := f.post(t, `{"action":"load","ratingKey":"rk1","positionSec":42}`); w.Code != http.StatusOK {
				t.Fatalf("load: %d %s", w.Code, w.Body.String())
			}
			failing.Store(true)
			_ = tc.do(f)
			s := f.hub.Snapshot()
			if s.RatingKey != "" {
				t.Errorf("room still advertises %q with no Plex session behind it", s.RatingKey)
			}
			if s.Resume == nil || s.Resume.RatingKey != "rk1" {
				t.Errorf("resume hint = %+v, want rk1 so the host can pick it back up", s.Resume)
			}
		})
	}
}

// A stop that lands while a load is still resolving its title must win.
// The load used to carry on regardless — start a fresh Plex transcode and
// broadcast the title again — so the host's "← library" or an admin's
// "send to lobby" was undone by a click made before it.
func TestStopCancelsPendingLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*testing.T, *hubTestFixture)
	}{
		{"host stop", func(t *testing.T, f *hubTestFixture) {
			if w := f.post(t, `{"action":"stop"}`); w.Code != http.StatusNoContent {
				t.Fatalf("stop: %d %s", w.Code, w.Body.String())
			}
		}},
		{"admin lobby", func(t *testing.T, f *hubTestFixture) { f.hub.SendEveryoneToLobby() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHubTestFixture(t)
			var armed atomic.Bool
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			hookPlex(f, func(r *http.Request) (*http.Response, error) {
				if armed.Load() && r.URL.Path == "/library/metadata/rk1" {
					once.Do(func() { close(entered); <-release })
				}
				return nil, nil
			})
			if w := f.post(t, `{"action":"load","ratingKey":"rk1"}`); w.Code != http.StatusOK {
				t.Fatalf("load: %d %s", w.Code, w.Body.String())
			}
			armed.Store(true)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- f.post(t, `{"action":"load","ratingKey":"rk1","restart":true}`) }()
			<-entered
			tc.stop(t, f)
			close(release)
			if w := <-done; w.Code != http.StatusConflict {
				t.Errorf("pending load: %d %s, want 409", w.Code, w.Body.String())
			}
			if rk := f.hub.Snapshot().RatingKey; rk != "" {
				t.Errorf("room advertises %q after the stop", rk)
			}
			if rk := f.hub.session.RatingKey(); rk != "" {
				t.Errorf("Plex transcode for %q running after the stop", rk)
			}

			// The stop cancels only loads already in flight.
			armed.Store(false)
			if w := f.post(t, `{"action":"load","ratingKey":"rk1"}`); w.Code != http.StatusOK {
				t.Fatalf("load after the stop: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// The same race one step later: the stop arrives while Plex is already
// starting the load. The stop tears the new transcode down once Start
// returns, so the load must not report success or broadcast the title —
// that would pull every library viewer into /watch only to bounce back.
func TestStopDuringPlexStartCancelsLoad(t *testing.T) {
	f := newHubTestFixture(t)
	var armed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hookPlex(f, func(r *http.Request) (*http.Response, error) {
		if armed.Load() && strings.HasSuffix(r.URL.Path, "/transcode/universal/decision") {
			once.Do(func() { close(entered); <-release })
		}
		return nil, nil
	})
	if w := f.post(t, `{"action":"load","ratingKey":"rk1"}`); w.Code != http.StatusOK {
		t.Fatalf("load: %d %s", w.Code, w.Body.String())
	}
	armed.Store(true)
	loaded := make(chan *httptest.ResponseRecorder, 1)
	go func() { loaded <- f.post(t, `{"action":"load","ratingKey":"rk1","restart":true}`) }()
	<-entered
	before := f.hub.stopGen.Load()
	stopped := make(chan *httptest.ResponseRecorder, 1)
	go func() { stopped <- f.post(t, `{"action":"stop"}`) }()
	for f.hub.stopGen.Load() == before { // the stop has begun; its session.Stop() queues on startMu
		runtime.Gosched()
	}
	close(release)
	if w := <-loaded; w.Code != http.StatusConflict {
		t.Errorf("pending load: %d %s, want 409", w.Code, w.Body.String())
	}
	if w := <-stopped; w.Code != http.StatusNoContent {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	if rk := f.hub.Snapshot().RatingKey; rk != "" {
		t.Errorf("room advertises %q after the stop", rk)
	}
	if rk := f.hub.session.RatingKey(); rk != "" {
		t.Errorf("Plex transcode for %q running after the stop", rk)
	}
}
