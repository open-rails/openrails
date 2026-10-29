// Package parking keeps the live provider subscriptions converge cannot land:
// an unmapped price, a customer linked to no one, a customer already holding
// the product. A park moves no money and cancels nothing; it raises a finding
// and waits. Fixing the cause wakes converge in the fixing transaction, and the
// park closes when the subscription lands by any path (a trigger on
// billing.subscriptions), so it lands exactly once. Park and Wake take the
// same advisory lock per cause: a park committed first is seen by the wake, a
// fix committed first is seen by the park.
package parking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/wakejobs"
)

const (
	FindingPriceUnmapped = "catalog.price.unmapped"
	FindingUnattributed  = "life.subscription.unattributed"
)

// ErrCauseFixed is a park whose cause was fixed between converge's read and
// the park: converge runs again instead.
var ErrCauseFixed = errors.New("park: its cause was fixed meanwhile; converge again")

// Subject is a live provider subscription converge cannot land, and why.
type Subject struct {
	PSPID           uuid.UUID
	Rail            string
	SubscriptionRef string
	Reason          billing.ParkReason
	PriceRef        string
	CustomerRef     string
	// HeldBy is the subscription a product_held customer already has.
	HeldBy *uuid.UUID
	// Unheld is a subscription no row holds: it parks only while none does,
	// and the backlog rule applies. While the PSP's import owns it (pulling,
	// or staging it unlanded) it parks without a finding, so a fix still
	// wakes it.
	Unheld   bool
	Evidence map[string]any
}

func (s Subject) cause() string {
	switch s.Reason {
	case billing.ParkPriceUnmapped:
		return s.PriceRef
	case billing.ParkCustomerUnlinked:
		return s.CustomerRef
	}
	return s.SubscriptionRef
}

// Result is what Park did. ImportOwned parks raise no finding. Finding is
// the finding it opened or raised, for the caller to notify.
type Result struct {
	ImportOwned bool
	Finding     *gen.BillingReconciliationFinding
}

// Park records s under its cause's lock. A cause fixed meanwhile, or an
// unheld subscription a row now holds, is ErrCauseFixed.
func Park(ctx context.Context, d *db.DB, s Subject, now time.Time) (Result, error) {
	var res Result
	mid, err := merchant.Require(ctx)
	if err != nil {
		return res, err
	}
	if s.PSPID == uuid.Nil || s.Rail == "" || s.SubscriptionRef == "" || (s.Reason == billing.ParkPriceUnmapped && s.PriceRef == "") {
		return res, fmt.Errorf("park %q: incomplete subject", s.SubscriptionRef)
	}
	if s.Evidence == nil {
		s.Evidence = map[string]any{}
	}
	s.Evidence["reason"] = s.Reason
	evidence, err := json.Marshal(s.Evidence)
	if err != nil {
		return res, err
	}
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		res = Result{}
		q := gen.New(tx)
		if err := q.LockParkCause(ctx, gen.LockParkCauseParams{MerchantID: mid.UUID(), PspID: s.PSPID, Reason: string(s.Reason), Ref: s.cause()}); err != nil {
			return err
		}
		if fixed, err := causeFixed(ctx, q, mid.UUID(), s); err != nil {
			return err
		} else if fixed {
			return ErrCauseFixed
		}
		if s.Unheld {
			// An import's Apply landed it meanwhile: converge it as held.
			if landed, err := q.SubscriptionLanded(ctx, gen.SubscriptionLandedParams{MerchantID: mid.UUID(), PspID: s.PSPID, SubscriptionRef: s.SubscriptionRef}); err != nil {
				return err
			} else if landed {
				return ErrCauseFixed
			}
			owned, err := importOwns(ctx, q, mid.UUID(), s)
			if err != nil {
				return err
			}
			res.ImportOwned = owned
		}
		prev, err := q.GetOpenPark(ctx, gen.GetOpenParkParams{MerchantID: mid.UUID(), PspID: s.PSPID, SubscriptionRef: s.SubscriptionRef})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		open := err == nil
		if _, err := q.OpenPark(ctx, gen.OpenParkParams{MerchantID: mid.UUID(), PspID: s.PSPID, Rail: s.Rail, SubscriptionRef: s.SubscriptionRef,
			Reason: string(s.Reason), PriceRef: nonEmpty(s.PriceRef), CustomerRef: nonEmpty(s.CustomerRef), ImportOwned: res.ImportOwned, Evidence: evidence, At: now}); err != nil {
			return err
		}
		if open && prev.Reason == string(billing.ParkPriceUnmapped) && (s.Reason != billing.ParkPriceUnmapped || deref(prev.PriceRef) != s.PriceRef) {
			if _, err := SyncPriceFinding(ctx, q, mid.UUID(), s.PSPID, deref(prev.PriceRef), false); err != nil {
				return err
			}
		}
		if s.Reason == billing.ParkPriceUnmapped || res.ImportOwned {
			// A customer cause or a missing plan raised before is settled.
			if err := resolve(ctx, q, mid.UUID(), s.PSPID, FindingUnattributed, s.SubscriptionRef); err != nil {
				return err
			}
		}
		switch {
		case s.Reason == billing.ParkPriceUnmapped:
			// Recounted either way: a park the import now owns no longer counts.
			f, err := SyncPriceFinding(ctx, q, mid.UUID(), s.PSPID, s.PriceRef, false)
			if !res.ImportOwned {
				res.Finding = f
			}
			return err
		case res.ImportOwned:
			return nil
		default:
			res.Finding, err = openUnattributed(ctx, q, mid.UUID(), s, evidence)
		}
		return err
	})
	return res, err
}

