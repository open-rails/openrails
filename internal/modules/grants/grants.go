// Package grants is the append-only grant ledger: derive-1 appends immutable
// grant events (the sole writer of billing.grants); derive-2 projects them into
// product access windows. Credit lots are the credits package's.
package grants

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Kind is what a grant confers.
type Kind string

const (
	Access Kind = "access"
	// Entitlement and Ownership grants are history from before product access.
	Entitlement Kind = "entitlement"
	Ownership   Kind = "ownership"
)

// SourceType is the origin of a grant.
type SourceType string

const (
	Purchase     SourceType = "purchase"
	Subscription SourceType = "subscription"
	Grace        SourceType = "grace"
	// Granted is a free product grant: comp, staff, import or migration.
	Granted SourceType = "grant"
)

// GrantReason says why a free product was granted.
type GrantReason string

const (
	ReasonComp      GrantReason = "comp"
	ReasonStaff     GrantReason = "staff"
	ReasonImport    GrantReason = "import"
	ReasonMigration GrantReason = "migration"
)

// Ledger is the append-only grant ledger for one merchant.
//
// Valid time lives in starts_at/ends_at: backdatable, so the fold replays at
// any wall clock. A termination is a point event at starts_at with ends_at
// NULL. Transaction time is created_at, never read as a business fact.
type Ledger struct {
	q        *gen.Queries
	merchant uuid.UUID
	now      func() time.Time
}

