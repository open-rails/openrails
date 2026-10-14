package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// LocalWriter performs enforce mode's idempotent local mirror writes of
// provider facts (payments, refunds, vault metadata, subscription
// materialization); subscription transitions go through DecisionApplier. No
// method calls a rail, so enforce can run under readonly. Each reports whether
// it changed anything, so a second enforce run is observably a no-op.
type LocalWriter interface {
	BackfillPayment(ctx context.Context, a BackfillPaymentAction) (bool, error)
	RecordRefund(ctx context.Context, a RecordRefundAction) (bool, error)
	AdoptPaymentMethod(ctx context.Context, a AdoptPaymentMethodAction) (bool, error)
	GrantAccess(ctx context.Context, a GrantAccessAction) (bool, error)
	MaterializeSubscription(ctx context.Context, a MaterializeSubscriptionAction) (MaterializeResult, error)
	AcceptProviderSchedules(ctx context.Context, psp uuid.UUID, snap *RemoteSnapshot, exhaustive bool, reader TransactionReader) (ProviderScheduleResult, error)
}

// PGLocalWriter applies enforce writes via the sqlc layer on a merchant-pinned
// connection.
type PGLocalWriter struct {
	DB  *db.DB
	Now func() time.Time
}

var _ LocalWriter = (*PGLocalWriter)(nil)

func (w *PGLocalWriter) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

func metadataJSON(m map[string]any) []byte {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

func (w *PGLocalWriter) BackfillPayment(ctx context.Context, a BackfillPaymentAction) (bool, error) {
	if strings.TrimSpace(a.Currency) == "" {
		return false, fmt.Errorf("payment currency required")
	}
	var changed bool
	err := w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := w.DB.NewWithPgxTx(tx)
		if a.SubscriptionID == nil {
			return errors.New("unattributed charge requires its accepted purchase order; price alone is not a bill")
		}
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, *a.SubscriptionID)
		if err != nil {
			return err
		}
		if sub.CustomerID != a.CustomerID || a.PspID == nil || sub.PspID != *a.PspID || string(sub.Rail) != a.Rail {
			return observed.ErrConflict
		}
		txn := RemoteTransaction{TransactionID: a.TransactionID, SubscriptionID: sub.RailSubscriptionID, Type: TransactionTypeSale, Success: true,
			AmountCents: a.AmountCents, Currency: a.Currency, OccurredAt: a.PurchasedAt, StripeBill: a.StripeBill, NextBillingAt: a.NextBillingAt, PaymentMetadata: a.Metadata}
		if a.AmountMicros != nil {
			return errors.New("historical amount-only import requires explicit accepted bill evidence")
		}
		count, err := backfillSubscriptionPayments(ctx, d, sub, []RemoteTransaction{txn}, w.now(), declaredImportLookback)
		if err != nil {
			return err
		}
		changed = count > 0
		// The lifecycle convergence path applies the verified paid period.
		// Replaying a money fact never grants today's catalog/current period.
		return nil
	})
	return changed, err
}

func (w *PGLocalWriter) RecordRefund(ctx context.Context, a RecordRefundAction) (bool, error) {
	if a.RefundedPaymentID == nil || strings.TrimSpace(a.TransactionID) == "" {
		return false, errors.New("refund requires its original payment and provider reference")
	}
	amount, err := moneyutil.RailMinorToNative(a.Currency, moneyutil.Cents(a.AmountCents))
	if err != nil || amount <= 0 {
		return false, errors.New("refund requires a positive exact billed amount")
	}
	svc := payments.NewPaymentService(w.DB, clockwork.NewFakeClockAt(w.now()))
	original, err := svc.GetByID(ctx, *a.RefundedPaymentID)
	if err != nil {
		return false, err
	}
	if original.OrderID == nil || original.CustomerID != a.CustomerID || string(original.Rail) != a.Rail || !strings.EqualFold(original.Currency, a.Currency) || original.PspID == nil || a.PspID == nil || *original.PspID != *a.PspID {
		return false, observed.ErrConflict
	}
	if a.SubscriptionID != nil && (original.SubscriptionID == nil || *a.SubscriptionID != *original.SubscriptionID) {
		return false, observed.ErrConflict
	}
	// Query reports can reuse the sale's reference for its refund action.
	// Namespace that verified action; retain the original provider reference.
	key := a.TransactionID
	if key == original.TransactionID {
		key = "refund:" + key
	}
	_, lookupErr := svc.GetByPSPTransactionID(db.WithPSPID(ctx, *original.PspID), original.Rail, key)
	if lookupErr != nil && !db.IsNotFound(lookupErr) {
		return false, lookupErr
	}
	_, err = svc.RecordObservedRefund(ctx, original.ID, key, amount, payments.ReversalRefund, a.PurchasedAt, a.Metadata)
	return db.IsNotFound(lookupErr) && err == nil, err
}

