//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
)

func TestDurationHistoricalArchiveRetainsBillingAndPaidGrants(t *testing.T) {
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
	for _, p := range contract.Profiles {
		profiles[p.Name] = p
	}
	var historical bytes.Buffer
	var writer *archivewire.Writer
	var table string
	_, err = archivewire.Read(bytes.NewReader(current.Bytes()), func(h archivewire.Header) error {
		var err error
		writer, err = archivewire.NewWriter(&historical, h.MerchantID, h.CatalogRevision)
		return err
	}, func(record archivewire.Record) error {
		if record.Kind == "table" {
			table = record.Table
			return writer.Table(table)
		}
		values := append([]*string(nil), record.Values...)
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
				values = append(values[:i], values[i+1:]...)
				return writer.Row(values)
			case table == "entitlements" && sourceType == "subscription" && c.Name == "ends_at":
				values[i] = nil
			}
		}
		return writer.Row(values)
	})
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	schema := "duration_archive_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	require.NoError(t, openrails.Migrate(t.Context(), w.pool, openrails.Config{Schema: schema, RiverSchema: schema}))
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+quoted+".merchants(id,slug,status) VALUES($1,'historical-duration','active')", client.MerchantID().UUID())
	require.NoError(t, err)
	destination, err := db.NewWithPGXPool(w.pool, schema)
	require.NoError(t, err)
	before := e.providerAttempts()
	result, err := merchantarchive.Restore(t.Context(), destination, client.MerchantID(), bytes.NewReader(historical.Bytes()))
	require.NoError(t, err)
	require.False(t, result.Replayed)
	var cadence, access int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT p.billing_interval_hours,s.access_duration_hours_snapshot FROM "+quoted+".subscriptions s JOIN "+quoted+".prices p ON p.id=s.price_id WHERE s.id=$1", e.sub.UUID()).Scan(&cadence, &access))
	require.Equal(t, 720, cadence)
	require.Equal(t, 720, access)
	var finite bool
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT bool_and(ends_at IS NOT NULL) FROM "+quoted+".entitlements WHERE source_type='subscription' AND source_id=$1", e.sub.UUID()).Scan(&finite))
	require.True(t, finite, "old open projections recover their paid grant bounds")
	var changed int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM (SELECT id,starts_at,ends_at FROM "+pgx.Identifier{w.schema}.Sanitize()+".grants EXCEPT SELECT id,starts_at,ends_at FROM "+quoted+".grants) changed").Scan(&changed))
	require.Zero(t, changed, "the immutable paid ledger is preserved exactly")
	replay, err := merchantarchive.Restore(t.Context(), destination, client.MerchantID(), bytes.NewReader(historical.Bytes()))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, result.Digest, replay.Digest)
	require.Equal(t, before, e.providerAttempts(), "restoring historical terms never bills the provider")
}
