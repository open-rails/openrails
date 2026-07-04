// Package webadmin embeds the built merchant admin console SPA (#740).
//
// web/dist is the committed Vite build output of web/admin (rebuild with
// `task admin-build`). It MUST stay committed: openrails is imported as a Go
// module by embedded hosts, and go:embed only ships files present in the
// module zip. web/admin itself is a separate marker module so its node tree
// never enters `go build ./...`.
package webadmin

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// DistFS returns the built SPA rooted at index.html.
func DistFS() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err) // embed layout is fixed at compile time
	}
	return sub
}

// HasBuiltAssets reports whether the embedded dist is a real Vite build
// (hashed bundles under assets/) rather than the committed placeholder.
func HasBuiltAssets() bool {
	entries, err := fs.ReadDir(embedded, "dist/assets")
	return err == nil && len(entries) > 0
}
