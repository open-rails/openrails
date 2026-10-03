package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
)

func TestMigrateValidatesInputsBeforeDatabase(t *testing.T) {
	require.ErrorContains(t, Migrate(context.Background(), nil, config.Config{}), "requires a Postgres pool")
	for _, schema := range []string{"", " Billing ", "_x9", strings.Repeat("a", 63)} {
		require.True(t, validIdentifier((&config.Config{Schema: schema}).SchemaName()), schema)
	}
	for _, schema := range []string{`billing;DROP SCHEMA public`, "9lives", "bill-ing", `"quoted"`, strings.Repeat("a", 64)} {
		require.False(t, validIdentifier((&config.Config{Schema: schema}).SchemaName()), schema)
	}
}
