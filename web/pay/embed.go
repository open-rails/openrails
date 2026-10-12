// Package pay embeds the payment page's build (`task
// payment-page-build` populates dist/). Only dist/.gitkeep is committed, so Go
// builds without Node and FS then returns nil.
package pay

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built payment page rooted at index.html, or nil when dist
// holds no build.
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
