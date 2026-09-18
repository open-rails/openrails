//go:build integration

package merchantarchive

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestPaymentHostEventUUIDArchiveRoundTrip(t *testing.T) {
	source, target := archiveDB(t, "openrails"), archiveDB(t, "archive_host_uuid")
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	seedBook(t, source, id)
	ctx, release, err := source.WithMerchantConn(merchant.WithID(t.Context(), id))
	require.NoError(t, err)
	defer release()
	var customer, price, psp uuid.UUID
	require.NoError(t, source.Qx(ctx).QueryRow(ctx, `SELECT customer_id,price_id,psp_id FROM openrails.payments WHERE merchant_id=$1 AND transaction_id='charge-1'`, id.UUID()).Scan(&customer, &price, &psp))
	// Its first 16 digits pass Luhn, reproducing the false positive in the
	// payment:<UUID> key. The random suffix keeps repeated tests independent.
	paymentID := uuid.MustParse("41111111-1111-4115-a111-" + uuid.NewString()[24:])
	now := time.Now().UTC()
	require.NoError(t, payments.NewPaymentRepo(source).Create(ctx, &models.Payment{
		ID: paymentID, CustomerID: customer, PriceID: price, PspID: &psp,
		Rail: models.RailNMI, TransactionID: "uuid-regression", Amount: 1000000,
		ListAmount: 1000000, Currency: "USD", Status: "completed",
		MoneyMovement: models.MoneyMovementRail, PurchasedAt: now, CreatedAt: now,
	}))
	// The production insert trigger authored the key; only acknowledge delivery.
	var key string
	require.NoError(t, source.Qx(ctx).QueryRow(ctx, `UPDATE openrails.host_outbox SET delivered_at=$3 WHERE merchant_id=$1 AND payment_id=$2 RETURNING dedupe_key`, id.UUID(), paymentID, now).Scan(&key))
	require.Equal(t, "payment:"+paymentID.String(), key)
	release()
	var artifact bytes.Buffer
	err = Export(t.Context(), source, id, &artifact)
	if err != nil {
		var archiveErr *Error
		require.ErrorAs(t, err, &archiveErr)
		t.Fatalf("export failed: %v; cause: %v", err, archiveErr.Err)
	}
	wrongKey := "payment:10000000-0000-0000-0000-000000000002"
	_, err = Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, artifact.Bytes(), "host_outbox", "dedupe_key", &wrongKey)))
	require.Error(t, err, "an ordinary UUID cannot replace the payment's replay coordinate")
	assertEmptyBook(t, target, id)
	_, err = Restore(t.Context(), target, id, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	var restored bytes.Buffer
	require.NoError(t, Export(t.Context(), target, id, &restored))
	require.Equal(t, artifact.String(), restored.String())
}
