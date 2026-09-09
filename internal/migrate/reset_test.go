package migrate

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestCanonicalResetTarget(t *testing.T) {
	t.Parallel()

	config, err := pgx.ParseConfig("postgresql://user@db.internal:5432/app")
	require.NoError(t, err)
	config.Host = " DB.Internal "
	require.Equal(t, "db.internal:5432/app", canonicalResetTarget(config))

	config.Host = "2001:db8::1"
	config.Port = 5433
	config.Database = "app/test"
	require.Equal(t, "[2001:db8::1]:5433/app%2Ftest", canonicalResetTarget(config))
}

func TestValidateResetConnectionConfigRejectsAnotherFallbackHost(t *testing.T) {
	t.Parallel()

	config, err := pgx.ParseConfig("host=dev.internal,prod.internal port=5432 dbname=app sslmode=disable")
	require.NoError(t, err)
	require.ErrorContains(t, validateResetConnectionConfig(config), "multiple host targets")

	config, err = pgx.ParseConfig("host=dev.internal dbname=app sslmode=prefer")
	require.NoError(t, err)
	require.NoError(t, validateResetConnectionConfig(config), "TLS fallback on the same host is safe")
}

func TestValidateResetAuthorizationUsesExactIdentityNotNameHeuristics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		target       string
		allowed      string
		confirmation string
		wantError    string
	}{
		{
			name:         "ordinary production name is refused when not allow-listed",
			target:       "db.internal:5432/app",
			confirmation: "reset-openrails@db.internal:5432/app",
			wantError:    "not allow-listed",
		},
		{
			name:         "harmless name containing prod is allowed by exact identity",
			target:       "localhost:5432/productivity",
			allowed:      "localhost:5432/productivity",
			confirmation: "reset-openrails@localhost:5432/productivity",
		},
		{
			name:         "same database on another host is refused",
			target:       "prod.internal:5432/app",
			allowed:      "dev.internal:5432/app",
			confirmation: "reset-openrails@prod.internal:5432/app",
			wantError:    "not allow-listed",
		},
		{
			name:         "wrong confirmation is refused after exact allow-list match",
			target:       "localhost:5432/app",
			allowed:      "localhost:5432/app",
			confirmation: "reset-openrails@localhost:5432/other",
			wantError:    "does not match",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateResetAuthorization(tt.target, tt.allowed, tt.confirmation)
			if tt.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}