// New binds a grant Ledger to a query handle and merchant. Compose it inside a
// pgx transaction for atomic derive-1 + derive-2.
func New(q *gen.Queries, merchant uuid.UUID) *Ledger {
	return &Ledger{
		q:        q,
		merchant: merchant,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the grant ledger's time source (event timestamps), so a
// caller that runs on an injected clock derives consistently. nil restores
// the default real clock.
func (l *Ledger) SetClock(now func() time.Time) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	l.now = now
}

// GrantInput describes a new grant to append.
type GrantInput struct {
	Customer uuid.UUID
	Product  *uuid.UUID
	Kind     Kind
	Source   SourceType
	SourceID string
	Payment  *uuid.UUID
	StartsAt time.Time // zero => now
	EndsAt   *time.Time
	Reason   *string // free-text provenance
	// Actor and GrantReason attribute a free product grant (Source Granted).
	Actor       string
	GrantReason GrantReason
	// Quantity is the seats an access grant gives: a per-seat subscription's;
	// nil otherwise.
	Quantity *int
}

// seats is a grant's quantity column.
func seats(quantity *int) (*int32, error) {
	if quantity == nil {
		return nil, nil
	}
	if *quantity < 1 || *quantity > math.MaxInt32 {
		return nil, fmt.Errorf("grants: quantity %d is out of range", *quantity)
	}
	q := int32(*quantity) // #nosec G115 -- range checked above
	return &q, nil
}

// Grant appends a 'grant' event (derive-1) and projects it (derive-2) on the
// same queries, so the caller's transaction commits both or neither.
func (l *Ledger) Grant(ctx context.Context, in GrantInput) (gen.BillingGrant, error) {
	starts := in.StartsAt
	if starts.IsZero() {
		starts = l.now()
	}
	var actor, reason *string
	if in.Actor != "" {
		actor = &in.Actor
	}
	if in.GrantReason != "" {
		r := string(in.GrantReason)
		reason = &r
	}
	quantity, err := seats(in.Quantity)
	if err != nil {
		return gen.BillingGrant{}, err
	}
	g, err := l.q.InsertGrant(ctx, gen.InsertGrantParams{
		Quantity:   quantity,
		MerchantID: l.merchant, CustomerID: in.Customer, ProductID: in.Product,
		Kind: string(in.Kind), SourceType: string(in.Source), SourceID: in.SourceID, PaymentID: in.Payment,
		Event: "grant", SupersedesID: nil,
		StartsAt: starts, EndsAt: in.EndsAt, Reason: in.Reason, Actor: actor, GrantReason: reason,
	})
	if err != nil {
		return g, err
	}
	return g, l.MaterializeGrant(ctx, g)
}

// GrantAccessOnce appends an access grant at its natural key (a purchase, a
// period start, an idempotency key). A replay returns the recorded grant and
// created=false.
func (l *Ledger) GrantAccessOnce(ctx context.Context, in GrantInput) (g gen.BillingGrant, created bool, err error) {
	if in.Product == nil {
		return g, false, fmt.Errorf("grants: an access grant needs its product")
	}
	starts := in.StartsAt
	if starts.IsZero() {
		starts = l.now()
	}
	var actor, reason *string
	if in.Actor != "" {
		actor = &in.Actor
	}
	if in.GrantReason != "" {
		r := string(in.GrantReason)
		reason = &r
	}
	quantity, err := seats(in.Quantity)
	if err != nil {
		return g, false, err
	}
	g, err = l.q.InsertAccessGrantOnce(ctx, gen.InsertAccessGrantOnceParams{
		Quantity:   quantity,
		MerchantID: l.merchant, CustomerID: in.Customer, ProductID: *in.Product,
		SourceType: string(in.Source), SourceID: in.SourceID, PaymentID: in.Payment,
		StartsAt: starts, EndsAt: in.EndsAt, Reason: in.Reason, Actor: actor, GrantReason: reason,
	})
	if err == nil {
		return g, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return g, false, err
	}
	switch {
	case in.Source == Granted:
		g, err = l.q.GetAccessGrantByIdempotencyKey(ctx, gen.GetAccessGrantByIdempotencyKeyParams{MerchantID: l.merchant, IdempotencyKey: in.SourceID})
	case in.Source == Purchase && in.Payment != nil:
		g, err = l.q.GetAccessGrantByPurchase(ctx, gen.GetAccessGrantByPurchaseParams{MerchantID: l.merchant, PaymentID: *in.Payment, ProductID: *in.Product})
	default:
		var found []gen.BillingGrant
		found, err = l.q.ListAccessGrantsAt(ctx, gen.ListAccessGrantsAtParams{
			MerchantID: l.merchant, CustomerID: in.Customer, ProductID: *in.Product,
			SourceType: string(in.Source), SourceID: in.SourceID, StartsAt: starts,
		})
		if err == nil && len(found) == 0 {
			err = pgx.ErrNoRows
		}
		if err == nil {
			g = found[0]
		}
	}
	if err != nil {
		return g, false, fmt.Errorf("grants: read recorded access grant: %w", err)
	}
	return g, false, nil
}

// Revoke appends a 'revoke' event terminating the grant (derive-1), effective now.
// The grant row is never edited. A grant may be terminated at most once (unique index).
func (l *Ledger) Revoke(ctx context.Context, grantID uuid.UUID, reason, actor string) (gen.BillingGrant, error) {
	return l.terminate(ctx, grantID, "revoke", reason, actor, time.Time{})
}

// RevokeAsOf is Revoke with an explicit effective instant (valid time) on the
// termination's starts_at, so a converged revocation agrees with the
// entitlement effect instead of stamping wall clock. Zero means now.
func (l *Ledger) RevokeAsOf(ctx context.Context, grantID uuid.UUID, reason, actor string, asOf time.Time) (gen.BillingGrant, error) {
	return l.terminate(ctx, grantID, "revoke", reason, actor, asOf)
}

// terminate appends a termination event (revoke/expire) superseding the grant.
// asOf is the effective instant on starts_at; zero means now(). ends_at is
// always NULL, so it never trips grants_valid_window_check even when the
// grant already expired.
func (l *Ledger) terminate(ctx context.Context, grantID uuid.UUID, event, reason, actor string, asOf time.Time) (gen.BillingGrant, error) {
	g, err := l.q.GetGrant(ctx, gen.GetGrantParams{MerchantID: l.merchant, ID: grantID})
	if err != nil {
		return gen.BillingGrant{}, fmt.Errorf("grants: load grant %s: %w", grantID, err)
	}
	if g.Event != "grant" {
		return gen.BillingGrant{}, fmt.Errorf("grants: %s is a %q event, not a grant", grantID, g.Event)
	}
	effective := asOf
	if effective.IsZero() {
		effective = l.now()
	}
	sup := grantID
	r := reason
	var by *string
	if actor != "" {
		by = &actor
	}
	return l.q.InsertGrant(ctx, gen.InsertGrantParams{
		Quantity:   g.Quantity,
		MerchantID: l.merchant, CustomerID: g.CustomerID, ProductID: g.ProductID,
		Kind: g.Kind, SourceType: g.SourceType, SourceID: sourceIDOf(g), PaymentID: g.PaymentID,
		Event: event, SupersedesID: &sup, SpecSnapshot: g.SpecSnapshot,
		StartsAt: effective, EndsAt: nil, Reason: &r, Actor: by,
	})
}

// MaterializeGrant projects a single grant event (derive-2): the access window
// of an access grant. Terminated grants have their window retracted.
// Historical entitlement and ownership grants have no projection.
func (l *Ledger) MaterializeGrant(ctx context.Context, g gen.BillingGrant) error {
	if g.Event != "grant" {
		return fmt.Errorf("grants: MaterializeGrant needs a grant event, got %q", g.Event)
	}
	terminated, err := l.q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: l.merchant, GrantID: g.ID})
	if err != nil {
		return fmt.Errorf("grants: termination check: %w", err)
	}

	switch Kind(g.Kind) {
	case Access:
		start := g.StartsAt
		if g.ImportRootID != nil && g.ImportAsOf != nil && g.ImportAsOf.After(start) {
			start = *g.ImportAsOf
		}
		if g.EndsAt != nil && !g.EndsAt.After(start) {
			return nil // A correction can end before its own effective instant.
		}
		var importTermination *gen.BillingGrant
		if terminated && g.Actor != nil && *g.Actor == "import" && g.GrantReason != nil && *g.GrantReason == string(ReasonImport) {
			t, err := l.q.GetGrantTermination(ctx, gen.GetGrantTerminationParams{MerchantID: l.merchant, GrantID: g.ID})
			if err != nil {
				return err
			}
			if t.Actor != nil && *t.Actor == "import" && (t.Event == "revoke" || t.Event == "supersede") {
				importTermination = &t
			}
		}
		if importTermination != nil && !importTermination.StartsAt.After(start) {
			return l.q.BoundImportedProductAccessByGrant(ctx, gen.BoundImportedProductAccessByGrantParams{
				MerchantID: l.merchant, GrantID: g.ID, AsOf: importTermination.StartsAt,
			})
		}
		if terminated && importTermination == nil {
			_, err := l.q.RevokeProductAccessByGrant(ctx, gen.RevokeProductAccessByGrantParams{
				MerchantID: l.merchant, GrantID: g.ID, RevokedAt: l.now(), RevokeReason: "grant_revoked",
			})
			return err
		}
		if g.ProductID == nil || g.SourceID == nil {
			return fmt.Errorf("grants: access grant %s lacks its product or source", g.ID)
		}
		revoked, err := l.q.RefundRevokedAccessWindow(ctx, gen.RefundRevokedAccessWindowParams{
			MerchantID: l.merchant, CustomerID: g.CustomerID, SourceID: *g.SourceID, PaymentID: g.PaymentID,
			ProductID: *g.ProductID, SourceType: g.SourceType, StartsAt: g.StartsAt, EndsAt: g.EndsAt,
		})
		if err != nil {
			return err
		}
		if revoked {
			if _, err := l.Revoke(ctx, g.ID, "refund", ""); err != nil {
				return err
			}
			_, err := l.q.RevokeProductAccessByGrant(ctx, gen.RevokeProductAccessByGrantParams{
				MerchantID: l.merchant, GrantID: g.ID, RevokedAt: l.now(), RevokeReason: "refund",
			})
			return err
		}
		if err := l.q.MaterializeProductAccess(ctx, gen.MaterializeProductAccessParams{
			MerchantID: l.merchant, CustomerID: g.CustomerID, ProductID: *g.ProductID, GrantID: g.ID,
			SourceType: g.SourceType, SourceID: *g.SourceID, PaymentID: g.PaymentID,
			StartsAt: start, EndsAt: g.EndsAt, Quantity: g.Quantity,
		}); err != nil {
			return fmt.Errorf("grants: materialize product access: %w", err)
		}
		if importTermination != nil {
			return l.q.BoundImportedProductAccessByGrant(ctx, gen.BoundImportedProductAccessByGrantParams{
				MerchantID: l.merchant, GrantID: g.ID, AsOf: importTermination.StartsAt,
			})
		}
		return nil

	case Entitlement, Ownership:
		return nil

	default:
		return fmt.Errorf("grants: unknown kind %q", g.Kind)
	}
}

