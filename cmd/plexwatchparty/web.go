package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

//go:embed web/login.html
var loginHTML []byte

//go:embed web/index.html
var indexHTML []byte

//go:embed web/player.html
var playerHTML []byte

//go:embed web/waiting.html
var waitingHTML []byte

//go:embed web/common.js
var commonJS []byte

// hls.js is vendored (pinned 1.6.16, from cdn.jsdelivr.net/npm/hls.js@1)
// rather than loaded off a CDN at runtime: an internet outage must not
// break LAN movie night, a CDN compromise must not inject script into
// authenticated sessions, and a floating @1 tag must not silently land
// untested upgrades. To update: curl the new dist/hls.min.js over this
// file and note the version here.
//
//go:embed web/hls.min.js
var hlsJS []byte

//go:embed web/player.css
var playerCSS []byte

//go:embed web/player.js
var playerJS []byte

//go:embed web/index.css
var indexCSS []byte

//go:embed web/index.js
var indexJS []byte

//go:embed web/admin.html
var adminHTML []byte

//go:embed web/admin.css
var adminCSS []byte

//go:embed web/admin.js
var adminJS []byte

// assetVersion is the cache-busting stamp for an embedded asset: the
// first 8 hex chars of its SHA-256. The /static/ routes serve with
// "public, max-age=86400, immutable", which is only safe if a changed
// asset gets a changed URL — otherwise browsers keep the previous
// build's CSS/JS against a freshly deployed page for up to a day.
func assetVersion(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// init rewrites every /static/ reference in the embedded pages to its
// content-addressed form (/static/name?v=<hash>). The /static/ handlers
// ignore the query string, so no route changes are needed; the stamp
// exists purely so the URL — and therefore the browser cache entry —
// rolls over exactly when the asset's bytes do. (The admin panel's
// /admin/static/ routes are no-cache and stay unstamped.)
func init() {
	assets := map[string][]byte{
		"/static/common.js":  commonJS,
		"/static/hls.min.js": hlsJS,
		"/static/player.css": playerCSS,
		"/static/player.js":  playerJS,
		"/static/index.css":  indexCSS,
		"/static/index.js":   indexJS,
	}
	pages := []*[]byte{&loginHTML, &indexHTML, &playerHTML, &waitingHTML}
	for path, body := range assets {
		stamped := []byte(path + "?v=" + assetVersion(body))
		for _, page := range pages {
			*page = bytes.ReplaceAll(*page, []byte(path), stamped)
		}
	}
}
