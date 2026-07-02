// Package grants is the #514 append-only grant ledger — the access-domain
// sibling of the #512 money ledger.
//
//   - derive-1 (Grant / Revoke / Expire / Supersede) appends immutable grant
//     events; it is the SOLE writer of openrails.grants.
//   - derive-2 (Materialize) folds the grant log into projections: entitlement
//     windows in openrails.entitlements, credit lots as #512 ledger deposits, and
//     derived ownership grants for catalog bundle includes.
//
// Grants are immutable: revoke/expire/supersede are NEW events referencing the
// original. A credit grant carries the lot amount+currency and IS the FIFO lot.
package grants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Kind is what a grant confers.
type Kind string

const (
	Entitlement Kind = "entitlement"
	Ownership   Kind = "ownership"
	Credit      Kind = "credit"
)

// SourceType is the origin of a grant.
type SourceType string

const (
	Purchase     SourceType = "purchase"
	Subscription SourceType = "subscription"
	Admin        SourceType = "admin"
	Grace        SourceType = "grace"
)

// Spec is the product spec snapshot captured on a grant at issuance, so derive-2
// is a pure function of the grant (exact + historical replay).
type Spec struct {
	Entitlements []string `json:"entitlements,omitempty"`
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
//     (enforced by grants_termination_no_window).
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
}

// Grant appends a 'grant' event (derive-1). Call Materialize afterwards (or rely
// on the convergence sweep) to project it.
func (l *Ledger) Grant(ctx context.Context, in GrantInput) (gen.OpenrailsGrant, error) {
	var spec []byte
	if in.Spec != nil {
		b, err := json.Marshal(in.Spec)
		if err != nil {
			return gen.OpenrailsGrant{}, fmt.Errorf("grants: marshal spec: %w", err)
		}
		spec = b
	}
	starts := in.StartsAt
	if starts.IsZero() {
		starts = l.now()
	}
	return l.q.InsertGrant(ctx, gen.InsertGrantParams{
		MerchantID: l.merchant, CustomerID: in.Customer, ProductID: in.Product,
		Kind: string(in.Kind), SourceType: string(in.Source), SourceID: in.SourceID, PaymentID: in.Payment,
		Event: "grant", SupersedesID: nil, SpecSnapshot: spec,
		StartsAt: starts, EndsAt: in.EndsAt, Amount: in.Amount, Currency: in.Currency,
	})
}

// Revoke appends a 'revoke' event terminating the grant (derive-1), effective now.
// The grant row is never edited. A grant may be terminated at most once (unique index).
func (l *Ledger) Revoke(ctx context.Context, grantID uuid.UUID, reason string) (gen.OpenrailsGrant, error) {
	return l.terminate(ctx, grantID, "revoke", reason, time.Time{})
}

// RevokeAsOf is Revoke with an explicit EFFECTIVE revocation instant (valid time)
// recorded on the termination's starts_at — for converge-not-replay revocations
// (e.g. grace lapsed last Tuesday), so the grant ledger agrees with the entitlement
// effect instead of stamping convergence wall-clock. The zero Time means "now".
func (l *Ledger) RevokeAsOf(ctx context.Context, grantID uuid.UUID, reason string, asOf time.Time) (gen.OpenrailsGrant, error) {
	return l.terminate(ctx, grantID, "revoke", reason, asOf)
}

// terminate appends a termination event (revoke/expire) superseding the grant.
// asOf is the effective revocation instant recorded on starts_at (valid time);
// the zero Time falls back to now(), mirroring Grant()'s zero-StartsAt handling.
// ends_at is ALWAYS NULL: a termination is a window-less point event (see the
// clock convention on Ledger), so it never trips grants_valid_window even when the
// grant it terminates already expired.
func (l *Ledger) terminate(ctx context.Context, grantID uuid.UUID, event, reason string, asOf time.Time) (gen.OpenrailsGrant, error) {
	g, err := l.q.GetGrant(ctx, gen.GetGrantParams{MerchantID: l.merchant, ID: grantID})
	if err != nil {
		return gen.OpenrailsGrant{}, fmt.Errorf("grants: load grant %s: %w", grantID, err)
	}
	if g.Event != "grant" {
		return gen.OpenrailsGrant{}, fmt.Errorf("grants: %s is a %q event, not a grant", grantID, g.Event)
	}
	effective := asOf
	if effective.IsZero() {
		effective = l.now()
	}
	sup := grantID
	r := reason
	return l.q.InsertGrant(ctx, gen.InsertGrantParams{
		MerchantID: l.merchant, CustomerID: g.CustomerID, ProductID: g.ProductID,
		Kind: g.Kind, SourceType: g.SourceType, SourceID: g.SourceID, PaymentID: g.PaymentID,
		Event: event, SupersedesID: &sup, SpecSnapshot: g.SpecSnapshot,
		StartsAt: effective, EndsAt: nil, Amount: g.Amount, Currency: g.Currency, Reason: &r,
	})
}

