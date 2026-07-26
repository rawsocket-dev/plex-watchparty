package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The /static/ routes serve with a far-future immutable Cache-Control,
// so every page reference must be content-addressed: a deploy that
// changes an asset must change its URL, or browsers keep the previous
// build's CSS/JS against the new HTML — unstyled, dead UI for up to a
// day after every deploy.
func TestPagesReferenceContentHashedAssets(t *testing.T) {
	pages := map[string][]byte{
		"index.html":   indexHTML,
		"player.html":  playerHTML,
		"waiting.html": waitingHTML,
	}
	assets := map[string][]byte{
		"/static/index.css":  indexCSS,
		"/static/index.js":   indexJS,
		"/static/common.js":  commonJS,
		"/static/player.css": playerCSS,
		"/static/player.js":  playerJS,
		"/static/hls.min.js": hlsJS,
	}
	// A /static/ ref whose name is followed by anything but ?v= is a
	// bare (cacheable-forever, never-busted) reference.
	unstamped := regexp.MustCompile(`/static/[A-Za-z0-9._-]+["'<\s]`)
	for name, body := range pages {
		s := string(body)
		for _, m := range unstamped.FindAllString(s, -1) {
			t.Errorf("%s references %s without a ?v= cache-buster", name, strings.TrimRight(m, "\"'< \n"))
		}
		for path, asset := range assets {
			if !strings.Contains(s, path) {
				continue // page doesn't use this asset
			}
			stamped := fmt.Sprintf("%s?v=%s", path, assetVersion(asset))
			if !strings.Contains(s, stamped) {
				t.Errorf("%s: reference to %s is not stamped with its content hash %q", name, path, stamped)
			}
		}
	}
}

func TestAssetVersionDeterministicAndDistinct(t *testing.T) {
	a := assetVersion([]byte("body { color: red }"))
	b := assetVersion([]byte("body { color: red }"))
	c := assetVersion([]byte("body { color: blue }"))
	if a != b {
		t.Errorf("same bytes hashed differently: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different bytes hashed identically: %q", a)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(a) {
		t.Errorf("version %q is not 8 lowercase hex chars", a)
	}
}
