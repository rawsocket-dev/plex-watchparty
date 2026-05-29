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

// Throwaway design gallery — logout-button placement options served at
// /mockups (public, no auth: pure HTML + CSS, no movie metadata, no Plex
// token surface). Remove this (route + embed + file) once a direction is
// chosen and wired into the real pages.
//
//go:embed web/mockups.html
var mockupsHTML []byte

//go:embed web/common.js
var commonJS []byte

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
