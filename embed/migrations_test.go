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
				err := ApplyMigrations(context.Background(), nil, MigrationOptions{Schema: tt.schema})
				require.ErrorContains(t, err, tt.want)
				return
			}
			_, err := validateMigrationSchema(tt.schema)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestRiverOwnershipDefaultsAndValidation(t *testing.T) {
	for _, ownership := range []RiverOwnership{{}, RiverManagedByOpenRails()} {
		schema, err := ownership.managedSchema("billing")
		require.NoError(t, err)
		require.Equal(t, "public", schema)
	}
	schema, err := RiverManagedByOpenRails("jobs").managedSchema("billing")
	require.NoError(t, err)
	require.Equal(t, "jobs", schema)
	for _, ownership := range []RiverOwnership{RiverManagedByOpenRails("billing"), RiverManagedByOpenRails("bad;sql"), RiverManagedByOpenRails("one", "two"), RiverFromHost(nil)} {
		_, err := ownership.managedSchema("billing")
		require.Error(t, err)
	}
}
