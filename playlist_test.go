package main

import (
	"strings"
	"testing"
)

const samplePlexPlaylist = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:10
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-MAP:URI="init.mp4"
#EXTINF:10.4,
https://plex.example/seg-0.ts
#EXTINF:10.4,
https://plex.example/seg-1.ts
#EXTINF:5.2,
https://plex.example/seg-2.ts
`

func TestRewritePlaylistComputesTimeRanges(t *testing.T) {
	_, segs, err := rewritePlaylist([]byte(samplePlexPlaylist), "https://plex.example/", 0, "rk1")
	if err != nil {
		t.Fatalf("rewritePlaylist: %v", err)
	}
	if len(segs) != 3 {
		t.Fatalf("len(segs) = %d, want 3", len(segs))
	}
	want := []struct{ s, e int64 }{{0, 10400}, {10400, 20800}, {20800, 26000}}
	for i, w := range want {
		if segs[i].StartMs != w.s || segs[i].EndMs != w.e {
			t.Errorf("segs[%d] = (%d, %d), want (%d, %d)",
				i, segs[i].StartMs, segs[i].EndMs, w.s, w.e)
		}
	}
}

func TestRewritePlaylistRespectsSessionOffset(t *testing.T) {
	_, segs, err := rewritePlaylist([]byte(samplePlexPlaylist), "https://plex.example/", 600000, "rk1")
	if err != nil {
		t.Fatal(err)
	}
	if segs[0].StartMs != 600000 || segs[0].EndMs != 610400 {
		t.Errorf("with offset=600s, first segment = (%d, %d), want (600000, 610400)",
			segs[0].StartMs, segs[0].EndMs)
	}
}

func TestRewritePlaylistRewritesURLs(t *testing.T) {
	out, _, err := rewritePlaylist([]byte(samplePlexPlaylist), "https://plex.example/", 0, "rk1")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "plex.example/seg-") {
		t.Errorf("original URLs leaked through:\n%s", s)
	}
	if !strings.Contains(s, "/hls/seg/") {
		t.Errorf("missing rewritten /hls/seg/ URL:\n%s", s)
	}
	// Header tags should pass through unchanged.
	for _, tag := range []string{"#EXTM3U", "#EXT-X-VERSION:6", "#EXT-X-TARGETDURATION:10"} {
		if !strings.Contains(s, tag) {
			t.Errorf("missing tag %q in output:\n%s", tag, s)
		}
	}
}

func TestSegCtxRoundTrip(t *testing.T) {
	c := segCtx{PlexURL: "https://plex/seg-0.ts?token=abc", StartMs: 1000, EndMs: 7000, Rating: "rk1"}
	enc := encodeSegCtx(c)
	if enc == "" {
		t.Fatal("encodeSegCtx returned empty")
	}
	got, err := decodeSegCtx(enc)
	if err != nil {
		t.Fatalf("decodeSegCtx: %v", err)
	}
	if *got != c {
		t.Errorf("round trip: got %+v, want %+v", *got, c)
	}
}