// MaterializeGrant projects a single grant event (derive-2): entitlement windows
// for entitlement grants, a #512 deposit for credit grants, and child ownership
// grants for catalog bundle includes. Terminated grants have their projection retracted.
// entitlementSourceType bridges the grant source vocabulary (purchase/
// subscription/admin/grace) to the entitlements table's vocabulary
// (subscription/one_off/admin/grace): a `purchase`-sourced grant projects an
// `one_off` entitlement; the others pass through unchanged.
func entitlementSourceType(grantSource string) string {
	if grantSource == "purchase" {
		return "one_off"
	}
	return grantSource
}

func (l *Ledger) MaterializeGrant(ctx context.Context, g gen.OpenrailsGrant) error {
	if g.Event != "grant" {
		return fmt.Errorf("grants: MaterializeGrant needs a grant event, got %q", g.Event)
	}
	terminated, err := l.q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: l.merchant, GrantID: g.ID})
	if err != nil {
		return fmt.Errorf("grants: termination check: %w", err)
	}
	if err := l.materializeUsageLimitBindings(ctx, g, terminated); err != nil {
		return err
	}

	switch Kind(g.Kind) {
	case Entitlement:
		if terminated {
			_, err := l.q.RevokeEntitlementsByGrant(ctx, gen.RevokeEntitlementsByGrantParams{
				MerchantID: l.merchant, GrantID: g.ID, RevokedAt: l.now(), RevokeReason: "grant_revoked",
			})
			return err
		}
		feats, err := specFeatures(g.SpecSnapshot)
		if err != nil {
			return err
		}
		for _, f := range feats {
			exists, err := l.q.EntitlementExistsForGrant(ctx, gen.EntitlementExistsForGrantParams{
				MerchantID: l.merchant, GrantID: g.ID, Entitlement: f,
			})
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			gid := g.ID
			// #511: the entitlement keeps its SEMANTIC source (so source-keyed
			// readers — revoke-by-subscription, grace, one-off, the CON checks —
			// work unchanged) and links to its grant via grant_id (DERIVE's
			// authoritative link). Two vocabulary bridges: grants say 'purchase',
			// entitlements say 'one_off'; and grant.source_id is free text while
			// entitlements.source_id is a uuid, so parse it (a real subscription/
			// payment source is a uuid → revoke-by-source resolves; a non-uuid
			// source falls back to the grant id).
			entSourceID := gid
			if parsed, perr := uuid.Parse(g.SourceID); perr == nil {
				entSourceID = parsed
			}
			if _, err := l.q.CreateEntitlement(ctx, gen.CreateEntitlementParams{
				Entitlement: f, StartAt: g.StartsAt, SourceType: entitlementSourceType(g.SourceType),
				MerchantID: l.merchant, CustomerID: g.CustomerID, EndAt: g.EndsAt,
				SourceID: &entSourceID, GrantID: &gid,
			}); err != nil {
				return fmt.Errorf("grants: materialize entitlement %q: %w", f, err)
			}
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
		if _, err := l.money.Deposit(ctx, g.CustomerID, *g.Currency, *g.Amount, "grant", g.ID.String(), g.ID); err != nil {
			return fmt.Errorf("grants: materialize credit deposit: %w", err)
		}
		return nil

	case Ownership:
		return l.materializeOwnershipIncludes(ctx, g, terminated)

	default:
		return fmt.Errorf("grants: unknown kind %q", g.Kind)
	}
}