// RevokeBySourceAsOf appends a revoke event to every LIVE grant of the customer
// that matches kind, one of sourceTypes and sourceID (a subscription or
// payment id string): the ledger side of a source-keyed retraction, so derive
// sees a terminated grant beside its retracted window. Already-terminated
// grants are skipped. asOf is the effective instant (valid time); zero is now.
func (l *Ledger) RevokeBySourceAsOf(ctx context.Context, customer uuid.UUID, kind Kind, sourceTypes []SourceType, sourceID, reason string, asOf time.Time) error {
	types := make([]string, len(sourceTypes))
	for i, st := range sourceTypes {
		types[i] = string(st)
	}
	live, err := l.q.ListLiveGrantsBySource(ctx, gen.ListLiveGrantsBySourceParams{
		MerchantID: l.merchant, CustomerID: customer, Kind: string(kind), SourceTypes: types, SourceID: sourceID,
	})
	if err != nil {
		return fmt.Errorf("grants: list grants for revoke-by-source: %w", err)
	}
	for _, g := range live {
		if _, err := l.RevokeAsOf(ctx, g.ID, reason, "", asOf); err != nil {
			return fmt.Errorf("grants: revoke %s by source: %w", g.ID, err)
		}
	}
	return nil
}

// The DERIVE detections below are single set queries: customer nil reads the
// whole merchant, non-nil one customer (its convergence job). Each returns at
// most limit rows, in the order a draining pass repairs them.

