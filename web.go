package main

import _ "embed"

//go:embed web/login.html
var loginHTML []byte

//go:embed web/index.html
var indexHTML []byte

//go:embed web/player.html
var playerHTML []byte
