package main

import _ "embed"

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