func (l *Ledger) materializeUsageLimitBindings(ctx context.Context, g gen.OpenrailsGrant, terminated bool) error {
	if g.ProductID == nil {
		return nil
	}
	if terminated {
		return l.q.RevokeProductUsageLimitBindingsByGrant(ctx, gen.RevokeProductUsageLimitBindingsByGrantParams{
			MerchantID: l.merchant,
			GrantID:    g.ID,
			RevokedAt:  l.now(),
		})
	}
	specs, err := l.q.ListProductUsageLimitSpecs(ctx, gen.ListProductUsageLimitSpecsParams{
		MerchantID: l.merchant,
		ProductID:  *g.ProductID,
	})
	if err != nil {
		return fmt.Errorf("grants: list product usage limits for %s: %w", g.ID, err)
	}
	for _, spec := range specs {
		exists, err := l.q.ProductUsageLimitBindingExistsForGrant(ctx, gen.ProductUsageLimitBindingExistsForGrantParams{
			MerchantID:    l.merchant,
			GrantID:       g.ID,
			UsageLimitKey: spec.UsageLimitKey,
		})
		if err != nil {
			return fmt.Errorf("grants: usage-limit binding lookup %q: %w", spec.UsageLimitKey, err)
		}
		if exists {
			continue
		}
		sourceID := g.ID
		if parsed, perr := uuid.Parse(g.SourceID); perr == nil {
			sourceID = parsed
		}
		productKey := spec.ProductKey
		grantID := g.ID
		if err := l.q.CreateProductUsageLimitBinding(ctx, gen.CreateProductUsageLimitBindingParams{
			ID:            uuidutil.NewV7(),
			MerchantID:    l.merchant,
			CustomerID:    g.CustomerID,
			UsageLimitKey: spec.UsageLimitKey,
			Measure:       spec.Measure,
			Windows:       spec.Windows,
			SourceType:    g.SourceType,
			SourceID:      &sourceID,
			ProductKey:    &productKey,
			GrantID:       &grantID,
			StartsAt:      g.StartsAt,
			EndsAt:        g.EndsAt,
			PolicyVersion: 1,
		}); err != nil {
			return fmt.Errorf("grants: materialize usage-limit binding %q: %w", spec.UsageLimitKey, err)
		}
	}
	return nil
}

func (l *Ledger) materializeOwnershipIncludes(ctx context.Context, g gen.OpenrailsGrant, terminated bool) error {
	if g.ProductID == nil {
		return nil
	}
	included, err := l.q.ListIncludedProductIDs(ctx, gen.ListIncludedProductIDsParams{
		MerchantID: l.merchant,
		ProductID:  *g.ProductID,
	})
	if err != nil {
		return fmt.Errorf("grants: list included products for %s: %w", g.ID, err)
	}
	for _, productID := range included {
		if terminated {
			if err := l.revokeIncludedOwnership(ctx, g, productID); err != nil {
				return err
			}
			continue
		}
		if err := l.grantIncludedOwnership(ctx, g, productID); err != nil {
			return err
		}
	}
	return nil
}

