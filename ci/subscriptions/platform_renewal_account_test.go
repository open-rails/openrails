//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
)

// These fixtures seed an accepted attempt and a mock receipt on another account.
// They exercise local settlement, not permission to submit NMI renewals there.
func TestPlatformRenewalPaymentAccount(t *testing.T) {
	for _, scenario := range []string{"paid", "canceled", "submission refused"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w, bt := platformSubscriptionWorld(t)
			c := w.newCustomer()
			paid := c.mustPurchase(remote, order{price: w.membership("platform:account", 9_990_000).ID, rail: "nmi", token: bt.collect("4111111111111111")})
			subID := *paid.SubscriptionID
			before := w.subscription(remote, subID)
			second, err := w.client[remote].CreatePSP(t.Context(), billing.CreatePSPParams{
				Key: "settlement", Rail: "nmi", Environment: "sandbox", PSPAccount: billing.PSPAccount{GatewayID: "settlement-account"},
				Credentials: map[string]string{"security_key": "settlement-key", "webhook_signing_secret": "settlement-webhook"},
			})
			require.NoError(t, err)
			w.advanceHealthyTo(*before.CurrentPeriodEndsAt)
			in := w.acceptPlatformRenewalOn(subID, second.ID)
			p, err := subscriptions.DecodeSubscriptionCollectionPayload(in)
			require.NoError(t, err)
			require.Equal(t, w.psp["nmi"].UUID(), p.Renewal.PSPID)
			require.Equal(t, second.ID.UUID(), p.Instrument.PSPID)
			rt := engine.Graph(w.rt).Runtime
			ctx := db.WithPSPID(merchant.WithID(t.Context(), w.client[remote].MerchantID()), second.ID.UUID())
			handler := money.NewSubscriptionCollectionHandler(rt.DB, rt.CollectionResolver, rt.Config, w.clock)
			if scenario == "submission refused" {
				out := handler.Execute(ctx, in)
				require.Equal(t, intents.OutcomeParked, out.Class, out.Reason)
				require.Contains(t, out.Reason, "original gateway account")
				require.Len(t, bt.salesOf("4111111111111111"), 1, "NMI cannot submit the original agreement through another account")
				require.Equal(t, before.CurrentPeriodEndsAt, w.subscription(remote, subID).CurrentPeriodEndsAt)
				return
			}

			// Decoding still binds the operation to its payment account, and only
			// platform custody can differ from the subscription's account.
			for _, mutation := range []string{"operation account", "provider custody"} {
				bad, payload := in, p
				if mutation == "operation account" {
					bad.PspID = new(w.psp["nmi"].UUID())
				} else {
					bad.CustodianID = nil
					payload.Instrument.Custodian, payload.Instrument.CustodianID = "psp", nil
					payload.Instrument.RailCustomerRef = "provider-customer"
				}
				bad.Payload, err = json.Marshal(payload)
				require.NoError(t, err)
				_, err = subscriptions.DecodeSubscriptionCollectionPayload(bad)
				if mutation == "provider custody" {
					require.ErrorContains(t, err, "provider held renewal cannot change its account")
				} else {
					require.ErrorContains(t, err, "operation contradicts accepted scope")
				}
			}
			wrong := fixedRenewalReader{CollectionPlane: rt.CollectionResolver, account: w.psp["nmi"].UUID()}
			_, _, err = intents.ReadNMICollectionReceipt(ctx, in, wrong, "")
			require.ErrorContains(t, err, "another provider account")

			if scenario == "paid" {
				w.checkPlatformRenewalOrder(p)
			}
			if scenario == "canceled" {
				c.must(http.MethodPost, "/subscriptions/"+subID.String()+"/cancel", "", map[string]any{"reason": "done"})
			}
			// No provider write occurs: this is a controlled financial fact for
			// testing the completion path, including an initially missing receipt.
			out := handler.Verify(ctx, in)
			require.NotEqual(t, intents.OutcomeSucceeded, out.Class)
			require.Len(t, completed(w.payments(remote, c.id)), 1)
			sale := w.nmi.AddSale(nmimock.Sale{Vault: w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"}), OrderID: p.OrderReference, OrderDescription: subscriptions.SubscriptionCollectionDescription(in.ID),
				Amount: "9.99", Currency: "USD", Card: nmimock.Card{Brand: "visa", Last4: "1111"}, InitiatedBy: "merchant", Indicator: "used", Initial: p.Instrument.Mandate.InitialTransactionID})
			var refunds atomic.Int64
			w.nmi.Intercept(func(r *http.Request) bool {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/refund") {
					refunds.Add(1)
					if r.Header.Get("Authorization") != "settlement-key" {
						t.Error("refund did not use the accepted payment account")
					}
				}
				return false
			}, nil)
			out = handler.Verify(ctx, in)
			require.Equal(t, intents.OutcomeSucceeded, out.Class, out.Reason)
			out = handler.Verify(ctx, in)
			require.Equal(t, intents.OutcomeSucceeded, out.Class, out.Reason)
			var paymentID billing.PaymentID
			for _, payment := range w.payments(remote, c.id) {
				if payment.TransactionID == sale.TransactionID {
					paymentID = payment.ID
					require.Equal(t, new(second.ID), payment.PSPID)
				}
			}
			require.False(t, paymentID.IsZero())
			var paymentCount int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payments WHERE subscription_id = $1 AND refunded_payment_id IS NULL`), subID.UUID()).Scan(&paymentCount))
			require.Equal(t, 2, paymentCount, "receipt replay must not create a second renewal payment")
			sub := w.subscription(remote, subID)
			require.Equal(t, before.PSPID, sub.PSPID)
			require.Equal(t, before.PriceID, sub.PriceID)
			if scenario == "paid" {
				require.True(t, sub.CurrentPeriodEndsAt.Equal(p.Renewal.PeriodEnd))
				require.NoError(t, rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
					d := rt.DB.NewWithPgxTx(tx)
					current, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subID.UUID())
					if err != nil {
						return err
					}
					next, err := intents.PrepareEngineRenewalTerms(ctx, d, current, *sub.CurrentPeriodEndsAt)
					if err != nil {
						return err
					}
					require.Equal(t, p.Renewal.PSPID, next.PSPID)
					require.Equal(t, p.Renewal.Amount, next.Amount)
					require.True(t, next.PeriodStart.Equal(p.Renewal.PeriodEnd))
					return nil
				}))
				_, err = w.client[remote].RefundPayment(t.Context(), paymentID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "account-refund"})
				require.NoError(t, err)
			} else {
				require.Equal(t, billing.SubscriptionCanceled, sub.Status)
				require.Equal(t, before.CurrentPeriodEndsAt, sub.CurrentPeriodEndsAt)
			}
			w.settle()
			payment, err := w.client[remote].GetPayment(t.Context(), paymentID)
			require.NoError(t, err)
			require.Equal(t, billing.PaymentRefunded, payment.Status)
			require.EqualValues(t, 1, refunds.Load())
		})
	}
}

func (w *world) acceptPlatformRenewalOn(id billing.SubscriptionID, account billing.PSPID) gen.BillingProviderIntent {
	w.t.Helper()
	rt := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(w.t.Context(), w.client[remote].MerchantID())
	var in gen.BillingProviderIntent
	err := rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := rt.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, id.UUID())
		if err != nil {
			return err
		}
		methodID, err := subscriptions.PaymentMethodOf(ctx, q, sub)
		if err != nil {
			return err
		}
		method, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: sub.MerchantID, ID: *methodID})
		if err != nil {
			return err
		}
		terms, err := intents.PrepareEngineRenewalTerms(ctx, d, sub, w.clock.Now())
		if err != nil {
			return err
		}
		instrument := charge.FreezeInstrument(method, account.UUID())
		instrument.Mandate, err = mandates.ForSubscription(ctx, q, sub.MerchantID, sub.CustomerID, sub.ID, method.ID, sub.PspID)
		if err != nil {
			return err
		}
		order, err := orders.OpenRenewal(ctx, q, sub.MerchantID, money.RenewalOrderOf(terms), w.clock.Now())
		if err != nil {
			return err
		}
		minor, err := moneyutil.NativeToRailMinorExact(terms.Currency, terms.Amount)
		if err != nil {
			return err
		}
		p := subscriptions.SubscriptionCollectionPayload{Initiator: charge.InitiatorMerchant, Renewal: terms, PreviousPeriodEnd: *sub.CurrentPeriodEndsAt,
			AcceptedAt: w.clock.Now().UTC().Truncate(time.Microsecond), PaymentMethodID: method.ID, Instrument: instrument, AmountMinor: minor,
			OrderReference: subscriptions.ObligationOrderReference(sub.ID, *sub.CurrentPeriodEndsAt), OrderID: order.ID}
		in, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: sub.MerchantID, Provider: "nmi", IntentType: subscriptions.TypeSubscriptionCollection,
			SubscriptionID: &sub.ID, PriceID: &terms.PriceID, PspID: account.UUID(), CustodianID: *method.CustodianID, Payload: p,
			IdempotencyKey: subscriptions.SubscriptionCollectionKey(sub.ID, *sub.CurrentPeriodEndsAt, 0), NextAttemptAt: w.clock.Now().Add(365 * 24 * time.Hour), Origin: intents.OriginSystem})
		if err != nil {
			return err
		}
		return orders.StartRenewalAttempt(ctx, q, order, in.ID, account.UUID(), "nmi", &method.ID, w.clock.Now())
	})
	require.NoError(w.t, err)
	in, claimed, err := intents.NewStore(rt.DB).ClaimByID(ctx, in.ID, w.clock.Now(), w.clock.Now().Add(time.Hour))
	require.NoError(w.t, err)
	require.True(w.t, claimed)
	return in
}

type fixedRenewalReader struct {
	money.CollectionPlane
	account uuid.UUID
}

func (r fixedRenewalReader) ResolveNMIClient(ctx context.Context, mid uuid.UUID, _ *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return r.CollectionPlane.ResolveNMIClient(ctx, mid, &r.account)
}

// Exercise the order writer before settlement; every case is rolled back.
func (w *world) checkPlatformRenewalOrder(p subscriptions.SubscriptionCollectionPayload) {
	w.t.Helper()
	rt := engine.Graph(w.rt).Runtime
	mid := w.client[remote].MerchantID()
	ctx := merchant.WithID(w.t.Context(), mid)
	terms := p.Renewal
	rollback := errors.New("rollback order check")
	for _, scenario := range []string{"accepted", "foreign merchant", "no order", "wrong period", "no attempt", "wrong attempt account", "provider bill"} {
		w.t.Run("order "+scenario, func(t *testing.T) {
			err := rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				d := rt.DB.NewWithPgxTx(tx)
				interval := int(terms.PeriodEnd.Sub(terms.PeriodStart) / time.Hour)
				b := observed.Bill{PSPID: p.Instrument.PSPID, CustomerID: terms.CustomerID, SubscriptionID: &terms.SubscriptionID,
					ExistingOrderID: &p.OrderID, Owner: observed.OwnerEngine, Kind: "renewal", SourceKind: "accepted_renewal", SourceKey: "test-account-boundary",
					Currency: terms.Currency, Total: terms.Amount, PeriodStart: terms.PeriodStart, PeriodEnd: terms.PeriodEnd, DueAt: terms.PeriodStart,
					Payment: &models.Payment{CustomerID: terms.CustomerID, SubscriptionID: &terms.SubscriptionID, Rail: models.RailNMI,
						Amount: terms.Amount, ListAmount: terms.Amount, Currency: terms.Currency, Status: "succeeded", MoneyMovement: models.MoneyMovementRail, TransactionID: "account-boundary"},
					Lines: []observed.Line{{PriceID: &terms.PriceID, ProductID: &terms.ProductID, Description: terms.ProductName, Quantity: terms.Quantity,
						UnitAmount: terms.UnitAmount(), Amount: terms.Amount, BillingIntervalHours: &interval}}}
				switch scenario {
				case "foreign merchant":
					ctx = merchant.WithID(ctx, billing.MerchantID(uuid.New()))
				case "no order":
					b.ExistingOrderID = nil
				case "wrong period":
					b.PeriodEnd = b.PeriodEnd.Add(time.Hour)
				case "no attempt":
					_, err := tx.Exec(ctx, w.q(`UPDATE billing.orders SET attempt_id = NULL WHERE merchant_id = $1 AND id = $2`), mid.UUID(), p.OrderID)
					require.NoError(t, err)
				case "wrong attempt account":
					_, err := tx.Exec(ctx, w.q(`UPDATE billing.order_attempts SET psp_id = $1 WHERE merchant_id = $2 AND order_id = $3`), terms.PSPID, mid.UUID(), p.OrderID)
					require.NoError(t, err)
				case "provider bill":
					b.SourceKind = "provider_renewal"
				}
				_, err := observed.Record(ctx, d, b, w.clock.Now())
				if scenario == "accepted" {
					require.NoError(t, err)
				} else if scenario == "foreign merchant" {
					require.ErrorIs(t, err, pgx.ErrNoRows)
				} else {
					require.ErrorIs(t, err, observed.ErrConflict)
				}
				return rollback
			})
			require.ErrorIs(t, err, rollback)
		})
	}
}
