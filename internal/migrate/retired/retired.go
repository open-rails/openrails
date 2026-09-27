// Package retired holds OpenRails' retired PostgreSQL chain and the verified
// conversion from it.
//
// v0.153.0 folded 0002_creator_catalogs into a fresh 0001 and later releases
// grew that baseline. Databases created by v0.147.0–v0.152.x recorded the old
// 0001 and 0002, so the current chain would refuse them (key 2 now names a
// different file). migratekit converts them only when the ledger and the
// schema's shape both prove they are that chain, and commits only a result
// equal to a fresh current 0001.
package retired

import (
	"embed"

	"github.com/open-rails/migratekit"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

//go:embed *.sql
var files embed.FS

// Render renders migrations from fsys for a schema, relocating the canonical
// openrails qualifier the way the current chain is relocated.
func Render(fsys embed.FS, names ...string) migratekit.Render {
	return func(schema string) ([]migratekit.Migration, error) {
		out := make([]migratekit.Migration, 0, len(names))
		for _, name := range names {
			raw, err := fsys.ReadFile(name)
			if err != nil {
				return nil, err
			}
			content, err := postgresmigrations.RewriteSchema(string(raw), schema)
			if err != nil {
				return nil, err
			}
			out = append(out, migratekit.Migration{Name: name, Content: content})
		}
		return out, nil
	}
}

// Conversions returns every retired chain OpenRails converts from.
func Conversions() []migratekit.Conversion {
	return []migratekit.Conversion{{
		Name:     "openrails v0.147.0–v0.152.x baseline with creator catalogs",
		Retired:  Render(files, "0001_schema.up.sql", "0002_creator_catalogs.up.sql"),
		Replaces: 1,
		SQL: func(schema string) (string, error) {
			raw, err := files.ReadFile("convert_from_v0147.sql")
			if err != nil {
				return "", err
			}
			return postgresmigrations.RewriteSchema(string(raw), schema)
		},
		Fallback: "OpenRails v0.152.1, the last release of that chain",
	}}
}
