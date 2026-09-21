package migrate

import (
	"fmt"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

// Relocate schema references in our embedded SQL/PLpgSQL migrations, preserving
// domain strings and comments. The lexer distinguishes quoted values from identifiers; dollar-quoted
// AS/DO bodies contain code and are scanned recursively. No cgo is required.
func rewriteMigrationsSchema(migrations []migratekit.Migration, schema string) ([]migratekit.Migration, error) {
	if schema == "" {
		schema = config.DefaultSchema
	}
	if schema == config.CanonicalSchema {
		return migrations, nil
	}
	out := make([]migratekit.Migration, len(migrations))
	for i, mig := range migrations {
		content, err := postgresmigrations.RewriteSchema(mig.Content, schema)
		if err != nil {
			return nil, fmt.Errorf("relocate migration %s: %w", mig.Name, err)
		}
		mig.Content = content
		out[i] = mig
	}
	return out, nil
}
