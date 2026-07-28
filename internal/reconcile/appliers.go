package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// LocalWriter performs enforce mode's idempotent LOCAL MIRROR writes (#665:
// provider-fact rows only — payments, refunds, vault metadata, subscription
// materialization; subscription state transitions go through the decider's
// DecisionApplier instead). No method ever calls a rail — that is the design
// invariant that lets enforce run under mode=readonly. Every method reports
// whether it changed anything, so a second enforce run is observably a no-op.
type LocalWriter interface {
	BackfillPayment(ctx context.Context, a BackfillPaymentAction) (bool, error)
	RecordRefund(ctx context.Context, a RecordRefundAction) (bool, error)
	AdoptPaymentMethod(ctx context.Context, a AdoptVaultAction) (bool, error)
	GrantEntitlements(ctx context.Context, a GrantEntitlementsAction) (int, error)
	MaterializeSubscription(ctx context.Context, a MaterializeSubscriptionAction) (MaterializeResult, error)
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
	currency := strings.TrimSpace(a.Currency)
	if currency == "" {
		return false, fmt.Errorf("payment currency required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := w.DB.Gen(ctx).ReconcileBackfillPayment(ctx, gen.ReconcileBackfillPaymentParams{
		MerchantID:     tid.UUID(),
		PriceID:        a.PriceID,
		Rail:           string(a.Rail),
		TransactionID:  a.TransactionID,
		Amount:         int64(moneyutil.CentsToMicros(moneyutil.Cents(a.AmountCents))),
		Currency:       currency,
		SubscriptionID: a.SubscriptionID,
		Metadata:       metadataJSON(a.Metadata),
		PurchasedAt:    a.PurchasedAt,
		CustomerID:     a.CustomerID,
		PspID:          a.PspID,
	})
	if err != nil {
		return false, err
	}
	if a.Grant != nil {
		if _, err := w.GrantEntitlements(ctx, *a.Grant); err != nil {
			return n > 0, err
		}
	}
	return n > 0, nil
}

func (w *PGLocalWriter) RecordRefund(ctx context.Context, a RecordRefundAction) (bool, error) {
	if a.MarkRefundedOnly {
		if a.RefundedPaymentID == nil {
			return false, nil
		}
		n, err := w.DB.Gen(ctx).ReconcileMarkPaymentRefunded(ctx, *a.RefundedPaymentID)
		return n > 0, err
	}
	amount := int64(moneyutil.CentsToMicros(moneyutil.Cents(a.AmountCents)))
	if amount > 0 {
		amount = -amount // refunds are negative-amount payment rows
	}
	currency := strings.TrimSpace(a.Currency)
	if currency == "" {
		return false, fmt.Errorf("payment currency required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := w.DB.Gen(ctx).ReconcileRecordRefund(ctx, gen.ReconcileRecordRefundParams{
		MerchantID:        tid.UUID(),
		PriceID:           a.PriceID,
		Rail:              string(a.Rail),
		TransactionID:     a.TransactionID,
		Amount:            amount,
		Currency:          currency,
		SubscriptionID:    a.SubscriptionID,
		RefundedPaymentID: a.RefundedPaymentID,
		Metadata:          metadataJSON(a.Metadata),
		PurchasedAt:       a.PurchasedAt,
		CustomerID:        a.CustomerID,
		PspID:             a.PspID,
	})
	if err != nil {
		return false, err
	}
	if n > 0 && a.RefundedPaymentID != nil {
		if _, err := w.DB.Gen(ctx).ReconcileMarkPaymentRefunded(ctx, *a.RefundedPaymentID); err != nil {
			return true, err
		}
	}
	return n > 0, nil
}

func (w *PGLocalWriter) AdoptPaymentMethod(ctx context.Context, a AdoptVaultAction) (bool, error) {
	n, err := w.DB.Gen(ctx).ReconcileAdoptPaymentMethod(ctx, gen.ReconcileAdoptPaymentMethodParams{
		LastFour:   a.LastFour,
		ExpiryDate: a.ExpiryDate,
		ID:         a.PaymentMethodID,
	})
	return n > 0, err
}

func (w *PGLocalWriter) GrantEntitlements(ctx context.Context, a GrantEntitlementsAction) (int, error) {
	now := w.now()
	granted := 0
	tid, err := merchant.Require(ctx)
	if err != nil {
		return granted, err
	}
	for _, name := range a.Entitlements {
		n, err := w.DB.Gen(ctx).ReconcileGrantSubscriptionEntitlement(ctx, gen.ReconcileGrantSubscriptionEntitlementParams{
			MerchantID:     tid.UUID(),
			CustomerID:     a.CustomerID,
			Entitlement:    name,
			StartAt:        a.StartAt,
			EndAt:          a.EndAt,
			SubscriptionID: a.SubscriptionID,
			Now:            now,
		})
		if err != nil {
			return granted, err
		}
		granted += int(n)
	}
	return granted, nil
}

// MaterializeSubscription creates the local subscription for a resolved PS-1
// (bootstrap mode v1.1). Idempotent: when any local subscription already
// carries the rail subscription id, the insert returns zero rows and
// nothing else runs. The created row snapshots the product's entitlements
// spec, and entitlements are granted through the normal subscription-sourced
// path when the remote period is still running.
func (w *PGLocalWriter) MaterializeSubscription(ctx context.Context, a MaterializeSubscriptionAction) (MaterializeResult, error) {
	var emailPtr *string
	if email := a.UserEmail; email != "" {
		emailPtr = &email
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return MaterializeResult{}, err
	}
	rows, err := w.DB.Gen(ctx).ReconcileMaterializeSubscription(ctx, gen.ReconcileMaterializeSubscriptionParams{
		MerchantID:         tid.UUID(),
		Status:             gen.OpenrailsSubscriptionStatus(a.Status),
		Rail:               a.Rail,
		RailSubscriptionID: a.RailSubscriptionID,
		UserEmail:          emailPtr,
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

	// Entitlements via the normal subscription-sourced path, for the remote
	// period when it is still running (mirrors the PS-4 current-period grant).
	now := w.now()
	if a.PeriodEndsAt == nil || a.PeriodEndsAt.After(now) {
		var spec map[string]json.RawMessage
		if len(rows[0].EntitlementsSpecSnapshot) > 0 {
			_ = json.Unmarshal(rows[0].EntitlementsSpecSnapshot, &spec)
		}
		if len(spec) > 0 {
			names := make([]string, 0, len(spec))
			for name := range spec {
				names = append(names, name)
			}
			sort.Strings(names)
			start := now
			if a.PeriodStartsAt != nil {
				start = *a.PeriodStartsAt
			}
			granted, err := w.GrantEntitlements(ctx, GrantEntitlementsAction{
				SubscriptionID: res.SubscriptionID,
				CustomerID:     a.CustomerID,
				Entitlements:   names,
				StartAt:        start,
				EndAt:          a.PeriodEndsAt,
			})
			if err != nil {
				return res, err
			}
			res.EntitlementsGranted = granted
		}
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
