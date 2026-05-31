package main

import (
	"bytes"
	"encoding/base64"
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

func testSegCodec(t *testing.T) *segCodec {
	t.Helper()
	c, err := newSegCodec("test-seed")
	if err != nil {
		t.Fatalf("newSegCodec: %v", err)
	}
	return c
}

func TestRewritePlaylistComputesTimeRanges(t *testing.T) {
	_, segs, err := rewritePlaylist(testSegCodec(t), []byte(samplePlexPlaylist), "https://plex.example/", 0, "rk1")
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
	_, segs, err := rewritePlaylist(testSegCodec(t), []byte(samplePlexPlaylist), "https://plex.example/", 600000, "rk1")
	if err != nil {
		t.Fatal(err)
	}
	if segs[0].StartMs != 600000 || segs[0].EndMs != 610400 {
		t.Errorf("with offset=600s, first segment = (%d, %d), want (600000, 610400)",
			segs[0].StartMs, segs[0].EndMs)
	}
}

func TestRewritePlaylistRewritesURLs(t *testing.T) {
	out, _, err := rewritePlaylist(testSegCodec(t), []byte(samplePlexPlaylist), "https://plex.example/", 0, "rk1")
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

func TestSegCodecRoundTrip(t *testing.T) {
	codec := testSegCodec(t)
	c := segCtx{PlexURL: "https://plex/seg-0.ts?token=abc", StartMs: 1000, EndMs: 7000, Rating: "rk1"}
	enc := codec.encode(c)
	if enc == "" {
		t.Fatal("encode returned empty")
	}
	got, err := codec.decode(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if *got != c {
		t.Errorf("round trip: got %+v, want %+v", *got, c)
	}
}

// A finding-1 regression test: the Plex token (carried inside segCtx.PlexURL)
// must not be recoverable from the client-visible /hls/seg/<enc>.ts path. With
// the old base64 encoding it was; AES-GCM makes the blob opaque ciphertext.
func TestRewritePlaylistHidesPlexToken(t *testing.T) {
	codec := testSegCodec(t)
	const token = "SECRETPLEXTOKEN123"
	pl := "#EXTM3U\n#EXTINF:4.0,\nhttps://plex.example/seg-0.ts?X-Plex-Token=" + token + "\n"
	out, _, err := rewritePlaylist(codec, []byte(pl), "https://plex.example/", 0, "rk1")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, token) {
		t.Fatalf("plex token leaked verbatim in rewritten playlist:\n%s", s)
	}
	// Pull the encoded blob and confirm base64-decoding it does NOT reveal
	// the token (it's ciphertext, not the old plaintext JSON).
	idx := strings.Index(s, "/hls/seg/")
	if idx < 0 {
		t.Fatalf("no /hls/seg/ url in output:\n%s", s)
	}
	rest := s[idx+len("/hls/seg/"):]
	dot := strings.Index(rest, ".ts")
	if dot < 0 {
		t.Fatalf("malformed seg url:\n%s", s)
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest[:dot])
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if bytes.Contains(raw, []byte(token)) {
		t.Fatal("plex token recoverable from base64-decoded seg context")
	}
	// The server, holding the key, can still recover the real URL.
	got, err := codec.decode(rest[:dot])
	if err != nil {
		t.Fatalf("server-side decode: %v", err)
	}
	if !strings.Contains(got.PlexURL, token) {
		t.Fatalf("server-side decode lost the token: %q", got.PlexURL)
	}
}

// Finding-2: a forged/tampered context must fail authentication so a viewer
// can't substitute an arbitrary PlexURL (SSRF) or ratingKey (cache path).
func TestSegCodecRejectsTamperedContext(t *testing.T) {
	codec := testSegCodec(t)
	enc := codec.encode(segCtx{PlexURL: "https://plex/x.ts?X-Plex-Token=t", StartMs: 0, EndMs: 1000, Rating: "rk1"})
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff // flip a byte in the GCM tag / ciphertext
	if _, err := codec.decode(base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("decode accepted a tampered context; GCM auth must reject it")
	}
}

func TestSegCodecRejectsForeignKey(t *testing.T) {
	a, err := newSegCodec("seed-A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newSegCodec("seed-B")
	if err != nil {
		t.Fatal(err)
	}
	enc := a.encode(segCtx{PlexURL: "https://plex/x.ts?X-Plex-Token=t", Rating: "rk1"})
	if _, err := b.decode(enc); err == nil {
		t.Fatal("a context minted under one key was decodable/forgeable under another")
	}
}
