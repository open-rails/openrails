//go:build integration

package merchantarchive

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionGatewayCoordinatesSurviveRestore(t *testing.T) {
	source, target := archiveDB(t, "openrails"), archiveDB(t, "subscription_gateway")
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	seedBook(t, source, id)
	ctx := merchant.WithID(t.Context(), id)
	// Checkout writes these fields on the normal NMI subscription path.
	metadata := `{"order_id":"signup-replay-coordinate","provider_transaction_id":"charge-1","delayed_start":"2027-01-01T00:00:00Z","e2e_run_id":"run-1","admin_notes":"retain billing notes"}`
	require.NoError(t, source.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE openrails.subscriptions SET gateway_response=$2::jsonb WHERE merchant_id=$1`, id.UUID(), metadata)
		return err
	}))
	count := func(d *db.DB) int {
		n := 0
		require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM openrails.subscriptions WHERE merchant_id=$1 AND gateway_response->>'order_id'='signup-replay-coordinate' AND gateway_response->>'provider_transaction_id'='charge-1' AND gateway_response=$2::jsonb`, id.UUID(), metadata).Scan(&n)
		}))
		return n
	}
	require.Equal(t, 1, count(source))
	var artifact bytes.Buffer
	require.NoError(t, Export(t.Context(), source, id, &artifact))
	_, err := Restore(t.Context(), target, id, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.Equal(t, 1, count(target), "NMI webhook/refund correlation and replay metadata must survive restore")
}

func TestSubscriptionGatewayUnknownMetadataRefuses(t *testing.T) {
	source, target := archiveDB(t, "openrails"), archiveDB(t, "subscription_unsafe")
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	seedBook(t, source, id)
	var artifact bytes.Buffer
	require.NoError(t, Export(t.Context(), source, id, &artifact))
	for _, metadata := range []string{
		`{"order_id":"retained","unknown":"must-refuse"}`,
		`{"order_id":"retained","raw_body":{"secret":"must-refuse"}}`,
		`{"provider_transaction_id":"sk_live_must-refuse"}`,
	} {
		_, err := Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, artifact.Bytes(), "subscriptions", "gateway_response", &metadata)))
		require.Error(t, err)
		assertEmptyBook(t, target, id)
		require.NoError(t, source.MerchantTx(merchant.WithID(t.Context(), id), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE openrails.subscriptions SET gateway_response=$2::jsonb WHERE merchant_id=$1`, id.UUID(), metadata)
			return err
		}))
		err = Export(t.Context(), source, id, io.Discard)
		require.Error(t, err)
		var archiveErr *Error
		require.ErrorAs(t, err, &archiveErr)
		require.Equal(t, "unsupported_state", archiveErr.Code)
		require.Equal(t, "subscriptions", archiveErr.Table)
	}
}

func TestRestoreRejectsInconsistentLiveSubscriptionTier(t *testing.T) {
	source, target := archiveDB(t, "openrails"), archiveDB(t, "subscription_tier")
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	seedBook(t, source, id)
	var artifact bytes.Buffer
	require.NoError(t, Export(t.Context(), source, id, &artifact))
	group := "unrelated-tier"
	for _, status := range []string{"active", "pending", "past_due", "unknown", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			modified := alteredArchive(t, artifact.Bytes(), "products", "tier_group", &group)
			modified = alteredArchive(t, modified, "subscriptions", "status", &status)
			if status == "cancelled" {
				// Ended billing still guards the product while paid access remains.
				future := "9999-01-01 00:00:00+00"
				modified = alteredArchive(t, modified, "subscriptions", "current_period_ends_at", &future)
				cancelled := "2026-01-15 00:00:00+00"
				modified = alteredArchive(t, modified, "subscriptions", "cancelled_at", &cancelled)
				cancelType := "user"
				modified = alteredArchive(t, modified, "subscriptions", "cancel_type", &cancelType)
			}
			_, err := Restore(t.Context(), target, id, bytes.NewReader(modified))
			require.Error(t, err, "a valid digest must not bypass the live tier invariant")
			var archiveErr *Error
			require.ErrorAs(t, err, &archiveErr)
			require.Equal(t, "subscriptions", archiveErr.Table)
			assertEmptyBook(t, target, id)
		})
	}
	_, err := Restore(t.Context(), target, id, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err, "a rejected artifact must leave the destination available for a corrected retry")
}

func TestRestorePreservesEndedHistoricalSubscriptionTier(t *testing.T) {
	source, target := archiveDB(t, "openrails"), archiveDB(t, "subscription_history")
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	seedBook(t, source, id)
	ctx := merchant.WithID(t.Context(), id)
	// Ordinary cancellation then product regrouping leaves the ended row's
	// historical tier unchanged; restore must not rewrite that valid history.
	require.NoError(t, source.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE openrails.subscriptions SET status='cancelled',cancel_type='user',cancelled_at='2026-01-15',ended_at='2026-02-01' WHERE merchant_id=$1`, id.UUID())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE openrails.products SET tier_group='new-tier' WHERE merchant_id=$1`, id.UUID())
		return err
	}))
	var artifact bytes.Buffer
	require.NoError(t, Export(t.Context(), source, id, &artifact))
	_, err := Restore(t.Context(), target, id, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	var restored bytes.Buffer
	require.NoError(t, Export(t.Context(), target, id, &restored))
	require.Equal(t, artifact.String(), restored.String())
}