func (l *Ledger) grantIncludedOwnership(ctx context.Context, parent gen.OpenrailsGrant, productID uuid.UUID) error {
	sourceID := includedOwnershipSourceID(parent.ID, productID)
	_, err := l.q.GetOwnershipGrantBySourceID(ctx, gen.GetOwnershipGrantBySourceIDParams{
		MerchantID: l.merchant,
		CustomerID: parent.CustomerID,
		ProductID:  productID,
		SourceID:   sourceID,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("grants: lookup included ownership %s: %w", productID, err)
	}
	_, err = l.Grant(ctx, GrantInput{
		Customer: parent.CustomerID,
		Product:  &productID,
		Kind:     Ownership,
		Source:   SourceType(parent.SourceType),
		SourceID: sourceID,
		Payment:  parent.PaymentID,
		StartsAt: parent.StartsAt,
		EndsAt:   parent.EndsAt,
	})
	if err != nil {
		return fmt.Errorf("grants: materialize included ownership %s: %w", productID, err)
	}
	return nil
}

func (l *Ledger) revokeIncludedOwnership(ctx context.Context, parent gen.OpenrailsGrant, productID uuid.UUID) error {
	sourceID := includedOwnershipSourceID(parent.ID, productID)
	child, err := l.q.GetOwnershipGrantBySourceID(ctx, gen.GetOwnershipGrantBySourceIDParams{
		MerchantID: l.merchant,
		CustomerID: parent.CustomerID,
		ProductID:  productID,
		SourceID:   sourceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("grants: lookup included ownership %s for revoke: %w", productID, err)
	}
	childTerminated, err := l.q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{
		MerchantID: l.merchant,
		GrantID:    child.ID,
	})
	if err != nil {
		return fmt.Errorf("grants: included ownership termination check %s: %w", child.ID, err)
	}
	if childTerminated {
		return nil
	}
	if _, err := l.Revoke(ctx, child.ID, "bundle_parent_revoked"); err != nil {
		return fmt.Errorf("grants: revoke included ownership %s: %w", child.ID, err)
	}
	return nil
}

func includedOwnershipSourceID(parentGrantID, productID uuid.UUID) string {
	return "include:" + parentGrantID.String() + ":" + productID.String()
}

// clawbackRevokedCredit retracts a revoked credit lot's UNSPENT remainder via a
// reversing transfer DR customer_balance / CR revoked_credits (the money is
// frozen there — recoverable/reversible — NOT refunded; a refund is a separate
// step). Idempotent: GetCreditLotRemaining nets out prior credit_revoke
// transfers, so a re-derive of an already-clawed lot moves nothing. (#514, see
// docs/consistency-invariants.md §11 decision 4.)
func (l *Ledger) clawbackRevokedCredit(ctx context.Context, g gen.OpenrailsGrant) error {
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
	src, sid, lot, c := "grant_revoke", g.ID.String(), g.ID, g.CustomerID
	_, err = l.money.Apply(ctx, ledger.Transfer{
		Debit: cust, Credit: rev, Amount: remaining, Currency: *g.Currency, Type: "credit_revoke",
		Source: &src, SourceID: &sid, GrantID: &lot, Customer: &c,
	})
	return err
}

// RevokeBySourceAsOf appends a revoke event to every LIVE grant of the customer
// that matches `kind` + one of `sourceTypes` + `sourceID` — the write-path
// companion to a source-keyed effect revocation, keeping the grant ledger the
// source of truth (the grant is terminated, so DERIVE sees a terminated grant
// with a separately retracted effect — consistent). Idempotent: an
// already-terminated grant is skipped. `sourceID` is compared as the grant's
// free-text source_id (e.g. a subscription or payment UUID string). asOf is the
// effective revocation instant threaded onto each termination's starts_at
// (valid time); the zero Time means "now".
func (l *Ledger) RevokeBySourceAsOf(ctx context.Context, customer uuid.UUID, kind Kind, sourceTypes []SourceType, sourceID, reason string, asOf time.Time) error {
	all, err := l.q.ListGrantsByCustomer(ctx, gen.ListGrantsByCustomerParams{MerchantID: l.merchant, CustomerID: customer})
	if err != nil {
		return fmt.Errorf("grants: list grants for revoke-by-source: %w", err)
	}
	want := make(map[string]bool, len(sourceTypes))
	for _, st := range sourceTypes {
		want[string(st)] = true
	}
	for i := range all {
		g := all[i]
		if g.Event != "grant" || Kind(g.Kind) != kind || !want[g.SourceType] || g.SourceID != sourceID {
			continue
		}
		terminated, err := l.q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: l.merchant, GrantID: g.ID})
		if err != nil {
			return fmt.Errorf("grants: termination check for %s: %w", g.ID, err)
		}
		if terminated {
			continue
		}
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
func (l *Ledger) MissingEffects(ctx context.Context, customer *uuid.UUID) ([]gen.OpenrailsGrant, error) {
	return l.q.ListLiveGrantsMissingEffects(ctx, gen.ListLiveGrantsMissingEffectsParams{
		MerchantID: l.merchant, CustomerID: customer,
	})
}

// UnretractedTerminations returns TERMINATED grants whose derived effect is still
// live — the detection behind `derive.grant_effect.excess` (#511): a revoke/expire
// event was recorded but its retraction never propagated. Repair = MaterializeGrant,
// which retracts (entitlement → revoke window; credit → clawback) — idempotent.
func (l *Ledger) UnretractedTerminations(ctx context.Context, customer *uuid.UUID) ([]gen.OpenrailsGrant, error) {
	return l.q.ListUnretractedTerminations(ctx, gen.ListUnretractedTerminationsParams{
		MerchantID: l.merchant, CustomerID: customer,
	})
}

// UngrantedGrantablePayments returns completed, positive, one-off payments for a
// product that PROMISES grants (non-empty entitlements/credits spec) yet produced
// NO grant — the spec-aware detection behind `derive.grant.missing` (grant tier,
// #511). Empty-spec products / pure fees are never flagged. Surface-only.
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

func specFeatures(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("grants: parse spec_snapshot: %w", err)
	}
	return s.Entitlements, nil
}

