// Package postgresmigrations embeds OpenRails' PostgreSQL migrations.
package postgresmigrations

import "embed"

//go:embed *.up.sql
var FS embed.FS
