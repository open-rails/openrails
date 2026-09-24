package embed

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	riverjobs "github.com/open-rails/openrails/internal/river"
)

// River decodes a job by kind into the worker's args type, so the public args
// must mirror the engine job field for field or host inserts silently drop data.
func TestInvoiceSweepArgsMirrorEngineInvoiceJob(t *testing.T) {
	public, engine := reflect.TypeOf(InvoiceSweepArgs{}), reflect.TypeOf(riverjobs.InvoiceArgs{})
	require.Equal(t, engine.NumField(), public.NumField())
	for i := range engine.NumField() {
		want := engine.Field(i)
		got, ok := public.FieldByName(want.Name)
		require.True(t, ok, want.Name)
		require.Equal(t, want.Type, got.Type, want.Name)
		require.Equal(t, want.Tag, got.Tag, want.Name)
	}
	require.Equal(t, riverjobs.InvoiceArgs{}.Kind(), InvoiceSweepArgs{}.Kind())
	require.Equal(t, QueueBilling, InvoiceSweepArgs{}.InsertOpts().Queue)
}

func TestRiverOwnershipSchema(t *testing.T) {
	for name, tc := range map[string]struct {
		ownership RiverOwnership
		want      string
	}{
		"zero value":             {RiverOwnership{}, "public"},
		"managed default":        {RiverManagedByOpenRails(), "public"},
		"managed custom":         {RiverManagedByOpenRails(" Jobs "), "jobs"},
		"managed shares billing": {RiverManagedByOpenRails("billing"), "billing"},
		"host owned":             {RiverFromHost(), ""},
	} {
		schema, err := tc.ownership.managedSchema("billing")
		require.NoError(t, err, name)
		require.Equal(t, tc.want, schema, name)
	}
	for _, ownership := range []RiverOwnership{RiverManagedByOpenRails("bad;sql"), RiverManagedByOpenRails("one", "two"), RiverManagedByOpenRails(strings.Repeat("a", 64))} {
		_, err := ownership.managedSchema("billing")
		require.Error(t, err)
	}
}

func TestMigrationInputsValidatedBeforeDatabase(t *testing.T) {
	require.ErrorContains(t, ApplyMigrations(context.Background(), nil, MigrationOptions{}), "postgres pool is required")
	for input, want := range map[string]string{"": "billing", " Billing ": "billing", "_x9": "_x9", strings.Repeat("a", 63): strings.Repeat("a", 63)} {
		got, err := validateMigrationSchema(input)
		require.NoError(t, err, input)
		require.Equal(t, want, got)
	}
	for _, input := range []string{`billing;DROP SCHEMA public`, "9lives", "bill-ing", `"quoted"`, strings.Repeat("a", 64)} {
		_, err := validateMigrationSchema(input)
		require.ErrorContains(t, err, "invalid database schema", input)
	}
}