// MissingEffects returns live grants whose derived effects are not fully
// materialized (`derive.grant_effect.missing`). Repair = MaterializeGrant
// (idempotent).
func (l *Ledger) MissingEffects(ctx context.Context, customer *uuid.UUID, limit int32) ([]gen.BillingGrant, error) {
	return l.q.ListLiveGrantsMissingEffects(ctx, gen.ListLiveGrantsMissingEffectsParams{
		MerchantID: l.merchant, CustomerID: customer, RowLimit: limit,
	})
}

// UnretractedTerminations returns terminated grants whose window is still
// live (`derive.grant_effect.excess`). Repair = MaterializeGrant, which
// revokes the window, idempotently.
func (l *Ledger) UnretractedTerminations(ctx context.Context, customer *uuid.UUID, limit int32) ([]gen.BillingGrant, error) {
	return l.q.ListUnretractedTerminations(ctx, gen.ListUnretractedTerminationsParams{
		MerchantID: l.merchant, CustomerID: customer, RowLimit: limit,
	})
}

// UngrantedGrantablePayments returns completed, positive, one-off payments
// that produced no grant (`derive.grant.missing`). Surface-only.
func (l *Ledger) UngrantedGrantablePayments(ctx context.Context, customer *uuid.UUID, limit int32) ([]gen.ListUngrantedGrantablePaymentsRow, error) {
	return l.q.ListUngrantedGrantablePayments(ctx, gen.ListUngrantedGrantablePaymentsParams{
		MerchantID: l.merchant, CustomerID: customer, RowLimit: limit,
	})
}

