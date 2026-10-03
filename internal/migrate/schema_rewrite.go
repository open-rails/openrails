package migrate

import (
	"fmt"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/internal/sqlschema"
)

// Migrations are authored in config.DefaultSchema; sqlschema relocates them
// (migratekit's WithSchema rewrite is a plain word replace and would change
// literals).
func rewriteMigrationsSchema(migrations []migratekit.Migration, schema string) ([]migratekit.Migration, error) {
	out := make([]migratekit.Migration, len(migrations))
	for i, mig := range migrations {
		content, err := sqlschema.Rewrite(mig.Content, schema)
		if err != nil {
			return nil, fmt.Errorf("relocate migration %s: %w", mig.Name, err)
		}
		mig.Content = content
		out[i] = mig
	}
	return out, nil
}