// Close ends the subscription's open park, if any, and settles its finding
// (a plan-less schedule's has no park). q must be the transaction converge's
// verdict commits in.
func Close(ctx context.Context, q *gen.Queries, pspID uuid.UUID, subscriptionRef string, by billing.ParkRelease, now time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	row, err := q.CloseOpenPark(ctx, gen.CloseOpenParkParams{MerchantID: mid.UUID(), PspID: pspID, SubscriptionRef: subscriptionRef, ReleasedBy: string(by), At: now})
	if errors.Is(err, pgx.ErrNoRows) {
		return resolve(ctx, q, mid.UUID(), pspID, FindingUnattributed, subscriptionRef)
	}
	if err != nil {
		return err
	}
	if row.Reason == string(billing.ParkPriceUnmapped) {
		_, err = SyncPriceFinding(ctx, q, mid.UUID(), pspID, deref(row.PriceRef), false)
		return err
	}
	return resolve(ctx, q, mid.UUID(), pspID, FindingUnattributed, subscriptionRef)
}

// wakePage bounds one read of the parks a fix wakes.
const wakePage = 500

// Wake queues converge, in d's transaction, for every open park the fixed
// cause names: price_unmapped parks on a price or plan ref, customer_unlinked
// parks on a provider customer. The park stays open until its subscription
// lands. d must be transaction-scoped.
func Wake(ctx context.Context, d *db.DB, pspID uuid.UUID, reason billing.ParkReason, ref string) (int, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	if ref == "" {
		return 0, nil
	}
	if !d.RiverProducerBound() {
		// A host tool without a job queue: the next provider event wakes it.
		log.WithContext(ctx).WithFields(log.Fields{"psp_id": pspID, "reason": reason, "ref": ref}).Warn("park wake skipped: no River producer is bound")
		return 0, nil
	}
	q := d.Gen(ctx)
	if err := q.LockParkCause(ctx, gen.LockParkCauseParams{MerchantID: mid.UUID(), PspID: pspID, Reason: string(reason), Ref: ref}); err != nil {
		return 0, err
	}
	woken, after := 0, ""
	for {
		type park struct{ ref, rail string }
		var page []park
		switch reason {
		case billing.ParkPriceUnmapped:
			rows, err := q.ListOpenParksByPrice(ctx, gen.ListOpenParksByPriceParams{MerchantID: mid.UUID(), PspID: pspID, PriceRef: ref, After: after, RowLimit: wakePage})
			if err != nil {
				return woken, err
			}
			for _, r := range rows {
				page = append(page, park{r.SubscriptionRef, r.Rail})
			}
		case billing.ParkCustomerUnlinked:
			rows, err := q.ListOpenParksByCustomer(ctx, gen.ListOpenParksByCustomerParams{MerchantID: mid.UUID(), PspID: pspID, CustomerRef: ref, After: after, RowLimit: wakePage})
			if err != nil {
				return woken, err
			}
			for _, r := range rows {
				page = append(page, park{r.SubscriptionRef, r.Rail})
			}
		default:
			return 0, fmt.Errorf("wake: %s parks wake by hand", reason)
		}
		for _, p := range page {
			if err := wakejobs.Enqueue(ctx, wakejobs.Tx{DB: d}, wakejobs.SubscriptionConvergeArgs{MerchantID: mid.UUID(), PSPID: pspID, Rail: p.rail, SubscriptionReference: p.ref}, 0); err != nil {
				return woken, fmt.Errorf("wake park %s: %w", p.ref, err)
			}
			woken++
		}
		if len(page) < wakePage {
			return woken, nil
		}
		after = page[len(page)-1].ref
	}
}

