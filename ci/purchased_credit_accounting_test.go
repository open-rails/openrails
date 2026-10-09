//go:build e2e && integration

package ci_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
	"github.com/stretchr/testify/require"
)

// Source-level financial proof: actual proceeds, promotional funding, isolated
// FIFO lot reversals and once-only posting all run against real PostgreSQL.
func TestPurchasedCreditAccounting(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "credit-accounting-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	database, err := db.NewWithPGXPool(f.pool, f.schema)
	require.NoError(t, err)
	customer := billing.CustomerID(uuid.New())
	_, err = client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
	require.NoError(t, err)
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "credit-accounting", DisplayName: "API credit"})
	require.NoError(t, err)
	price, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "pack", Currency: "USD", UnitAmount: 80_000_000})
	require.NoError(t, err)
	psp, err := client.DeclarePSP(ctx, client.MerchantID(), billing.PSPDeclaration{Key: "credit-stripe", Rail: "stripe", AccountID: "acct_credit_test"})
	require.NoError(t, err)
	start := time.Now().UTC().Truncate(time.Microsecond)
	expiry := start.Add(365 * 24 * time.Hour)
	newPayment := func(amount int64) *models.Payment {
		p := &models.Payment{ID: uuid.New(), CustomerID: customer.UUID(), PriceID: price.ID.UUID(), Channel: models.ChannelRail, Rail: "stripe", PspID: new(psp.ID.UUID()),
			TransactionID: uuid.NewString(), Amount: amount, ListAmount: amount, Currency: "USD", Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: start, CreatedAt: start}
		require.NoError(t, payments.NewPaymentService(database).Create(ctx, p))
		return p
	}
	payment := newPayment(80_000_000)
	params := purchasedcredits.Params{CustomerID: identity.CustomerID(customer), PaymentID: payment.ID, ProductID: product.ID.UUID(), Currency: "USD", Amount: 100_000_000, PaidAmount: 80_000_000, StartsAt: start, ExpiresAt: &expiry}
	svc := purchasedcredits.New(database)
	paymentService := payments.NewPaymentService(database)
	var cumulative int64
	var lastReversal *models.Payment
	syncRefund := func(total int64) error {
		if total == cumulative {
			return purchasedcredits.New(database).ApplyReversal(ctx, payment.ID, lastReversal.ID, nil)
		}
		if total > cumulative {
			var err error
			lastReversal, err = paymentService.Refund(ctx, payment.ID, uuid.NewString(), total-cumulative, payments.ReversalChargeback)
			if err != nil {
				return err
			}
		} else {
			recovery := &models.Payment{ID: uuid.New(), CustomerID: customer.UUID(), PriceID: price.ID.UUID(), Channel: models.ChannelRail, Rail: "stripe", PspID: new(psp.ID.UUID()), TransactionID: uuid.NewString(), Amount: cumulative - total, ListAmount: payment.Amount, Currency: "USD", Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: start, CreatedAt: start, RefundedPaymentID: &payment.ID, ReversalKind: new(payments.ReversalDisputeReversal), Metadata: map[string]any{"reverses_payment_id": lastReversal.ID.String()}}
			if err := paymentService.Create(ctx, recovery); err != nil {
				return err
			}
		}
		cumulative = total
		return nil
	}

	const duplicates = 8
	var wg sync.WaitGroup
	ids := make(chan uuid.UUID, duplicates)
	errs := make(chan error, duplicates)
	for range duplicates {
		wg.Go(func() { id, err := svc.Fund(ctx, params); ids <- id; errs <- err })
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var grantID uuid.UUID
	for id := range ids {
		if grantID == uuid.Nil {
			grantID = id
		}
		require.Equal(t, grantID, id)
	}
	assertBalance := func(want int64) {
		bal, err := client.GetBalance(ctx, customer, "USD")
		require.NoError(t, err)
		require.Equal(t, want, bal.BalanceAmount)
	}
	accountBalance := func(account string) int64 {
		var amount int64
		err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT (credits_posted-debits_posted)::bigint FROM billing.ledger_accounts WHERE merchant_id=$1 AND account_type=$2 AND currency='USD'", client.MerchantID().UUID(), account).Scan(&amount)
		})
		require.NoError(t, err)
		return amount
	}
	assertBalance(100_000_000)
	require.EqualValues(t, -80_000_000, accountBalance("processor_clearing"))
	require.EqualValues(t, -20_000_000, accountBalance("promotional_funding"))
	changed := params
	changed.Amount++
	_, err = svc.Fund(ctx, changed)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	// A rollback removes the new lot and every accounting leg together.
	rollbackPayment := newPayment(80_000_000)
	rolled := params
	rolled.PaymentID = rollbackPayment.ID
	stop := errors.New("rollback requested")
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := purchasedcredits.New(database.NewWithPgxTx(tx)).Fund(ctx, rolled)
		if err != nil {
			return err
		}
		return stop
	})
	require.ErrorIs(t, err, stop)
	assertBalance(100_000_000)
	// Half the actual cash returns half the face value, including its bonus.
	require.NoError(t, syncRefund(40_000_000))
	require.NoError(t, syncRefund(40_000_000))
	assertBalance(50_000_000)
	require.EqualValues(t, -40_000_000, accountBalance("processor_clearing"))
	require.EqualValues(t, -10_000_000, accountBalance("promotional_funding"))
	require.NoError(t, syncRefund(0))
	assertBalance(100_000_000)
	// Distinct cash payments receive distinct lots. A forced reversal may remove
	// its source lot, never another purchase's remaining credits.
	second := newPayment(200_000_000)
	other := params
	other.PaymentID = second.ID
	other.Amount = 200_000_000
	other.PaidAmount = 200_000_000
	secondGrant, err := svc.Fund(ctx, other)
	require.NoError(t, err)
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "api.call", Amount: 60_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	require.ErrorIs(t, svc.ValidateRefund(ctx, payment.ID, 80_000_000), billing.ErrConflict)
	require.NoError(t, syncRefund(80_000_000))
	assertBalance(200_000_000)
	require.EqualValues(t, -60_000_000, accountBalance("credit_refund_loss"))
	require.EqualValues(t, 0, accountBalance("credit_refund_clearing"))
	require.NoError(t, syncRefund(0))
	assertBalance(240_000_000) // Restore only the 40 that were withdrawn, not the 60 consumed.
	require.EqualValues(t, 0, accountBalance("credit_refund_loss"))
	// Replaying funding after reversals still returns the same historical lot.
	id, err := svc.Fund(ctx, params)
	require.NoError(t, err)
	require.Equal(t, grantID, id)
	assertBalance(240_000_000)
	// Pending cash refunds reserve the face value against new service usage.
	reserved, err := paymentService.ReserveRefund(ctx, second.ID, uuid.NewString(), 100_000_000, nil)
	require.NoError(t, err)
	bal, err := client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 100_000_000, bal.HeldAmount)
	require.EqualValues(t, 140_000_000, bal.AvailableAmount)
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "api.call", Amount: 150_000_000, Source: "test", SourceID: uuid.NewString()})
	require.Error(t, err, "an ordinary spend cannot consume pending refund funds")
	require.NoError(t, paymentService.MarkFailed(ctx, reserved.ID))
	bal, err = client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.Zero(t, bal.HeldAmount)
	deadline := time.Now().Add(time.Hour)
	requestID := uuid.NewString()
	admitted, err := client.Admit(ctx, []billing.AdmitParams{{RequestID: requestID, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 200_000_000, ExpiresAt: &deadline}})
	require.NoError(t, err)
	require.True(t, admitted[0].Allowed())
	require.ErrorIs(t, svc.ValidateRefund(ctx, second.ID, 100_000_000), billing.ErrConflict, "voluntary refunds cannot consume previously authorized work")
	_, err = client.ReleaseAdmission(ctx, requestID)
	require.NoError(t, err)
	// Every maintained account counter still matches immutable transfers.
	require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		remaining, err := q.GetCreditLotRemaining(ctx, gen.GetCreditLotRemainingParams{MerchantID: client.MerchantID().UUID(), GrantID: grantID})
		if err != nil {
			return err
		}
		require.EqualValues(t, 40_000_000, remaining)
		return nil
	}))
	// An unrelated merchant revocation must survive a later dispute recovery.
	require.NoError(t, syncRefund(20_000_000))
	_, err = client.RevokeCreditGrant(ctx, customer, billing.CreditGrantID(grantID), billing.RevokeCreditGrantParams{Reason: "separate merchant revocation"})
	require.NoError(t, err)
	require.NoError(t, syncRefund(0))
	assertBalance(200_000_000)
	id, err = svc.Fund(ctx, params)
	require.NoError(t, err)
	require.Equal(t, grantID, id)
	assertBalance(200_000_000)
	// A pending refund protects its own lot from FIFO spending even when another
	// lot makes the customer's aggregate available balance sufficient.
	third := newPayment(100_000_000)
	thirdTerms := params
	thirdTerms.PaymentID, thirdTerms.Amount, thirdTerms.PaidAmount = third.ID, 100_000_000, 100_000_000
	_, err = svc.Fund(ctx, thirdTerms)
	require.NoError(t, err)
	reserved, err = paymentService.ReserveRefund(ctx, second.ID, uuid.NewString(), 200_000_000, nil)
	require.NoError(t, err)
	_, err = client.RevokeCreditGrant(ctx, customer, billing.CreditGrantID(secondGrant), billing.RevokeCreditGrantParams{Reason: "cannot revoke the cash refund reservation"})
	require.Error(t, err, "another available lot cannot make the reserved source revocable")
	require.ErrorIs(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		ledger := grants.New(gen.New(tx), client.MerchantID().UUID())
		ledger.SetClock(func() time.Time { return expiry.Add(time.Hour) })
		_, err := ledger.ExpireLapsed(ctx, customer.UUID(), "USD")
		if err != nil {
			return err
		}
		remaining, err := gen.New(tx).GetCreditLotRemaining(ctx, gen.GetCreditLotRemainingParams{MerchantID: client.MerchantID().UUID(), GrantID: secondGrant})
		if err != nil {
			return err
		}
		require.EqualValues(t, 0, remaining, "expiry retires the lot; its pending cash refund will use the same retired source")
		return stop
	}), stop)
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "api.call", Amount: 100_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	_, err = paymentService.CompleteRefundReservation(ctx, reserved.ID, uuid.NewString(), nil)
	require.NoError(t, err)
	assertBalance(0)
	require.EqualValues(t, 0, accountBalance("credit_refund_loss"), "voluntary refund must not consume a different lot and invent merchant loss")
	// A dispute win cannot restore credit removed by a different refund. The
	// chargeback below recovered no unused credit, so winning it restores none.
	fourth := newPayment(100_000_000)
	fourthTerms := thirdTerms
	fourthTerms.PaymentID = fourth.ID
	_, err = svc.Fund(ctx, fourthTerms)
	require.NoError(t, err)
	_, err = paymentService.Refund(ctx, fourth.ID, uuid.NewString(), 50_000_000, payments.ReversalRefund)
	require.NoError(t, err)
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "api.call", Amount: 50_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	chargeback, err := paymentService.Refund(ctx, fourth.ID, uuid.NewString(), 50_000_000, payments.ReversalChargeback)
	require.NoError(t, err)
	recovery := &models.Payment{ID: uuid.New(), CustomerID: customer.UUID(), PriceID: price.ID.UUID(), Channel: models.ChannelRail, Rail: "stripe", PspID: new(psp.ID.UUID()), TransactionID: uuid.NewString(), Amount: 50_000_000, ListAmount: fourth.Amount, Currency: "USD", Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: start, CreatedAt: start, RefundedPaymentID: &fourth.ID, ReversalKind: new(payments.ReversalDisputeReversal), Metadata: map[string]any{"reverses_payment_id": chargeback.ID.String()}}
	require.NoError(t, paymentService.Create(ctx, recovery))
	assertBalance(0)
	require.EqualValues(t, 0, accountBalance("credit_refund_loss"))
	for _, retiredAs := range []string{"revoked_credits", "expired_credits"} {
		retiredPayment := newPayment(100_000_000)
		retiredTerms := thirdTerms
		retiredTerms.PaymentID = retiredPayment.ID
		retiredGrant, err := svc.Fund(ctx, retiredTerms)
		require.NoError(t, err)
		if retiredAs == "revoked_credits" {
			_, err = client.RevokeCreditGrant(ctx, customer, billing.CreditGrantID(retiredGrant), billing.RevokeCreditGrantParams{Reason: "retired credit refund test"})
			require.NoError(t, err)
		} else {
			require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				ledger := grants.New(gen.New(tx), client.MerchantID().UUID())
				ledger.SetClock(func() time.Time { return expiry.Add(time.Hour) })
				_, err := ledger.ExpireLapsed(ctx, customer.UUID(), "USD")
				return err
			}))
		}
		frozen := accountBalance(retiredAs)
		refunded, err := paymentService.Refund(ctx, retiredPayment.ID, uuid.NewString(), 100_000_000, payments.ReversalChargeback)
		require.NoError(t, err)
		require.EqualValues(t, frozen-100_000_000, accountBalance(retiredAs))
		require.EqualValues(t, 0, accountBalance("credit_refund_loss"), "retired value is available in its source account, not consumed loss")
		returned := &models.Payment{ID: uuid.New(), CustomerID: customer.UUID(), PriceID: price.ID.UUID(), Channel: models.ChannelRail, Rail: "stripe", PspID: new(psp.ID.UUID()), TransactionID: uuid.NewString(), Amount: 100_000_000, ListAmount: retiredPayment.Amount, Currency: "USD", Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: start, CreatedAt: start, RefundedPaymentID: &retiredPayment.ID, ReversalKind: new(payments.ReversalDisputeReversal), Metadata: map[string]any{"reverses_payment_id": refunded.ID.String()}}
		require.NoError(t, paymentService.Create(ctx, returned))
		require.EqualValues(t, frozen, accountBalance(retiredAs))
		assertBalance(0)
	}
	for _, scenario := range []struct{ sweepFirst, release bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		expiringPayment := newPayment(100_000_000)
		expiringTerms := thirdTerms
		expiringTerms.PaymentID = expiringPayment.ID
		_, err := svc.Fund(ctx, expiringTerms)
		require.NoError(t, err)
		pending, err := paymentService.ReserveRefund(ctx, expiringPayment.ID, uuid.NewString(), 50_000_000, nil)
		require.NoError(t, err)
		afterExpiry := expiry.Add(time.Hour)
		readCapacity := func() {
			require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				q := gen.New(tx)
				capacity, err := q.GetAdmissionCapacity(ctx, gen.GetAdmissionCapacityParams{MerchantID: client.MerchantID().UUID(), CustomerID: customer.UUID(), Currency: "USD", AsOf: afterExpiry, HeldSince: afterExpiry.Add(-30 * 24 * time.Hour)})
				if err != nil {
					return err
				}
				held, err := q.GetFinancialHeldAmount(ctx, gen.GetFinancialHeldAmountParams{MerchantID: client.MerchantID().UUID(), CustomerID: customer.UUID(), Currency: "USD", AsOf: afterExpiry, HeldSince: afterExpiry.Add(-30 * 24 * time.Hour)})
				if err != nil {
					return err
				}
				require.EqualValues(t, 0, capacity.Balance-capacity.Held, "the expired unrefunded half must never authorize new work")
				require.Equal(t, capacity.Held, held, "balance and admission reserve the same unavailable amount")
				return nil
			}))
		}
		if scenario.sweepFirst {
			require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				ledger := grants.New(gen.New(tx), client.MerchantID().UUID())
				ledger.SetClock(func() time.Time { return afterExpiry })
				_, err := ledger.ExpireLapsed(ctx, customer.UUID(), "USD")
				return err
			}))
		}
		readCapacity()
		settlement := payments.NewPaymentService(database, clockwork.NewFakeClockAt(afterExpiry))
		if scenario.release {
			err = settlement.MarkFailed(ctx, pending.ID)
		} else {
			_, err = settlement.CompleteRefundReservation(ctx, pending.ID, uuid.NewString(), nil)
		}
		require.NoError(t, err)
		readCapacity() // Immediately after completion/release; no subsequent expiry sweep.
		assertBalance(0)
		require.EqualValues(t, 0, accountBalance("credit_refund_loss"))
	}
	// Imported/manual legacy lots can share a payment. Native once-per-payment
	// enforcement is deliberately scoped to the new paid_amount provenance.
	require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		ledger := grants.New(gen.New(tx), client.MerchantID().UUID())
		for range 2 {
			_, err := ledger.Grant(ctx, grants.GrantInput{Customer: customer.UUID(), Product: new(product.ID.UUID()), Kind: grants.Credit, Source: grants.Admin, SourceID: uuid.NewString(), Payment: &fourth.ID, StartsAt: start, Amount: new(int64(1)), Currency: new("USD"), Spec: &grants.Spec{Deposit: &grants.DepositProvenance{Source: "legacy-import"}}})
			if err != nil {
				return err
			}
		}
		return nil
	}))
}
