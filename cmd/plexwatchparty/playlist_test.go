package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
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
	_, segs, err := rewritePlaylist(testSegCodec(t), []byte(samplePlexPlaylist), "https://plex.example/", "rk1", "sess-test")
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

// Plex's playlist for a transcode started at offset=X lists the whole
// title from 00000.ts and marks X only with EXT-X-START (captured from PMS
// 1.43.5). Its cumulative time is already movie time, so the segment
// after the 2670th second must be stamped 2670 s — not 2670 s + offset.
func TestRewritePlaylistTimesAreAbsoluteForOffsetStart(t *testing.T) {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-START:TIME-OFFSET=2670.000000\n#EXT-X-ALLOW-CACHE:NO\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := 0; i < 5696; i++ {
		fmt.Fprintf(&b, "#EXTINF:1, nodesc\n%05d.ts\n", i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	_, segs, err := rewritePlaylist(testSegCodec(t), []byte(b.String()), "https://plex.example/session/s/base/index.m3u8", "rk1", "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	if got := segs[2670]; got.StartMs != 2670000 || got.EndMs != 2671000 || !strings.HasSuffix(got.OrigURL, "/02670.ts") {
		t.Errorf("segs[2670] = %+v, want 02670.ts at [2670000,2671000]", got)
	}
	if last := segs[len(segs)-1]; last.EndMs != 5696000 {
		t.Errorf("last segment ends at %d, want the title's end 5696000", last.EndMs)
	}
}

func TestTSHasNoMedia(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"plex placeholder (PAT only)", tsPlaceholder(), true},
		{"PAT + video PES", tsMediaSegment(7), false},
		{"not TS at all", []byte("SEGDATA"), false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := tsHasNoMedia(c.data); got != c.want {
			t.Errorf("%s: tsHasNoMedia = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRewritePlaylistRewritesURLs(t *testing.T) {
	out, _, err := rewritePlaylist(testSegCodec(t), []byte(samplePlexPlaylist), "https://plex.example/", "rk1", "sess-test")
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
	out, _, err := rewritePlaylist(codec, []byte(pl), "https://plex.example/", "rk1", "sess-test")
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

// Every rewritten segment context must carry the id of the Plex session
// whose playlist minted it, so the segment proxy can tell an in-flight
// request from a SUPERSEDED session apart from a current-session failure
// — the former must not trigger a recovery restart (the churn where one
// offset seek cost ~3 Plex restarts).
func TestRewritePlaylistStampsSessionID(t *testing.T) {
	codec := testSegCodec(t)
	body, segs, err := rewritePlaylist(codec, []byte(samplePlexPlaylist), "https://plex.example/", "rk1", "watchparty-abc123")
	if err != nil {
		t.Fatalf("rewritePlaylist: %v", err)
	}
	if len(segs) != 3 {
		t.Fatalf("len(segs) = %d, want 3", len(segs))
	}
	// Decode the first rewritten URL's context and check the stamp.
	var enc string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "/hls/seg/") {
			enc = strings.TrimSuffix(strings.TrimPrefix(line, "/hls/seg/"), ".ts")
			break
		}
	}
	if enc == "" {
		t.Fatal("no rewritten segment URL found")
	}
	ctx, err := codec.decode(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ctx.SessionID != "watchparty-abc123" {
		t.Errorf("ctx.SessionID = %q, want the minting session id", ctx.SessionID)
	}
}
