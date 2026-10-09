//go:build e2e && integration

package ci_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
)

// A provider bill above the hold leaves a prepaid customer owing. No new hold
// is granted against that debt, and the customer's next funding repays it
// first: exactly, once, from cash value only, through the invoice that claims
// it, and back to owed when the funding payment is charged back.
func TestPrepaidOverdraftRepaidByNextFunding(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "owed-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	database, err := db.NewWithPGXPool(f.pool, f.schema)
	require.NoError(t, err)
	paymentService := payments.NewPaymentService(database)

	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "owed-credit", DisplayName: "Credit"})
	require.NoError(t, err)
	price, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "pack", Currency: "USD", UnitAmount: 20_000_000})
	require.NoError(t, err)
	psp, err := client.DeclarePSP(ctx, client.MerchantID(), billing.PSPDeclaration{Key: "owed-stripe", Rail: "stripe", AccountID: "acct_owed_test"})
	require.NoError(t, err)

	newCustomer := func() billing.CustomerID {
		customer := billing.CustomerID(uuid.New())
		_, err := client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
		require.NoError(t, err)
		return customer
	}
	grant := func(customer billing.CustomerID, amount int64, sourceID string) *billing.CreditGrant {
		g, err := client.CreateCreditGrant(ctx, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: amount, Source: "support", SourceID: sourceID})
		require.NoError(t, err)
		return g
	}
	open := func(customer billing.CustomerID, operationID string, amount int64) error {
		body := []byte(`{"rental":"` + operationID + `"}`)
		_, err := client.OpenOperationAuthorization(ctx, billing.OpenOperationAuthorizationParams{
			OperationID: operationID, CustomerID: customer, RecordOwner: "user:1", Currency: "USD", Amount: amount,
			ClaimReference: "claim:" + operationID, AuthorizationBody: body, AuthorizationBodySHA256: billing.SHA256(sha256.Sum256(body)),
		})
		return err
	}
	balance := func(customer billing.CustomerID) *billing.Balance {
		bal, err := client.GetBalance(ctx, customer, "USD")
		require.NoError(t, err)
		return bal
	}
	// overdraw leaves the customer owing `owed` above a fully spent `held`.
	overdraw := func(customer billing.CustomerID, operationID string, held, owed int64) {
		grant(customer, held, operationID+"-seed")
		require.NoError(t, open(customer, operationID, held))
		settleProviderCost(t, ctx, database, client, operationID, held+owed)
		bal := balance(customer)
		require.EqualValues(t, 0, bal.BalanceAmount)
		require.EqualValues(t, owed, bal.OwedAmount)
		require.Equal(t, billing.BillingModePrepaid, bal.BillingMode, "an involuntary overdraft keeps the customer prepaid")
	}
	purchase := func(customer billing.CustomerID, face, paid int64) (*models.Payment, purchasedcredits.Params) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		p := &models.Payment{ID: uuid.New(), CustomerID: customer.UUID(), PriceID: price.ID.UUID(), Channel: models.ChannelRail, Rail: "stripe", PspID: new(psp.ID.UUID()),
			TransactionID: uuid.NewString(), Amount: paid, ListAmount: paid, Currency: "USD", Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now, CreatedAt: now}
		require.NoError(t, paymentService.Create(ctx, p))
		params := purchasedcredits.Params{CustomerID: identity.CustomerID(customer), PaymentID: p.ID, ProductID: product.ID.UUID(), Currency: "USD", Amount: face, PaidAmount: paid, StartsAt: now}
		_, err := purchasedcredits.New(database).Fund(ctx, params)
		require.NoError(t, err)
		return p, params
	}
	repayments := func(customer billing.CustomerID) []billing.CreditTransaction {
		page, err := client.ListCreditTransactions(ctx, customer, billing.CreditTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		var out []billing.CreditTransaction
		for _, tx := range page.Items {
			if tx.Type == billing.CreditOwedRepayment {
				out = append(out, tx)
			}
		}
		return out
	}

	t.Run("funding repays exactly once, cash value only", func(t *testing.T) {
		customer := newCustomer()
		overdraw(customer, "od-exact", 1_000_000, 300_000)
		require.ErrorIs(t, open(customer, "od-exact-next", 1), billing.ErrInsufficientCredits)

		small := grant(customer, 100_000, "small")
		bal := balance(customer)
		require.EqualValues(t, 0, bal.BalanceAmount)
		require.EqualValues(t, 200_000, bal.OwedAmount, "a smaller funding leaves the rest owed")
		replayed := grant(customer, 100_000, "small")
		require.True(t, replayed.Replayed)
		require.EqualValues(t, 200_000, balance(customer).OwedAmount)
		got := repayments(customer)
		require.Len(t, got, 1)
		require.EqualValues(t, -100_000, got[0].Amount)
		require.Equal(t, small.ID, *got[0].CreditGrantID)

		// 0.15 USD of credit for 0.05 USD: only the 0.05 paid repays debt.
		purchase(customer, 150_000, 50_000)
		bal = balance(customer)
		require.EqualValues(t, 150_000, bal.OwedAmount)
		require.EqualValues(t, 100_000, bal.BalanceAmount)
		require.ErrorIs(t, open(customer, "od-exact-bonus", 1), billing.ErrInsufficientCredits, "owed exceeds the balance")

		_, params := purchase(customer, 20_000_000, 20_000_000)
		bal = balance(customer)
		require.EqualValues(t, 0, bal.OwedAmount)
		require.EqualValues(t, 19_950_000, bal.BalanceAmount)
		_, err := purchasedcredits.New(database).Fund(ctx, params)
		require.NoError(t, err)
		require.EqualValues(t, 19_950_000, balance(customer).BalanceAmount, "a replayed funding repays nothing again")
		require.Len(t, repayments(customer), 3)
		require.NoError(t, open(customer, "od-exact-after", 19_950_000))
	})

	t.Run("prepaid holds and admissions exclude money owed", func(t *testing.T) {
		customer := newCustomer()
		grant(customer, 2_000_000, "seed")
		require.NoError(t, open(customer, "od-cap-a", 1_000_000))
		require.NoError(t, open(customer, "od-cap-b", 1_000_000))
		settleProviderCost(t, ctx, database, client, "od-cap-a", 1_500_000)
		_, err := client.ReleaseOperationAuthorization(ctx, billing.ReleaseOperationAuthorizationParams{OperationID: "od-cap-b", ReleaseReference: "never-created:b"})
		require.NoError(t, err)
		bal := balance(customer)
		require.EqualValues(t, 1_000_000, bal.BalanceAmount)
		require.EqualValues(t, 500_000, bal.OwedAmount)

		deadline := time.Now().Add(time.Hour)
		admit := func(id string, amount int64) billing.AdmissionVerdict {
			v, err := client.Admit(ctx, []billing.AdmitParams{{RequestID: id, CustomerID: customer, Invoker: customer.String(),
				InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: amount, ExpiresAt: &deadline}})
			require.NoError(t, err)
			return v[0]
		}
		require.False(t, admit("od-cap-admit-over", 500_001).Allowed())
		require.True(t, admit("od-cap-admit", 500_000).Allowed())
		_, err = client.ReleaseAdmission(ctx, "od-cap-admit")
		require.NoError(t, err)

		require.ErrorIs(t, open(customer, "od-cap-over", 500_001), billing.ErrInsufficientCredits)
		require.NoError(t, open(customer, "od-cap-fit", 500_000))
		_, err = client.ExtendOperationAuthorization(ctx, billing.ExtendOperationAuthorizationParams{OperationID: "od-cap-fit", Ordinal: 1, Amount: 1, MinimumAmount: 1})
		require.ErrorIs(t, err, billing.ErrInsufficientCredits)
	})

	t.Run("arrears capacity is unchanged", func(t *testing.T) {
		customer := newCustomer()
		_, err := client.SetCreditLimit(ctx, customer, billing.SetCreditLimitParams{Currency: "USD", Amount: 1_000_000})
		require.NoError(t, err)
		grant(customer, 500_000, "seed")
		require.NoError(t, open(customer, "ar-a", 500_000))
		settleProviderCost(t, ctx, database, client, "ar-a", 800_000)
		bal := balance(customer)
		require.Equal(t, billing.BillingModeArrears, bal.BillingMode)
		require.EqualValues(t, 300_000, bal.OwedAmount)
		require.ErrorIs(t, open(customer, "ar-over", 700_001), billing.ErrInsufficientCredits)
		require.NoError(t, open(customer, "ar-line", 700_000), "the remaining credit line, not the prepaid rule")
		grant(customer, 200_000, "arrears-funding")
		require.EqualValues(t, 100_000, balance(customer).OwedAmount, "funding repays arrears debt too")
	})

	t.Run("repayment pays the invoice that claims the debt", func(t *testing.T) {
		svc := money.NewMoneyService(database)
		from, to := time.Now().UTC().Add(-24*time.Hour).Truncate(time.Second), time.Now().UTC().Add(24*time.Hour).Truncate(time.Second)

		invoiced := newCustomer()
		overdraw(invoiced, "od-inv", 1_000_000, 300_000)
		inv, err := svc.FinalizeInvoice(ctx, identity.CustomerID(invoiced), "USD", from, to)
		require.NoError(t, err)
		require.EqualValues(t, 300_000, inv.AmountDue)
		require.Equal(t, "open", inv.Status)
		grant(invoiced, 1_000_000, "pays-invoice")
		bal := balance(invoiced)
		require.EqualValues(t, 0, bal.OwedAmount)
		require.EqualValues(t, 700_000, bal.BalanceAmount)
		paid, err := client.GetInvoice(ctx, billing.InvoiceID(inv.ID))
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceStatus("paid"), paid.Status)
		require.EqualValues(t, 0, paid.AmountDue)
		require.EqualValues(t, 300_000, paid.AmountPaid)
		history, err := client.ListInvoicePayments(ctx, billing.InvoiceID(inv.ID), billing.PageRequest{})
		require.NoError(t, err)
		require.Len(t, history.Items, 1)
		require.EqualValues(t, 300_000, history.Items[0].Amount)
		require.Nil(t, history.Items[0].Rail)

		// Repaid before any invoice: the period's invoice claims nothing.
		early := newCustomer()
		overdraw(early, "od-early", 1_000_000, 300_000)
		grant(early, 1_000_000, "before-invoice")
		inv, err = svc.FinalizeInvoice(ctx, identity.CustomerID(early), "USD", from, to)
		require.NoError(t, err)
		require.EqualValues(t, 0, inv.AmountDue)
		require.EqualValues(t, 300_000, inv.TotalAmount)
		require.EqualValues(t, 300_000, inv.AmountPaid)
		require.Equal(t, "paid", inv.Status)
	})

	t.Run("refund and chargeback of a lot that repaid owed", func(t *testing.T) {
		refunded := newCustomer()
		overdraw(refunded, "od-refund", 1_000_000, 300_000)
		payment, _ := purchase(refunded, 20_000_000, 20_000_000)
		require.EqualValues(t, 19_700_000, balance(refunded).BalanceAmount)
		credits := purchasedcredits.New(database)
		require.ErrorIs(t, credits.ValidateRefund(ctx, payment.ID, 20_000_000), billing.ErrConflict, "the repaid part is used credit")
		require.NoError(t, credits.ValidateRefund(ctx, payment.ID, 19_700_000))
		_, err := paymentService.Refund(ctx, payment.ID, uuid.NewString(), 19_700_000, payments.ReversalRefund)
		require.NoError(t, err)
		bal := balance(refunded)
		require.EqualValues(t, 0, bal.BalanceAmount)
		require.EqualValues(t, 0, bal.OwedAmount, "refunding unused credit leaves the repayment standing")

		disputed := newCustomer()
		overdraw(disputed, "od-dispute", 1_000_000, 300_000)
		payment, _ = purchase(disputed, 20_000_000, 20_000_000)
		chargeback, err := paymentService.Refund(ctx, payment.ID, uuid.NewString(), 20_000_000, payments.ReversalChargeback)
		require.NoError(t, err)
		bal = balance(disputed)
		require.EqualValues(t, 0, bal.BalanceAmount)
		require.EqualValues(t, 300_000, bal.OwedAmount, "a charged-back repayment is owed again")

		now := time.Now().UTC().Truncate(time.Microsecond)
		recovery := &models.Payment{ID: uuid.New(), CustomerID: disputed.UUID(), PriceID: price.ID.UUID(), Channel: models.ChannelRail, Rail: "stripe", PspID: new(psp.ID.UUID()),
			TransactionID: uuid.NewString(), Amount: 20_000_000, ListAmount: 20_000_000, Currency: "USD", Status: "completed", MoneyMovement: models.MoneyMovementRail,
			PurchasedAt: now, CreatedAt: now, RefundedPaymentID: &payment.ID, ReversalKind: new(payments.ReversalDisputeReversal),
			Metadata: map[string]any{"reverses_payment_id": chargeback.ID.String()}}
		require.NoError(t, paymentService.Create(ctx, recovery))
		bal = balance(disputed)
		require.EqualValues(t, 19_700_000, bal.BalanceAmount)
		require.EqualValues(t, 0, bal.OwedAmount, "a won dispute repays it again")
	})
}
