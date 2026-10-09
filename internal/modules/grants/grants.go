// Package grants is the #514 append-only grant ledger — the access-domain
// sibling of the #512 money ledger.
//
//   - derive-1 (Grant / Revoke / Expire / Supersede) appends immutable grant
//     events; it is the SOLE writer of billing.grants.
//   - derive-2 (Materialize) folds the grant log into projections: product
//     access windows in billing.product_access and credit lots as deposits.
//
// Grants are immutable: revoke/expire/supersede are NEW events referencing the
// original. An access grant gives a product for a window; the customer's keys
// are the product's at check time. A credit grant carries the lot
// amount+currency and IS the FIFO lot.
package grants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// Kind is what a grant confers.
type Kind string

const (
	Access Kind = "access"
	Credit Kind = "credit"
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
	// Admin is an operator's credit deposit.
	Admin SourceType = "admin"
)

// GrantReason says why a free product was granted.
type GrantReason string

const (
	ReasonComp      GrantReason = "comp"
	ReasonStaff     GrantReason = "staff"
	ReasonImport    GrantReason = "import"
	ReasonMigration GrantReason = "migration"
)

// Spec is captured on a credit grant at issuance, so derive-2 is a pure
// function of the grant. Historical entitlement grants carried their keys.
type Spec struct {
	Entitlements []string           `json:"entitlements,omitempty"`
	Deposit      *DepositProvenance `json:"deposit,omitempty"`
}

// DepositProvenance is recorded once with a credit deposit, never supplied by a replay.
type DepositProvenance struct {
	Source  string `json:"source"`
	Invoker string `json:"invoker"`
	// PaidAmount distinguishes purchased credits from manually granted deposits.
	PaidAmount *int64 `json:"paid_amount,string,omitempty"`
}

// Ledger is the append-only grant ledger for one merchant. It composes a #512
// money ledger over the same query handle so a credit grant and its deposit
// transfer commit together.
//
// Clock convention (#658 — name the clock):
//   - VALID TIME (effective/domain time) lives in starts_at/ends_at. It is the
//     time a fact is true in the domain, NOT when we recorded it — so it is
//     backdatable and the fold is a pure function of source facts, replayable at
//     any wall-clock. A grant event carries a real window [starts_at, ends_at];
//     a termination event (revoke/expire/supersede) is a WINDOW-LESS point event
//     whose effective instant is starts_at and whose ends_at is always NULL
//     (enforced by grants_termination_no_window_check).
//   - TRANSACTION TIME (when we wrote the row) lives in created_at. It is never
//     read as a business fact.
type Ledger struct {
	q        *gen.Queries
	merchant uuid.UUID
	money    *ledger.Ledger
	now      func() time.Time
}

