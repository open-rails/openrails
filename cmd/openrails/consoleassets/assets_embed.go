//go:build console_assets

// Console-bearing build (#754): `task admin-build` populates dist/, then
// `go build -tags console_assets` embeds it. Missing dist = compile error.
package consoleassets

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built admin console SPA rooted at index.html.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // embed layout is fixed at compile time
	}
	return sub
}