// RefundedSourceGrants returns live grants whose backing payment was refunded
// (`derive.grant.excess`). Surface-only: a goodwill refund may keep access.
func (l *Ledger) RefundedSourceGrants(ctx context.Context, customer *uuid.UUID, limit int32) ([]gen.ListLiveGrantsWithRefundedPaymentRow, error) {
	return l.q.ListLiveGrantsWithRefundedPayment(ctx, gen.ListLiveGrantsWithRefundedPaymentParams{
		MerchantID: l.merchant, CustomerID: customer, RowLimit: limit,
	})
}

// derive-1 from stored sources: a migrate may insert subscriptions and wallet
// payments without grants, so these source-keyed detections create the grant
// and window from the bare source. Live data already has its grant.

// UngrantedSubscriptions returns active/canceled/unknown subscriptions for a
// grantable product with no subscription-sourced grant yet
// (`derive.subscription.missing`). Unknown fails open; chargeback cancels get
// no runway. scanSince bounds the scan to windows ending on/after it.
// customer nil = merchant-wide sweep.
func (l *Ledger) UngrantedSubscriptions(ctx context.Context, customer *uuid.UUID, scanSince time.Time, limit int32) ([]gen.ListUngrantedSubscriptionsRow, error) {
	return l.q.ListUngrantedSubscriptions(ctx, gen.ListUngrantedSubscriptionsParams{
		MerchantID: l.merchant, CustomerID: customer, ScanSince: scanSince, RowLimit: limit,
	})
}

// UngrantedWalletPayments returns completed solana wallet payments carrying a
// stored access window with no grant yet (`derive.wallet.missing`). customer
// nil = merchant-wide sweep.
func (l *Ledger) UngrantedWalletPayments(ctx context.Context, customer *uuid.UUID, scanSince time.Time, limit int32) ([]gen.ListUngrantedWalletPaymentsRow, error) {
	return l.q.ListUngrantedWalletPayments(ctx, gen.ListUngrantedWalletPaymentsParams{
		MerchantID: l.merchant, CustomerID: customer, ScanSince: scanSince, RowLimit: limit,
	})
}

// DeriveSubscriptionGrant creates the access grant and window for a
// subscription that has none (derive-1): the subscription's product for
// [COALESCE(period_start, started_at), + accepted access duration), or to the
// paid period end for a provider default; access state gating already happened
// in the detection query. Re-runnable: once a grant exists the detection
// excludes the subscription.
func (l *Ledger) DeriveSubscriptionGrant(ctx context.Context, sub gen.ListUngrantedSubscriptionsRow) error {
	start, end, ok := subscriptionWindow(sub.StartedAt, sub.CurrentPeriodStartsAt, sub.CurrentPeriodEndsAt, sub.AccessDurationHoursSnapshot, sub.ProviderAccessDefaultSnapshot)
	if !ok {
		return nil
	}
	_, err := l.deriveAccessWindow(ctx, customerWindow{Customer: sub.CustomerID, Product: sub.ProductID, Source: Subscription, SourceID: sub.ID.String(), Start: start, End: end})
	return err
}

// GrantSubscriptionWindow records a subscription's access grant for one
// window of its product and projects it, skipping a window already on the
// ledger. It reports whether it materialized one.
func (l *Ledger) GrantSubscriptionWindow(ctx context.Context, customer, subscription, product uuid.UUID, source SourceType, start time.Time, end *time.Time, quantity *int) (bool, error) {
	return l.deriveAccessWindow(ctx, customerWindow{Customer: customer, Product: product, Source: source, SourceID: subscription.String(), Start: start.UTC(), End: end, Quantity: quantity})
}

