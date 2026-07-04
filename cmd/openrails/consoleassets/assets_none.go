//go:build !console_assets

// Package consoleassets is the BINARY-boundary home of the admin console SPA
// bytes (#754): plain `go build ./...` links zero frontend bytes and never
// needs Node; `-tags console_assets` embeds the dist/ produced by
// `task admin-build` (see assets_embed.go). Enabling admin_console on a
// no-assets binary is a loud boot error.
package consoleassets

import "io/fs"

// FS returns nil: this binary was built without -tags console_assets.
func FS() fs.FS { return nil }