// New binds a grant Ledger to a query handle and merchant. Compose it inside a
// pgx transaction for atomic derive-1 + derive-2.
func New(q *gen.Queries, merchant uuid.UUID) *Ledger {
	return &Ledger{
		q:        q,
		merchant: merchant,
		money:    ledger.New(q, merchant),
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the grant ledger's time source (event timestamps, FIFO/
// expiry "as-of"), so a caller that runs on an injected clock (e.g. the money
// service) derives consistently. nil restores the default real clock.
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
	Spec     *Spec
	StartsAt time.Time // zero => now
	EndsAt   *time.Time
	Amount   *int64  // credit lots
	Currency *string // credit lots
	Reason   *string // free-text provenance (or#906: a deposit's description)
	// Actor and GrantReason attribute a free product grant (Source Granted).
	Actor       string
	GrantReason GrantReason
}

// Grant appends a 'grant' event (derive-1). Call Materialize afterwards (or rely
// on the convergence sweep) to project it.
func (l *Ledger) Grant(ctx context.Context, in GrantInput) (gen.BillingGrant, error) {
	var spec []byte
	if in.Spec != nil {
		b, err := json.Marshal(in.Spec)
		if err != nil {
			return gen.BillingGrant{}, fmt.Errorf("grants: marshal spec: %w", err)
		}
		spec = b
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
	return l.q.InsertGrant(ctx, gen.InsertGrantParams{
		MerchantID: l.merchant, CustomerID: in.Customer, ProductID: in.Product,
		Kind: string(in.Kind), SourceType: string(in.Source), SourceID: in.SourceID, PaymentID: in.Payment,
		Event: "grant", SupersedesID: nil, SpecSnapshot: spec,
		StartsAt: starts, EndsAt: in.EndsAt, Amount: in.Amount, Currency: in.Currency,
		Reason: in.Reason, Actor: actor, GrantReason: reason,
	})
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
	g, err = l.q.InsertAccessGrantOnce(ctx, gen.InsertAccessGrantOnceParams{
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
func (l *Ledger) Revoke(ctx context.Context, grantID uuid.UUID, reason string) (gen.BillingGrant, error) {
	return l.terminate(ctx, grantID, "revoke", reason, time.Time{})
}

// RevokeAsOf is Revoke with an explicit EFFECTIVE revocation instant (valid time)
// recorded on the termination's starts_at — for converge-not-replay revocations
// (e.g. grace lapsed last Tuesday), so the grant ledger agrees with the entitlement
// effect instead of stamping convergence wall-clock. The zero Time means "now".
func (l *Ledger) RevokeAsOf(ctx context.Context, grantID uuid.UUID, reason string, asOf time.Time) (gen.BillingGrant, error) {
	return l.terminate(ctx, grantID, "revoke", reason, asOf)
}

// terminate appends a termination event (revoke/expire) superseding the grant.
// asOf is the effective revocation instant recorded on starts_at (valid time);
// the zero Time falls back to now(), mirroring Grant()'s zero-StartsAt handling.
// ends_at is ALWAYS NULL: a termination is a window-less point event (see the
// clock convention on Ledger), so it never trips grants_valid_window_check even when the
// grant it terminates already expired.
func (l *Ledger) terminate(ctx context.Context, grantID uuid.UUID, event, reason string, asOf time.Time) (gen.BillingGrant, error) {
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
	return l.q.InsertGrant(ctx, gen.InsertGrantParams{
		MerchantID: l.merchant, CustomerID: g.CustomerID, ProductID: g.ProductID,
		Kind: g.Kind, SourceType: g.SourceType, SourceID: sourceIDOf(g), PaymentID: g.PaymentID,
		Event: event, SupersedesID: &sup, SpecSnapshot: g.SpecSnapshot,
		StartsAt: effective, EndsAt: nil, Amount: g.Amount, Currency: g.Currency, Reason: &r,
	})
}

// MaterializeGrant projects a single grant event (derive-2): the access window
// of an access grant and the deposit of a credit grant. Terminated grants have
// their projection retracted. Historical entitlement and ownership grants have
// no projection.
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
		if terminated {
			_, err := l.q.RevokeProductAccessByGrant(ctx, gen.RevokeProductAccessByGrantParams{
				MerchantID: l.merchant, GrantID: g.ID, RevokedAt: l.now(), RevokeReason: "grant_revoked",
			})
			return err
		}
		if g.ProductID == nil || g.SourceID == nil {
			return fmt.Errorf("grants: access grant %s lacks its product or source", g.ID)
		}
		if err := l.q.MaterializeProductAccess(ctx, gen.MaterializeProductAccessParams{
			MerchantID: l.merchant, CustomerID: g.CustomerID, ProductID: *g.ProductID, GrantID: g.ID,
			SourceType: g.SourceType, SourceID: *g.SourceID, PaymentID: g.PaymentID,
			StartsAt: g.StartsAt, EndsAt: g.EndsAt,
		}); err != nil {
			return fmt.Errorf("grants: materialize product access: %w", err)
		}
		return nil

	case Credit:
		if terminated {
			return l.clawbackRevokedCredit(ctx, g)
		}
		deposited, err := l.q.GrantCreditDeposited(ctx, gen.GrantCreditDepositedParams{MerchantID: l.merchant, GrantID: g.ID})
		if err != nil {
			return err
		}
		if deposited {
			return nil
		}
		if g.Amount == nil || g.Currency == nil {
			return fmt.Errorf("grants: credit grant %s missing amount/currency", g.ID)
		}
		var spec Spec
		if len(g.SpecSnapshot) > 0 {
			if err := json.Unmarshal(g.SpecSnapshot, &spec); err != nil {
				return fmt.Errorf("grants: decode credit funding: %w", err)
			}
		}
		coord := ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: g.ID.String()}
		if spec.Deposit != nil && spec.Deposit.PaidAmount != nil {
			if err := l.money.PurchasedDeposit(ctx, g.CustomerID, *g.Currency, *g.Amount, *spec.Deposit.PaidAmount, coord, g.ID); err != nil {
				return err
			}
		} else if _, err := l.money.Deposit(ctx, g.CustomerID, *g.Currency, *g.Amount, coord, g.ID); err != nil {
			return fmt.Errorf("grants: materialize credit deposit: %w", err)
		}
		return nil

	case Entitlement, Ownership:
		return nil

	default:
		return fmt.Errorf("grants: unknown kind %q", g.Kind)
	}
}

// clawbackRevokedCredit retracts a revoked credit lot's UNSPENT remainder via a
// reversing transfer DR customer_balance / CR revoked_credits (the money is
// frozen there — recoverable/reversible — NOT refunded; a refund is a separate
// step). Idempotent: GetCreditLotRemaining nets out prior credit_revoke
// transfers, so a re-derive of an already-clawed lot moves nothing. (#514, see
// docs/consistency-invariants.md §11 decision 4.)
func (l *Ledger) clawbackRevokedCredit(ctx context.Context, g gen.BillingGrant) error {
	if g.Currency == nil {
		return fmt.Errorf("grants: revoked credit grant %s missing currency", g.ID)
	}
	remaining, err := l.q.GetCreditLotRemaining(ctx, gen.GetCreditLotRemainingParams{MerchantID: l.merchant, GrantID: g.ID})
	if err != nil {
		return fmt.Errorf("grants: lot remaining for %s: %w", g.ID, err)
	}
	if remaining <= 0 {
		return nil // fully spent/expired/already-clawed — nothing to retract
	}
	cust, err := l.money.EnsureCustomerBalance(ctx, g.CustomerID, *g.Currency)
	if err != nil {
		return err
	}
	rev, err := l.money.EnsureSystemAccount(ctx, ledger.RevokedCredits, *g.Currency)
	if err != nil {
		return err
	}
	lot, c := g.ID, g.CustomerID
	_, err = l.money.Apply(ctx, ledger.Transfer{
		Debit: cust, Credit: rev, Amount: remaining, Currency: *g.Currency, Type: ledger.CreditRevoke,
		Coord:   ledger.Coord{Operation: ledger.OpCreditRevoke, Source: "grant_revoke", SourceID: g.ID.String()},
		GrantID: &lot, Customer: &c,
	})
	return err
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
		if _, err := l.RevokeAsOf(ctx, g.ID, reason, asOf); err != nil {
			return fmt.Errorf("grants: revoke %s by source: %w", g.ID, err)
		}
	}
	return nil
}

// The four DERIVE detections below are single set queries (#575): `customer` nil
// sweeps the whole merchant (the convergence sweep — one anti-join, not one query
// per grant-holder), non-nil scopes to that customer (the inline AfterMutation
// path). Each query mirrors the previous per-grant Go detection exactly; the
// equivalence is pinned by the converge DERIVE integration tests.

// MissingEffects returns live grants whose derived grant effects are NOT fully
// materialized — the detection behind `derive.grant_effect.missing` (#511 DERIVE
// plane). Repair = MaterializeGrant (idempotent), so re-running converges to empty.
func (l *Ledger) MissingEffects(ctx context.Context, customer *uuid.UUID) ([]gen.BillingGrant, error) {
	return l.q.ListLiveGrantsMissingEffects(ctx, gen.ListLiveGrantsMissingEffectsParams{
		MerchantID: l.merchant, CustomerID: customer,
	})
}

// UnretractedTerminations returns TERMINATED grants whose derived effect is still
// live — the detection behind `derive.grant_effect.excess` (#511): a revoke/expire
// event was recorded but its retraction never propagated. Repair = MaterializeGrant,
// which retracts (entitlement → revoke window; credit → clawback) — idempotent.
func (l *Ledger) UnretractedTerminations(ctx context.Context, customer *uuid.UUID) ([]gen.BillingGrant, error) {
	return l.q.ListUnretractedTerminations(ctx, gen.ListUnretractedTerminationsParams{
		MerchantID: l.merchant, CustomerID: customer,
	})
}

// UngrantedGrantablePayments returns completed, positive, one-off payments
// that produced NO grant — the detection behind `derive.grant.missing` (grant
// tier, #511): every purchase grants its product or a credit lot. Surface-only.
func (l *Ledger) UngrantedGrantablePayments(ctx context.Context, customer *uuid.UUID) ([]gen.ListUngrantedGrantablePaymentsRow, error) {
	return l.q.ListUngrantedGrantablePayments(ctx, gen.ListUngrantedGrantablePaymentsParams{
		MerchantID: l.merchant, CustomerID: customer,
	})
}

// RefundedSourceGrants returns LIVE grants whose backing payment was refunded —
// the detection behind `derive.grant.excess` (grant tier, #511): the source no
// longer justifies the grant (money came back, access still live). Surface-only —
// an operator decides (a goodwill refund may intentionally keep access).
func (l *Ledger) RefundedSourceGrants(ctx context.Context, customer *uuid.UUID) ([]gen.ListLiveGrantsWithRefundedPaymentRow, error) {
	return l.q.ListLiveGrantsWithRefundedPayment(ctx, gen.ListLiveGrantsWithRefundedPaymentParams{
		MerchantID: l.merchant, CustomerID: customer,
	})
}

// --- #631 derive-1 from stored sources -------------------------------------
//
// derive-1 today only repairs EXISTING grants (MissingEffects/Unretracted) and
// SURFACES ungranted one-off payments for an operator. After the migrate/
// convergence split the host-one migrate inserts source-of-truth subscriptions +
// solana wallet payments but NO grants/entitlements (#724) — so the engine must
// CREATE the grant + entitlement window from the bare source. These detections
// are source-keyed (source_type+source_id), so they are a NO-OP for live data
// (which already carries its grant) and only fire on the migrated cohort.

// UngrantedSubscriptions returns active/canceled/unknown subscriptions for a
// grantable product with no subscription-sourced grant yet — the detection behind
// `derive.subscription.missing` (#631). #716 fail-open: `unknown` sources too, so
// the standing-access lane can engage for imported unknowns. #717: chargeback
// cancels are excluded — no runway. scanSince bounds the scan to
// windows ending on/after it (3y). customer nil = merchant-wide sweep.
func (l *Ledger) UngrantedSubscriptions(ctx context.Context, customer *uuid.UUID, scanSince time.Time) ([]gen.ListUngrantedSubscriptionsRow, error) {
	return l.q.ListUngrantedSubscriptions(ctx, gen.ListUngrantedSubscriptionsParams{
		MerchantID: l.merchant, CustomerID: customer, ScanSince: scanSince,
	})
}

// UngrantedWalletPayments returns completed solana wallet payments carrying a
// stored access window with no grant yet — the detection behind
// `derive.wallet.missing` (#631). customer nil = merchant-wide sweep.
func (l *Ledger) UngrantedWalletPayments(ctx context.Context, customer *uuid.UUID, scanSince time.Time) ([]gen.ListUngrantedWalletPaymentsRow, error) {
	return l.q.ListUngrantedWalletPayments(ctx, gen.ListUngrantedWalletPaymentsParams{
		MerchantID: l.merchant, CustomerID: customer, ScanSince: scanSince,
	})
}

// DeriveSubscriptionGrant creates the access grant and window for a
// subscription that has none (derive-1): the subscription's product for
// [COALESCE(period_start, started_at), + accepted access duration); access
// state gating already happened in the detection query. Re-runnable: once a
// grant exists the detection excludes the subscription.
func (l *Ledger) DeriveSubscriptionGrant(ctx context.Context, sub gen.ListUngrantedSubscriptionsRow) error {
	start, end, ok := subscriptionWindow(sub.StartedAt, sub.CurrentPeriodStartsAt, sub.AccessDurationHoursSnapshot)
	if !ok {
		return nil
	}
	_, err := l.deriveAccessWindow(ctx, customerWindow{Customer: sub.CustomerID, Product: sub.ProductID, Source: Subscription, SourceID: sub.ID.String(), Start: start, End: end})
	return err
}

// GrantSubscriptionWindow records a subscription's access grant for one
// window of its product and projects it, skipping a window already on the
// ledger. It reports whether it materialized one.
func (l *Ledger) GrantSubscriptionWindow(ctx context.Context, customer, subscription, product uuid.UUID, source SourceType, start time.Time, end *time.Time) (bool, error) {
	return l.deriveAccessWindow(ctx, customerWindow{Customer: customer, Product: product, Source: source, SourceID: subscription.String(), Start: start.UTC(), End: end})
}

// DeriveWalletGrant creates the access grant and window for a solana wallet
// payment that has none (derive-1): [purchased_at, expiration_rfc3339),
// source purchase, payment-linked so the refund check sees it.
func (l *Ledger) DeriveWalletGrant(ctx context.Context, pay gen.ListUngrantedWalletPaymentsRow) error {
	if !pay.ExpiresAt.After(pay.PurchasedAt) {
		return nil
	}
	pid := pay.ID
	exp := pay.ExpiresAt.UTC()
	_, err := l.deriveAccessWindow(ctx, customerWindow{Customer: pay.CustomerID, Product: pay.ProductID, Source: Purchase, SourceID: pay.ID.String(), Payment: &pid, Start: pay.PurchasedAt.UTC(), End: &exp})
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
}

// deriveAccessWindow records the source's access grant and asks derive-2 to
// project it. The GRANT is provenance — recorded UNCONDITIONALLY (#695:
// detection keys on grant existence); whether a WINDOW materializes is
// MaterializeGrant's decision. Replay is keyed by the exact interval.
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
		StartsAt: w.Start, EndsAt: w.End,
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

// subscriptionWindow uses the accepted access duration independently of billing.
func subscriptionWindow(startedAt time.Time, periodStart *time.Time, hours *int32) (time.Time, *time.Time, bool) {
	start := startedAt
	if periodStart != nil && !periodStart.IsZero() {
		start = *periodStart
	}
	if start.IsZero() {
		return time.Time{}, nil, false
	}
	start = start.UTC()
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
