//go:build integration

package paymentmethods

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestCustodianIsAlwaysStated (or#880 phase 1) pins the custody axis:
// payment_methods.custodian records WHO HOLDS the instrument, independently of
// who charges it (rail + psp_id). "Stored at the processor" is the stated
// value 'psp', never an empty string — an unstated custodian must be
// impossible, not merely unusual.
func TestCustodianIsAlwaysStated(t *testing.T) {
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	database, err := db.NewWithPGXPool(pool, "openrails")
	require.NoError(t, err)
	ctx := dbtest.WithTestMerchant(context.Background())

	customerID, err := db.EnsureCustomerID(ctx, database.Qx(ctx), uuid.Nil, uuid.NewString())
	require.NoError(t, err)

	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailNMI))
	repo := NewPaymentMethodRepo(database)
	create := func(custodian string) uuid.UUID {
		pm := &models.PaymentMethod{
			ID:                   uuid.New(),
			CustomerID:           customerID,
			Rail:                 models.RailNMI,
			PspID:                pspID,
			RailCustomerRef:      "vault-" + uuid.NewString()[:8],
			RailMethodRef:        "bill-" + uuid.NewString()[:8],
			RebillDriver:         models.RebillDriverProvider,
			InitialTransactionID: "txn-" + uuid.NewString()[:8],
			Custodian:            custodian,
			CreatedAt:            time.Now().UTC(),
			UpdatedAt:            time.Now().UTC(),
		}
		require.NoError(t, repo.Create(ctx, pm))
		t.Cleanup(func() { _ = repo.Delete(ctx, pm.ID) })
		return pm.ID
	}
	readCustodian := func(id uuid.UUID) string {
		var got string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT custodian FROM openrails.payment_methods WHERE id = $1`, id).Scan(&got))
		return got
	}

	// A writer that says nothing about custody still lands a STATED fact: the
	// card is held by the processor that charges it.
	require.Equal(t, models.CustodianPSP, readCustodian(create("")))

	// An explicitly third-party-held instrument keeps its custodian; the rail
	// (who charges) is untouched by custody.
	btID := create(models.CustodianBasisTheory)
	require.Equal(t, models.CustodianBasisTheory, readCustodian(btID))
	var rail string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT rail FROM openrails.payment_methods WHERE id = $1`, btID).Scan(&rail))
	require.Equal(t, string(models.RailNMI), rail)

	// The model's round-trip carries custody back out.
	loaded, err := repo.GetByID(ctx, btID)
	require.NoError(t, err)
	require.Equal(t, models.CustodianBasisTheory, loaded.Custodian)

	// The constraint, not convention, is what makes an unstated custodian
	// impossible: a direct INSERT with '' is refused.
	rawInsert := func(custodian string) error {
		id := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO openrails.payment_methods
			   (id, merchant_id, customer_id, rail, psp_id, rail_customer_ref, rail_method_ref, initial_transaction_id, custodian)
			 VALUES ($1, $2, $3, 'nmi', $4, $5, $6, 'txn-x', $7)`,
			id, dbtest.TestMerchantID.UUID(), customerID, pspID,
			"vault-"+uuid.NewString()[:8], "bill-"+uuid.NewString()[:8], custodian)
		if err == nil {
			t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM openrails.payment_methods WHERE id = $1`, id) })
		}
		return err
	}
	require.ErrorContains(t, rawInsert(""), "payment_methods_custodian_check")
	// The vocabulary is closed too — a custodian we do not implement cannot
	// arrive silently on a money path (adding one is a migration).
	require.ErrorContains(t, rawInsert("hyperswitch"), "payment_methods_custodian_check")
	for _, ok := range models.Custodians() {
		require.NoError(t, rawInsert(ok), "declared custodian %q must be accepted", ok)
	}
}