// AdoptPaymentMethod takes the provider's card onto the method through the
// card writer every source uses, so the change is a card version
// (provider_read) and a pull that reads the same card again writes nothing. A
// method that writer no longer versions (its status is final) keeps the
// in-place adoption.
func (w *PGLocalWriter) AdoptPaymentMethod(ctx context.Context, a AdoptPaymentMethodAction) (bool, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	var life paymentmethods.CardLifecycle
	err = w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		life, err = paymentmethods.ApplyCardLifecycle(ctx, gen.New(tx), paymentmethods.CardEvent{
			MerchantID: tid.UUID(), PaymentMethodID: a.PaymentMethodID,
			Source: paymentmethods.SourceProviderRead, EventRef: a.EventRef,
			Card: paymentmethods.Card{Card: a.Card}, At: w.now(),
		})
		return err
	})
	if err != nil {
		return false, err
	}
	if life.Method.Status == paymentmethods.StatusActive {
		return life.Change != "", nil
	}
	_, last4, month, year := a.Card.Columns()
	n, err := w.DB.Gen(ctx).ReconcileAdoptPaymentMethod(ctx, gen.ReconcileAdoptPaymentMethodParams{
		CardLast4:    last4,
		CardExpMonth: month,
		CardExpYear:  year,
		ID:           a.PaymentMethodID,
		MerchantID:   tid.UUID(),
	})
	return n > 0, err
}

func (w *PGLocalWriter) GrantAccess(ctx context.Context, a GrantAccessAction) (bool, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	granted := false
	err = w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		gl := grants.New(gen.New(tx), tid.UUID())
		gl.SetClock(w.now)
		granted, err = gl.GrantSubscriptionWindow(ctx, a.CustomerID, a.SubscriptionID, a.ProductID, grants.Subscription, a.StartsAt, a.EndsAt, nil)
		return err
	})
	return granted, err
}

// MaterializeSubscription creates the local subscription for a resolved
// remote-only rail subscription. Idempotent: when a local subscription already
// carries the rail subscription id, the insert returns no row and nothing else
// runs. Its product is granted through the subscription-sourced path while the
// remote period still runs.
func (w *PGLocalWriter) MaterializeSubscription(ctx context.Context, a MaterializeSubscriptionAction) (MaterializeResult, error) {
	if a.Rail == string(models.RailSolana) || a.Provider == ProviderSolana {
		return MaterializeResult{}, errors.New("Solana materialization requires its accepted purchase and verified chain receipt")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return MaterializeResult{}, err
	}
	rows, err := w.DB.Gen(ctx).ReconcileMaterializeSubscription(ctx, gen.ReconcileMaterializeSubscriptionParams{
		MerchantID:         tid.UUID(),
		Status:             string(a.Status),
		Rail:               a.Rail,
		RailSubscriptionID: a.RailSubscriptionID,
		PeriodStartsAt:     a.PeriodStartsAt,
		PeriodEndsAt:       a.PeriodEndsAt,
		StartedAt:          a.StartedAt,
		CustomerID:         a.CustomerID,
		PriceID:            a.PriceID,
		Rails:              localRailNames(a.Provider),
		PspID:              a.PspID,
	})
	if err != nil {
		return MaterializeResult{}, err
	}
	if len(rows) == 0 {
		return MaterializeResult{}, nil // already materialized (or price vanished): no-op
	}
	res := MaterializeResult{SubscriptionID: rows[0].ID, Created: true}

	// Explicit access is independent of billing; a provider DEFAULT requires
	// this observed service period's verified end and never means indefinite.
	now := w.now()
	start := now
	if a.StartedAt != nil {
		start = *a.StartedAt
	}
	if a.PeriodStartsAt != nil {
		start = *a.PeriodStartsAt
	}
	var accessEnd *time.Time
	if rows[0].ProviderAccessDefaultSnapshot {
		accessEnd = rows[0].CurrentPeriodEndsAt
	} else if rows[0].AccessDurationHoursSnapshot != nil {
		end := start.Add(time.Duration(*rows[0].AccessDurationHoursSnapshot) * time.Hour)
		accessEnd = &end
	}
	if !rows[0].ProviderAccessDefaultSnapshot && accessEnd == nil || accessEnd != nil && accessEnd.After(now) {
		granted, err := w.GrantAccess(ctx, GrantAccessAction{
			SubscriptionID: res.SubscriptionID,
			CustomerID:     a.CustomerID,
			ProductID:      rows[0].ProductID,
			StartsAt:       start,
			EndsAt:         accessEnd,
		})
		if err != nil {
			return res, err
		}
		res.AccessGranted = granted
	}

	if a.Backfill != nil {
		b := *a.Backfill
		subID := res.SubscriptionID
		b.SubscriptionID = &subID
		backfilled, err := w.BackfillPayment(ctx, b)
		if err != nil {
			return res, err
		}
		res.PaymentBackfilled = backfilled
	}
	return res, nil
}

// uuidOrNil is an optional id: the zero id is none.
func uuidOrNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