// --- #631 derive-1 from stored sources -------------------------------------
//
// derive-1 today only repairs EXISTING grants (MissingEffects/Unretracted) and
// SURFACES ungranted one-off payments for an operator. After the migrate/
// convergence split the doujins migrate inserts source-of-truth subscriptions +
// solana wallet payments but NO grants/entitlements (#724) — so the engine must
// CREATE the grant + entitlement window from the bare source. These detections
// are source-keyed (source_type+source_id), so they are a NO-OP for live data
// (which already carries its grant) and only fire on the migrated cohort.

// UngrantedSubscriptions returns active/cancelled subscriptions for a grantable
// product with no subscription-sourced grant yet — the detection behind
// `derive.subscription.missing` (#631). scanSince bounds the scan to
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

// DeriveSubscriptionGrant creates the entitlement grant(s) + window for a
// subscription that has none (derive-1). The window is the subscription period
// [COALESCE(period_start,started_at), COALESCE(period_end,ended_at)); access-state
// gating already happened in the detection query. Re-runnable: once a grant
// exists the detection excludes the sub.
func (l *Ledger) DeriveSubscriptionGrant(ctx context.Context, sub gen.ListUngrantedSubscriptionsRow) error {
	start, end, ok := subscriptionWindow(sub)
	if !ok {
		return nil
	}
	_, err := l.deriveEntitlementWindows(ctx, customerWindow{Customer: sub.CustomerID, Source: Subscription, SourceID: sub.ID.String(), Feats: productSpecKeys(sub.EntitlementsSpec), Start: start, End: &end})
	return err
}

// DeriveWalletGrant creates the entitlement grant(s) + window for a solana wallet
// payment that has none (derive-1). Window = [purchased_at, expiration_rfc3339);
// grant source is `purchase` (→ `one_off` entitlement), payment-linked so the
// refund check sees it.
func (l *Ledger) DeriveWalletGrant(ctx context.Context, pay gen.ListUngrantedWalletPaymentsRow) error {
	if !pay.ExpiresAt.After(pay.PurchasedAt) {
		return nil
	}
	pid := pay.ID
	exp := pay.ExpiresAt.UTC()
	_, err := l.deriveEntitlementWindows(ctx, customerWindow{Customer: pay.CustomerID, Source: Purchase, SourceID: pay.ID.String(), Payment: &pid, Feats: productSpecKeys(pay.EntitlementsSpec), Start: pay.PurchasedAt.UTC(), End: &exp})
	return err
}

// AdminGrantExists reports whether an entitlement grant from this admin source is
// already recorded — the #636 idempotency check for the admin-comp import path.
func (l *Ledger) AdminGrantExists(ctx context.Context, sourceID string) (bool, error) {
	return l.q.AdminGrantExistsForSource(ctx, gen.AdminGrantExistsForSourceParams{MerchantID: l.merchant, SourceID: sourceID})
}

// GrantAdmin records an operator/manual "comp" as a source-of-truth admin grant
// (source_type=admin) + materializes its entitlement window(s) — derive-1 for the
// access FACT that has no payment/subscription behind it (#636). The host (e.g. the
// doujins legacy migrate) hands over the comp instead of writing entitlements.
// Idempotent by sourceID. end nil = indefinite. Per-feature overlap-skip, like the
// other derive-1 paths. Returns the number of feature-windows created (0 = every
// feature overlapped an existing live window, i.e. blocked) and whether the source
// was already imported (idempotent skip — no write).
func (l *Ledger) GrantAdmin(ctx context.Context, customer uuid.UUID, sourceID string, feats []string, start time.Time, end *time.Time) (created int, alreadyExists bool, err error) {
	exists, err := l.AdminGrantExists(ctx, sourceID)
	if err != nil {
		return 0, false, err
	}
	if exists {
		return 0, true, nil
	}
	created, err = l.deriveEntitlementWindows(ctx, customerWindow{Customer: customer, Source: Admin, SourceID: sourceID, Feats: feats, Start: start.UTC(), End: end})
	return created, false, err
}

