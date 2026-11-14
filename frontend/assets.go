package frontend

import "embed"

// Assets contains the built desktop interface.
//
//go:embed all:dist
var Assets embed.FS
