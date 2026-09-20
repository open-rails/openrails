package embed

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyMigrationsValidatesInitializationInputs(t *testing.T) {
	tests := []struct {
		name   string
		pool   bool
		schema string
		want   string
	}{
		{name: "nil pool", schema: "openrails", want: "postgres pool is required"},
		{name: "invalid schema", pool: true, schema: `billing;DROP SCHEMA public`, want: "invalid database schema"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.pool {
				err := ApplyMigrations(context.Background(), nil, tt.schema)
				require.ErrorContains(t, err, tt.want)
				return
			}
			_, err := validateMigrationSchema(tt.schema)
			require.ErrorContains(t, err, tt.want)
		})
	}
}
