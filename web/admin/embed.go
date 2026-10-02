// Package admin embeds the admin console build (#754): `task admin-build`
// populates dist/. Only dist/.gitkeep is committed, so `go build ./...` needs
// no Node and FS then reports no console.
package admin

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built admin console SPA rooted at index.html, or nil when
// dist holds no build.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // embed layout is fixed at compile time
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