// DeriveWalletGrant creates the access grant and window for a solana wallet
// payment that has none (derive-1): [purchased_at, expiration_rfc3339),
// source purchase, payment-linked so the refund check sees it.
func (l *Ledger) DeriveWalletGrant(ctx context.Context, pay gen.ListUngrantedWalletPaymentsRow) error {
	if pay.ProductID == nil || !pay.ExpiresAt.After(pay.PurchasedAt) {
		return nil
	}
	pid := pay.ID
	exp := pay.ExpiresAt.UTC()
	_, err := l.deriveAccessWindow(ctx, customerWindow{Customer: pay.CustomerID, Product: *pay.ProductID, Source: Purchase, SourceID: pay.ID.String(), Payment: &pid, Start: pay.PurchasedAt.UTC(), End: &exp})
	return err
}

// GrantProduct records a free product grant (source grant) and projects its
// window, once per idempotency key: a replay returns the recorded grant with
// created=false. end nil is indefinite.
func (l *Ledger) GrantProduct(ctx context.Context, customer, product uuid.UUID, idempotencyKey string, start time.Time, end *time.Time, actor string, reason GrantReason, note *string) (gen.BillingGrant, bool, error) {
	g, created, err := l.GrantAccessOnce(ctx, GrantInput{
		Customer: customer, Product: &product, Kind: Access, Source: Granted, SourceID: idempotencyKey,
		StartsAt: start.UTC(), EndsAt: end, Reason: note, Actor: actor, GrantReason: reason,
	})
	if err != nil {
		return g, false, err
	}
	if created {
		if err := l.MaterializeGrant(ctx, g); err != nil {
			return g, false, err
		}
	}
	return g, created, nil
}

// ReconcileImportedProduct applies one source's declared comp inside an open
// transaction. The original root permanently owns the key; corrections append
// a termination and a replacement, preserving every declared window. A refusal
// returns a reason without changing the source; errors abort the transaction.
func (l *Ledger) ReconcileImportedProduct(ctx context.Context, customer, product uuid.UUID, source string, start time.Time, end *time.Time, removed bool, asOf time.Time) (changed bool, blockedReason string, err error) {
	if err := l.q.LockImportedGrantSource(ctx, gen.LockImportedGrantSourceParams{MerchantID: l.merchant, SourceID: source}); err != nil {
		return false, "", err
	}
	root, err := l.q.GetAccessGrantByIdempotencyKey(ctx, gen.GetAccessGrantByIdempotencyKeyParams{MerchantID: l.merchant, IdempotencyKey: source})
	var rootID *uuid.UUID
	if err == nil {
		if root.Actor == nil || *root.Actor != "import" || root.GrantReason == nil || *root.GrantReason != string(ReasonImport) {
			return false, "admin grant source_id is already owned by a non-import grant", nil
		}
		if root.CustomerID != customer || root.ProductID == nil || *root.ProductID != product {
			return false, "admin grant source_id cannot change customer or product", nil
		}
		current, err := l.q.GetLatestImportedGrant(ctx, gen.GetLatestImportedGrantParams{MerchantID: l.merchant, SourceID: source})
		if err != nil {
			return false, "", err
		}
		termination, err := l.q.GetGrantTermination(ctx, gen.GetGrantTerminationParams{MerchantID: l.merchant, GrantID: current.ID})
		terminated := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, "", err
		}
		sameWindow := current.StartsAt.Equal(start) && ((current.EndsAt == nil && end == nil) || (current.EndsAt != nil && end != nil && current.EndsAt.Equal(*end)))
		if (removed && terminated) || (!removed && !terminated && sameWindow) {
			return false, "", nil
		}
		horizon := current.ImportAsOf
		if terminated && (horizon == nil || termination.StartsAt.After(*horizon)) {
			horizon = &termination.StartsAt
		}
		if horizon != nil && !asOf.After(*horizon) {
			return false, "changed admin grant requires as_of later than its last change", nil
		}
		if !terminated {
			event, reason := "supersede", "import window changed"
			if removed {
				event, reason = "revoke", "import removed"
			}
			if _, err := l.terminate(ctx, current.ID, event, reason, "import", asOf); err != nil {
				return false, "", err
			}
			if err := l.MaterializeGrant(ctx, current); err != nil {
				return false, "", err
			}
		}
		if removed {
			return true, "", nil
		}
		rootID = &root.ID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", err
	} else if removed {
		return false, "", nil
	}
	g, err := l.q.InsertImportedGrant(ctx, gen.InsertImportedGrantParams{
		MerchantID: l.merchant, CustomerID: customer, ProductID: product, SourceID: source,
		StartsAt: start, EndsAt: end, ImportAsOf: asOf, ImportRootID: rootID,
	})
	if errors.Is(err, pgx.ErrNoRows) && rootID == nil {
		return false, "admin grant source_id is already in use", nil
	}
	if err != nil {
		return false, "", err
	}
	if err := l.MaterializeGrant(ctx, g); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// customerWindow is one source's access window: the product for
