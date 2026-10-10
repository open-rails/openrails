//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
)

// A version 1 archive's per-key entitlement windows restore as product access,
// as the cutover converts a database; an archive whose conversion would change
// a customer's access needs the cutover's approval and is refused whole.
func TestLegacyArchiveConvertsToProductAccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		timed bool
	}{{"keys match the product", false}, {"mixed-duration grant", true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			client, buyer := w.client[embedded], w.newCustomer()
			product := w.giftProduct("asset:timed", "service:permanent")
			buyer.grant(product, nil, nil)
			w.settle()
			w.stop()
			mid, payer := client.MerchantID(), buyer.customerID().UUID()
			ctx := merchant.WithID(t.Context(), mid)
			start := w.clock.Now().UTC()
			source, err := db.NewWithPGXPool(w.pool, w.schema)
			require.NoError(t, err)
			var current bytes.Buffer
			require.NoError(t, merchantarchive.Export(ctx, source, mid, &current))
			windows := contract.LegacyEntitlements
			legacy := legacyArchive(t, current.Bytes(), func(table string, values []*string) []*string {
				if table != "entitlements" || !tc.timed {
					return values
				}
				var key *string
				for i, c := range windows.Columns {
					if c.Name == "entitlement" {
						key = values[i]
					}
				}
				for i, c := range windows.Columns {
					if c.Name == "ends_at" && *key == "asset:timed" {
						values[i] = new(start.Add(12 * time.Hour).Format("2006-01-02 15:04:05.999999-07"))
					}
				}
				return values
			})

			schema := "legacy_archive_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
			require.NoError(t, engine.Migrate(t.Context(), w.pool, openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}}))
			quoted := pgx.Identifier{schema}.Sanitize()
			t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
			_, err = w.pool.Exec(t.Context(), "INSERT INTO "+quoted+".merchants(id,slug,status) VALUES($1,'historical-entitlements','active')", mid.UUID())
			require.NoError(t, err)
			target, err := db.NewWithPGXPool(w.pool, schema)
			require.NoError(t, err)
			result, err := merchantarchive.Restore(ctx, target, mid, bytes.NewReader(legacy))
			if tc.timed {
				var refusal *merchantarchive.Error
				require.True(t, errors.As(err, &refusal), "%v %+v", err, result)
				require.Equal(t, "unsupported_state", refusal.Code)
				require.Equal(t, "entitlements", refusal.Table)
				require.EqualValues(t, 1, refusal.Count, "the timed key would be held indefinitely")
				var rows int
				require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+quoted+".products").Scan(&rows))
				require.Zero(t, rows, "a refused restore writes nothing")
				return
			}
			require.NoError(t, err)
			original, err := archivewire.Read(bytes.NewReader(legacy), nil, nil)
			require.NoError(t, err)
			require.Equal(t, original.Digest, result.Digest)
			access, err := target.Gen(ctx).ListProductAccessPage(ctx, gen.ListProductAccessPageParams{MerchantID: mid.UUID(), CustomerIds: []uuid.UUID{payer}, AtTime: start, FetchLimit: 10})
			require.NoError(t, err)
			require.Len(t, access, 1)
			require.Equal(t, product.UUID(), access[0].ProductID)
			require.Equal(t, "grant", access[0].SourceType)
			require.Nil(t, access[0].EndsAt)
			require.Equal(t, "migration", *access[0].GrantReason)
			for _, at := range []time.Time{start.Add(time.Hour), start.Add(1000 * time.Hour)} {
				held, err := target.Gen(ctx).CheckDerivedEntitlements(ctx, gen.CheckDerivedEntitlementsParams{
					MerchantID: mid.UUID(), CustomerIds: []uuid.UUID{payer}, AtTime: at, Entitlements: []string{"asset:timed", "service:permanent"}, RowLimit: 2,
				})
				require.NoError(t, err)
				require.Len(t, held, 2, "a held key is a row")
			}
			var live int
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+quoted+`.grants g WHERE g.kind = 'entitlement' AND g.event = 'grant'
				AND NOT EXISTS (SELECT 1 FROM `+quoted+".grants t WHERE t.supersedes_id = g.id)").Scan(&live))
			require.Zero(t, live, "the per-key grants are superseded")
			replayed, err := merchantarchive.Restore(ctx, target, mid, bytes.NewReader(legacy))
			require.NoError(t, err)
			require.True(t, replayed.Replayed)
			require.Equal(t, original.Digest, replayed.Digest)
		})
	}
}
