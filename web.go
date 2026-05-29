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

// Throwaway design artifact — header-redesign mockups served at
// /admin/mockups so they're viewable in the deployed container.
// Remove this (and the route + file) once a direction is chosen.
//
//go:embed web/mockups.html
var mockupsHTML []byte