// [Start, End) (End nil = indefinite), keyed to (Source, SourceID).
type customerWindow struct {
	Customer uuid.UUID
	Product  uuid.UUID
	Source   SourceType
	SourceID string
	Payment  *uuid.UUID
	Start    time.Time
	End      *time.Time
	Quantity *int
}

// deriveAccessWindow records the source's access grant and asks derive-2 to
// project it. The grant is always recorded (detection keys on it);
// MaterializeGrant decides whether a window materializes. Replay is keyed by
// the exact interval.
func (l *Ledger) deriveAccessWindow(ctx context.Context, w customerWindow) (bool, error) {
	exists, err := l.q.AccessGrantWindowExists(ctx, gen.AccessGrantWindowExistsParams{
		MerchantID: l.merchant, CustomerID: w.Customer, ProductID: w.Product, SourceType: string(w.Source), SourceID: w.SourceID,
		StartsAt: w.Start, EndsAt: w.End,
	})
	if err != nil {
		return false, fmt.Errorf("grants: derive-1 replay check: %w", err)
	}
	if exists {
		return false, nil
	}
	product := w.Product
	g, created, err := l.GrantAccessOnce(ctx, GrantInput{
		Customer: w.Customer, Product: &product, Kind: Access, Source: w.Source, SourceID: w.SourceID, Payment: w.Payment,
		StartsAt: w.Start, EndsAt: w.End, Quantity: w.Quantity,
	})
	if err != nil {
		return false, fmt.Errorf("grants: derive-1 grant %s: %w", w.SourceID, err)
	}
	if !created {
		return false, nil
	}
	if err := l.MaterializeGrant(ctx, g); err != nil {
		return false, fmt.Errorf("grants: derive-1 materialize %s: %w", w.SourceID, err)
	}
	return true, nil
}

// subscriptionWindow uses the accepted access duration independently of
// billing; a provider default ends at the verified paid period end.
func subscriptionWindow(startedAt time.Time, periodStart, periodEnd *time.Time, hours *int32, providerDefault bool) (time.Time, *time.Time, bool) {
	start := startedAt
	if periodStart != nil && !periodStart.IsZero() {
		start = *periodStart
	}
	if start.IsZero() {
		return time.Time{}, nil, false
	}
	start = start.UTC()
	if providerDefault {
		// Never indefinite: no verified service end, no window.
		if periodEnd == nil || !periodEnd.After(start) {
			return time.Time{}, nil, false
		}
		end := periodEnd.UTC()
		return start, &end, true
	}
	if hours == nil {
		return start, nil, true
	}
	if *hours <= 0 {
		return time.Time{}, nil, false
	}
	end := start.Add(time.Duration(*hours) * time.Hour)
	return start, &end, true
}

// sourceIDOf is a grant's source id; "" when it has none (NULL).
func sourceIDOf(g gen.BillingGrant) string {
	if g.SourceID == nil {
		return ""
	}
	return *g.SourceID
}