// causeFixed re-reads the cause under its lock.
func causeFixed(ctx context.Context, q *gen.Queries, mid uuid.UUID, s Subject) (bool, error) {
	switch s.Reason {
	case billing.ParkPriceUnmapped:
		return q.ProviderRefMapped(ctx, gen.ProviderRefMappedParams{MerchantID: mid, PspID: s.PSPID, Ref: s.PriceRef})
	case billing.ParkCustomerUnlinked:
		if s.CustomerRef == "" {
			return false, nil
		}
		if s.Rail == "stripe" {
			_, err := q.GetPSPCustomerByRef(ctx, gen.GetPSPCustomerByRefParams{MerchantID: mid, PspID: s.PSPID, RemoteCustomerRef: s.CustomerRef})
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return err == nil, err
		}
		owners, err := q.ListVaultOwners(ctx, gen.ListVaultOwnersParams{MerchantID: mid, PspID: s.PSPID, Vault: s.CustomerRef})
		return len(owners) == 1, err
	}
	return false, nil
}

// importOwns applies the backlog rule: the PSP's latest import that has not
// failed is still pulling, or staged the subscription and has not landed it.
// With no such import, nothing owns it.
func importOwns(ctx context.Context, q *gen.Queries, mid uuid.UUID, s Subject) (bool, error) {
	row, err := q.GetParkBacklog(ctx, gen.GetParkBacklogParams{MerchantID: mid, PspID: s.PSPID, SubscriptionRef: s.SubscriptionRef})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.Status == string(billing.PSPImportPulling) {
		return true, nil
	}
	return row.StagedState != "" && row.StagedState != string(billing.PSPImportImported), nil
}

// ImportOwns is the backlog rule read outside a park.
func ImportOwns(ctx context.Context, q *gen.Queries, pspID uuid.UUID, subscriptionRef string) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	return importOwns(ctx, q, mid.UUID(), Subject{PSPID: pspID, SubscriptionRef: subscriptionRef})
}

// SyncPlanless keeps life.subscription.unattributed true to an unheld
// subscription that names no plan. No price can map it and no fix wakes a
// park, so nothing parks: the finding names the subscription once the PSP's
// import no longer owns it (the backlog rule), and resolves while one does.
// It returns the finding it raised.
func SyncPlanless(ctx context.Context, d *db.DB, s Subject) (*gen.BillingReconciliationFinding, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.PSPID == uuid.Nil || s.Rail == "" || s.SubscriptionRef == "" {
		return nil, fmt.Errorf("plan-less finding %q: incomplete subject", s.SubscriptionRef)
	}
	if s.Evidence == nil {
		s.Evidence = map[string]any{}
	}
	s.Evidence["reason"] = "plan_missing"
	evidence, err := json.Marshal(s.Evidence)
	if err != nil {
		return nil, err
	}
	var raised *gen.BillingReconciliationFinding
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		raised = nil
		q := gen.New(tx)
		if owned, err := importOwns(ctx, q, mid.UUID(), s); err != nil || owned {
			if err != nil {
				return err
			}
			return resolve(ctx, q, mid.UUID(), s.PSPID, FindingUnattributed, s.SubscriptionRef)
		}
		action := fmt.Sprintf("Provider subscription %s names no plan, so no price maps it and it cannot land: give it a plan at the provider (its next event lands or parks it), or cancel it there.", s.SubscriptionRef)
		row, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: mid.UUID(), FindingType: FindingUnattributed, SubjectKey: s.SubscriptionRef,
			Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence, PspID: &s.PSPID})
		raised = &row
		return err
	})
	return raised, err
}

