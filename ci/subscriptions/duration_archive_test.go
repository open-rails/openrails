//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
)

func TestDurationHistoricalArchiveRefusesOrderlessPayments(t *testing.T) {
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	client := w.client[embedded]
	events, err := client.ListHostEvents(t.Context(), billing.HostEventListParams{})
	require.NoError(t, err)
	for _, event := range events.Items {
		_, err := client.AcknowledgeHostEvents(t.Context(), []billing.HostEventID{event.ID})
		require.NoError(t, err)
	}
	w.settle()
	w.stop()
	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	var current bytes.Buffer
	require.NoError(t, merchantarchive.Export(t.Context(), source, client.MerchantID(), &current))

	// Reproduce the preceding archive profile: auto_renew instead of cadence,
	// no subscription access snapshot, and a historically open projection.
	profiles := map[string]contract.Profile{}
	for _, p := range contract.ProfilesFor(1) {
		profiles[p.Name] = p
	}
	legacy := legacyArchive(t, current.Bytes(), func(table string, values []*string) []*string {
		sourceType := ""
		for i, c := range profiles[table].Columns {
			if c.Name == "source_type" && values[i] != nil {
				sourceType = *values[i]
			}
		}
		for i, c := range profiles[table].Columns {
			switch {
			case table == "prices" && c.Name == "billing_interval_hours":
				values[i] = new(map[bool]string{true: "true", false: "false"}[values[i] != nil])
			case table == "subscriptions" && c.Name == "access_duration_hours_snapshot":
				return append(values[:i], values[i+1:]...)
			case table == "entitlements" && sourceType == "subscription" && c.Name == "ends_at":
				values[i] = nil
			}
		}
		return values
	})
	historical := bytes.NewBuffer(legacy)

	schema := "duration_archive_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	require.NoError(t, engine.Migrate(t.Context(), w.pool, openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}}))
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+quoted+".merchants(id,slug,status) VALUES($1,'historical-duration','active')", client.MerchantID().UUID())
	require.NoError(t, err)
	destination, err := db.NewWithPGXPool(w.pool, schema)
	require.NoError(t, err)
	before := e.providerAttempts()
	_, err = merchantarchive.Restore(t.Context(), destination, client.MerchantID(), bytes.NewReader(historical.Bytes()))
	var refusal *merchantarchive.Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "invalid_artifact", refusal.Code)
	require.ErrorContains(t, errors.Unwrap(err), "payments without order identity")
	for _, table := range []string{"products", "prices", "subscriptions", "payments", "grants"} {
		var rows int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+quoted+"."+table).Scan(&rows))
		require.Zero(t, rows, "refused archive must roll back every retained row")
	}
	require.Equal(t, before, e.providerAttempts(), "refusing historical terms never bills the provider")
}
