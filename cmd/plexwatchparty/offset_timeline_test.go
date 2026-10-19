package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// realShapePlex mimics what Plex Media Server 1.43 actually returns for
// /video/:/transcode/universal/start.m3u8?offset=X: a FULL-LENGTH playlist
// on the absolute movie timeline (one 1 s segment per second of the title,
// 00000.ts onward) whose start is marked only by EXT-X-START. Segments
// before X are not transcoded; Plex answers them with a 188-byte PAT-only
// placeholder. Segments from X on carry the movie time they encode.
type realShapePlex struct {
	durationSec int
	offset      atomic.Int64
}

func (p *realShapePlex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const base = "/video/:/transcode/universal/"
	switch {
	case r.URL.Path == base+"decision":
		w.Write([]byte(`{"MediaContainer":{}}`))
	case r.URL.Path == base+"start.m3u8":
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		p.offset.Store(off)
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-START:TIME-OFFSET=%d.000000\n#EXT-X-ALLOW-CACHE:NO\n#EXT-X-MEDIA-SEQUENCE:0\n", off)
		for i := 0; i < p.durationSec; i++ {
			fmt.Fprintf(&b, "#EXTINF:1, nodesc\n%05d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		w.Write([]byte(b.String()))
	case r.URL.Path == base+"stop":
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(r.URL.Path, base) && strings.HasSuffix(r.URL.Path, ".ts"):
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base), ".ts"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if int64(n) < p.offset.Load() {
			w.Write(tsPlaceholder())
			return
		}
		w.Write(tsMediaSegment(n))
	case r.URL.Path == "/library/metadata/rk1":
		fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"title":"Test Movie","duration":%d,"Media":[{"videoCodec":"h264","width":1920,"height":1080,"Part":[{"key":"/p"}]}]}]}}`, p.durationSec*1000)
	default:
		http.NotFound(w, r)
	}
}

// tsPlaceholder is the 188-byte segment Plex returns for a segment outside
// the transcode window: one PAT packet padded with 0xFF, no media.
func tsPlaceholder() []byte {
	pkt := make([]byte, 188)
	for i := range pkt {
		pkt[i] = 0xFF
	}
	copy(pkt, []byte{0x47, 0x40, 0x00, 0x10, 0x00, 0x00, 0xB0, 0x0D, 0x00, 0x01, 0xC1, 0x00, 0x00, 0x00, 0x01, 0xF0, 0x00, 0x2A, 0xB1, 0x04, 0xB2})
	return pkt
}

// tsMediaSegment is a PAT packet followed by one video PES packet whose
// payload names the movie second it holds ("t=<sec>").
func tsMediaSegment(sec int) []byte {
	pes := make([]byte, 188)
	for i := range pes {
		pes[i] = 0xFF
	}
	copy(pes, []byte{0x47, 0x41, 0x00, 0x10, 0x00, 0x00, 0x01, 0xE0, 0x00, 0x00, 0x80, 0x00, 0x00})
	copy(pes[13:], fmt.Sprintf("t=%d;", sec))
	return append(tsPlaceholder(), pes...)
}

// offsetSession loads rk1 at startSec against the real-shaped mock and
// returns the rewritten /hls/seg URLs in playlist order.
func offsetSession(t *testing.T, startSec int) (*segTestFixture, []string) {
	t.Helper()
	f := segFixtureFor(t, newHubTestFixtureWith(t, &realShapePlex{durationSec: 600}))
	if w := f.post(t, fmt.Sprintf(`{"action":"load","ratingKey":"rk1","positionSec":%d}`, startSec)); w.Code != http.StatusOK {
		t.Fatalf("load: %d %s", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	playlistHandler(f.codec, f.hub.session, f.bw)(w, httptest.NewRequest("GET", "/hls/index.m3u8", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("playlist: %d %s", w.Code, w.Body.String())
	}
	var segs []string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "/hls/seg/") {
			segs = append(segs, line)
		}
	}
	if len(segs) != 600 {
		t.Fatalf("rewritten playlist has %d segments, want 600", len(segs))
	}
	return f, segs
}

func (f *segTestFixture) getURL(t *testing.T, u string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.handler(w, httptest.NewRequest("GET", u, nil))
	return w
}

// A session started mid-title must cache each segment under the movie time
// it actually holds. Plex's playlist is already on the absolute timeline,
// so adding the session offset again filed movie second 120 under 240 s —
// poisoning the persistent cache for every later viewer of that range.
func TestOffsetStartCachesSegmentsAtMovieTime(t *testing.T) {
	f, segs := offsetSession(t, 120)
	w := f.getURL(t, segs[120])
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "t=120;") {
		t.Fatalf("segment 120: status=%d body=%q", w.Code, w.Body.String())
	}
	path, ok := f.cache.Get(cacheKey{ratingKey: "rk1", startMs: 120000, endMs: 121000})
	if !ok {
		t.Fatalf("movie second 120 not cached under [120000,121000]; ranges=%v", f.cache.RangesFor("rk1"))
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "t=120;") {
		t.Errorf("cache entry [120000,121000] holds %q, want movie second 120", b)
	}
	if _, ok := f.cache.Get(cacheKey{ratingKey: "rk1", startMs: 240000, endMs: 241000}); ok {
		t.Errorf("movie second 120 was also filed under 240 s")
	}
}

// Plex answers segments outside the transcode window with a media-less
// placeholder. Passing it through is harmless; caching it would serve a
// blank second to every later session that reaches that movie time.
func TestPlaceholderSegmentIsNotCached(t *testing.T) {
	f, segs := offsetSession(t, 120)
	w := f.getURL(t, segs[0])
	if w.Code != http.StatusOK {
		t.Fatalf("segment 0: status=%d", w.Code)
	}
	if r := f.cache.RangesFor("rk1"); len(r) != 0 {
		t.Errorf("placeholder segment was cached: ranges=%v", r)
	}
}
