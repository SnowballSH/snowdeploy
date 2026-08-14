// Package web carries the built single-page app. The assets are committed so
// `go build` alone produces a self-contained daemon: no Node toolchain is
// needed to build or install the binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var assets embed.FS

// Assets is the built app rooted at its index.html.
func Assets() (fs.FS, error) {
	return fs.Sub(assets, "dist")
}