// customerWindow is one source's derived access window: grant N entitlement
// features for [Start, End) (End nil = indefinite), keyed to (Source, SourceID).
type customerWindow struct {
	Customer uuid.UUID
	Source   SourceType
	SourceID string
	Payment  *uuid.UUID
	Feats    []string
	Start    time.Time
	End      *time.Time
}

// deriveEntitlementWindows grants + materializes one entitlement window per
// feature, SKIPPING any feature that overlaps an existing live entitlement (so a
// CreateEntitlement can never trip the no-overlap exclusion constraint). One grant
// per (source, feature), mirroring the live entitlement path. The overlap check
// sees rows committed earlier in the same converge run (each create auto-commits
// on the shared conn), so sequential derives within a run stay consistent.
// ponytail: v1 drops a feature's window entirely when it overlaps; #631-followup
// (O2) clips/merges partial overlaps. A fully-overlapped sub re-fires each sweep
// until the overlapping window expires, then converges — bounded, self-healing.
func (l *Ledger) deriveEntitlementWindows(ctx context.Context, w customerWindow) (int, error) {
	upper := overlapUpperBound(w.End)
	created := 0
	for _, f := range w.Feats {
		overlaps, err := l.q.EntitlementWindowOverlaps(ctx, gen.EntitlementWindowOverlapsParams{
			MerchantID: l.merchant, CustomerID: w.Customer, Entitlement: f, LowerBound: w.Start, UpperBound: upper,
		})
		if err != nil {
			return created, fmt.Errorf("grants: derive-1 overlap check %q: %w", f, err)
		}
		if overlaps {
			continue
		}
		g, err := l.Grant(ctx, GrantInput{
			Customer: w.Customer, Kind: Entitlement, Source: w.Source, SourceID: w.SourceID, Payment: w.Payment,
			Spec: &Spec{Entitlements: []string{f}}, StartsAt: w.Start, EndsAt: w.End,
		})
		if err != nil {
			return created, fmt.Errorf("grants: derive-1 grant %s/%q: %w", w.SourceID, f, err)
		}
		if err := l.MaterializeGrant(ctx, g); err != nil {
			return created, fmt.Errorf("grants: derive-1 materialize %s/%q: %w", w.SourceID, f, err)
		}
		created++
	}
	return created, nil
}

// overlapUpperBound is the EntitlementWindowOverlaps upper bound for a window: its
// end, or a practical-infinity sentinel for an indefinite (nil-end) window — the
// entitlement period's upper is 'infinity', so any real future window overlaps.
func overlapUpperBound(end *time.Time) time.Time {
	if end != nil {
		return *end
	}
	return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
}

// subscriptionWindow mirrors the retired migrate logic: start =
// current_period_starts_at ?? started_at, end = current_period_ends_at ??
// ended_at; valid only if end is strictly after start.
func subscriptionWindow(s gen.ListUngrantedSubscriptionsRow) (time.Time, time.Time, bool) {
	start := s.StartedAt
	if s.CurrentPeriodStartsAt != nil && !s.CurrentPeriodStartsAt.IsZero() {
		start = *s.CurrentPeriodStartsAt
	}
	var end time.Time
	switch {
	case s.CurrentPeriodEndsAt != nil && !s.CurrentPeriodEndsAt.IsZero():
		end = *s.CurrentPeriodEndsAt
	case s.EndedAt != nil && !s.EndedAt.IsZero():
		end = *s.EndedAt
	}
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return time.Time{}, time.Time{}, false
	}
	return start.UTC(), end.UTC(), true
}

// productSpecKeys returns the entitlement feature names — the keys of a product's
// entitlements_spec ({name: hours} JSONB). Mirrors the retired migrate's
// billingProductEntitlementNames (sorted, trimmed, empties dropped).
func productSpecKeys(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