// SyncPriceFinding keeps catalog.price.unmapped true to one provider price:
// high while parks the import does not own name it; medium when raise (a
// webhook first observed it active and unmapped) or when it is already open;
// resolved once the price is mapped, or retired with no parks. It takes the
// price's park lock, so q must be a transaction's. It returns the finding it
// wrote.
func SyncPriceFinding(ctx context.Context, q *gen.Queries, mid, pspID uuid.UUID, ref string, raise bool) (*gen.BillingReconciliationFinding, error) {
	if ref == "" {
		return nil, nil
	}
	if err := q.LockParkCause(ctx, gen.LockParkCauseParams{MerchantID: mid, PspID: pspID, Reason: string(billing.ParkPriceUnmapped), Ref: ref}); err != nil {
		return nil, err
	}
	mapped, err := q.ProviderRefMapped(ctx, gen.ProviderRefMappedParams{MerchantID: mid, PspID: pspID, Ref: ref})
	if err != nil {
		return nil, err
	}
	if mapped {
		return nil, resolve(ctx, q, mid, pspID, FindingPriceUnmapped, ref)
	}
	parked, err := q.CountOpenParksByPrice(ctx, gen.CountOpenParksByPriceParams{MerchantID: mid, PspID: pspID, PriceRef: ref})
	if err != nil {
		return nil, err
	}
	retired := false
	if obj, err := q.GetProviderCatalogObject(ctx, gen.GetProviderCatalogObjectParams{MerchantID: mid, PspID: pspID, Kind: "price", Ref: ref}); err == nil {
		retired = obj.Retired
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	open := false
	if f, err := q.GetProviderFinding(ctx, gen.GetProviderFindingParams{MerchantID: mid, FindingType: FindingPriceUnmapped, PspID: pspID, SubjectKey: ref}); err == nil {
		open = f.ResolvedAt == nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	severity, action := "medium", fmt.Sprintf("Provider price %s maps to no price: map it with a provider price, or ignore this finding if it is not ours.", ref)
	switch {
	case parked > 0:
		severity = "high"
		action = fmt.Sprintf("%d live provider subscriptions bill price %s, which maps to no price, and are parked: map it with a provider price and they land, or ignore this finding if it is not ours.", parked, ref)
	case retired:
		return nil, resolve(ctx, q, mid, pspID, FindingPriceUnmapped, ref)
	case !raise && !open:
		return nil, nil
	}
	evidence, err := json.Marshal(map[string]any{"price_ref": ref, "parked": parked, "retired": retired})
	if err != nil {
		return nil, err
	}
	row, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: mid, FindingType: FindingPriceUnmapped, SubjectKey: ref,
		Severity: severity, Status: "requires_review", RecommendedAction: &action, Evidence: evidence, PspID: &pspID})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func openUnattributed(ctx context.Context, q *gen.Queries, mid uuid.UUID, s Subject, evidence []byte) (*gen.BillingReconciliationFinding, error) {
	action := fmt.Sprintf("Provider subscription %s bills customer %s, who is linked to no customer: link it (a PSP import's link, or the customer's checkout) and the subscription lands.", s.SubscriptionRef, s.CustomerRef)
	if s.Reason == billing.ParkProductHeld {
		held := ""
		if s.HeldBy != nil {
			held = " " + s.HeldBy.String()
		}
		action = fmt.Sprintf("Provider subscription %s bills a customer who already holds its product through subscription%s: cancel one of them at the provider; the next event converges it.", s.SubscriptionRef, held)
	}
	row, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: mid, FindingType: FindingUnattributed, SubjectKey: s.SubscriptionRef,
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence, PspID: &s.PSPID})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func resolve(ctx context.Context, q *gen.Queries, mid, pspID uuid.UUID, findingType, subject string) error {
	_, err := q.AutoResolveProviderFinding(ctx, gen.AutoResolveProviderFindingParams{MerchantID: mid, FindingType: findingType, PspID: pspID, SubjectKey: subject})
	return err
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
