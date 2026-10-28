package pspimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/progress"
)

// heldVia is linked_via of a record OpenRails already holds.
const heldVia = "held"

// proposeCustomers is how many customers one reconcile page diffs.
const proposeCustomers = 50

// heldSubscription is a subscription a classify pass found held.
type heldSubscription struct {
	id, customer uuid.UUID
	ref          string
	host         bool
}

// classify marks each staged object OpenRails already holds held, its holder
// as the customer: a subscription by its provider id, a provider customer by
// the cards or customer link naming it, a transaction by its payment or by
// its held subscription. One the rules or a person link to another customer
// is a conflict naming both: a landed subscription never moves. It returns
// the subscriptions it found held that were not held before.
func (im *Importer) classify(ctx context.Context, row gen.BillingPspImport) ([]heldSubscription, error) {
	q := im.DB.Gen(ctx)
	subHolder := map[string]holding{}
	var newly []heldSubscription
	for _, kind := range []billing.PSPImportRecordKind{billing.PSPImportSubscription, billing.PSPImportCustomer, billing.PSPImportTransaction} {
		var page []gen.BillingPspImportRecord
		flush := func() error {
			refs := make([]string, 0, len(page))
			for _, r := range page {
				refs = append(refs, r.Ref)
			}
			holders := map[string][]uuid.UUID{}
			subs := map[string]gen.ListPSPHeldSubscriptionsRow{}
			switch kind {
			case billing.PSPImportSubscription:
				rows, err := q.ListPSPHeldSubscriptions(ctx, gen.ListPSPHeldSubscriptionsParams{MerchantID: row.MerchantID, PspID: row.PspID, Refs: refs})
				if err != nil {
					return err
				}
				for _, s := range rows {
					holders[s.Ref] = appendDistinct(holders[s.Ref], s.CustomerID)
					subs[s.Ref] = s
				}
			case billing.PSPImportCustomer:
				rows, err := q.ListPSPHeldVaults(ctx, gen.ListPSPHeldVaultsParams{MerchantID: row.MerchantID, PspID: row.PspID, Refs: refs})
				if err != nil {
					return err
				}
				for _, v := range rows {
					holders[v.Ref] = appendDistinct(holders[v.Ref], v.CustomerID)
				}
			case billing.PSPImportTransaction:
				ids := make([]string, 0, len(page))
				for _, r := range page {
					var data transactionData
					if err := json.Unmarshal(r.Data, &data); err != nil {
						return err
					}
					ids = append(ids, data.TransactionID)
				}
				rows, err := q.ListPSPHeldTransactions(ctx, gen.ListPSPHeldTransactionsParams{MerchantID: row.MerchantID, PspID: row.PspID, Refs: ids})
				if err != nil {
					return err
				}
				byTxn := map[string][]uuid.UUID{}
				for _, t := range rows {
					byTxn[t.Ref] = appendDistinct(byTxn[t.Ref], t.CustomerID)
				}
				for i, r := range page {
					holders[r.Ref] = byTxn[ids[i]]
				}
			}
			var updates []linkUpdate
			for _, r := range page {
				h := holding{customers: holders[r.Ref]}
				if kind == billing.PSPImportTransaction && len(h.customers) == 0 {
					h = subHolder[deref(r.SubscriptionRef)]
				}
				if len(h.customers) == 0 && h.conflict == "" {
					continue
				}
				u, held := h.decide(r)
				if kind == billing.PSPImportSubscription {
					sh := holding{customers: h.customers}
					if !held {
						sh.conflict = "its subscription: " + u.reason
					}
					subHolder[r.Ref] = sh
					if held && r.State != string(billing.PSPImportHeld) {
						s := subs[r.Ref]
						newly = append(newly, heldSubscription{id: s.ID, customer: s.CustomerID, ref: r.Ref, host: s.FactsAuthority == string(reconcile.HostFacts)})
					}
				}
				if u.state == billing.PSPImportRecordState(r.State) && u.customer == uuidOf(r.CustomerID) && u.reason == deref(r.Reason) {
					continue
				}
				updates = append(updates, u)
			}
			page = page[:0]
			return im.writeLinks(ctx, row, updates)
		}
		if err := im.eachRecord(ctx, row, kind, func(r gen.BillingPspImportRecord) error {
			if r.State == string(billing.PSPImportImported) {
				return nil
			}
			page = append(page, r)
			if len(page) >= stageBatch {
				return flush()
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if err := flush(); err != nil {
			return nil, err
		}
	}
	return newly, nil
}

// holding is who holds a staged object, or why its hold conflicts.
type holding struct {
	customers []uuid.UUID
	conflict  string
}

// decide is a held record's state: held by its one holder, or a conflict
// when several hold it or a rule or link names another customer.
func (h holding) decide(r gen.BillingPspImportRecord) (linkUpdate, bool) {
	u := linkUpdate{id: r.ID, state: billing.PSPImportConflict}
	switch {
	case h.conflict != "":
		u.reason = h.conflict
	case len(h.customers) > 1:
		u.reason = "OpenRails holds it for several customers: " + joinIDs(h.customers)
	case r.State == string(billing.PSPImportConflict):
		u.reason = deref(r.Reason)
		if !strings.HasPrefix(u.reason, "OpenRails holds it") {
			u.reason = fmt.Sprintf("OpenRails holds it for %s; %s", h.customers[0], u.reason)
		}
	case r.CustomerID != nil && *r.CustomerID != h.customers[0] && r.State != string(billing.PSPImportHeld):
		u.reason = fmt.Sprintf("OpenRails holds it for %s; %s links it to %s", h.customers[0], deref(r.LinkedVia), *r.CustomerID)
	default:
		u.state, u.customer, u.via = billing.PSPImportHeld, h.customers[0], heldVia
		if r.State == string(billing.PSPImportHeld) {
			u.reason = deref(r.Reason)
		}
		return u, true
	}
	return u, false
}

func appendDistinct(ids []uuid.UUID, id uuid.UUID) []uuid.UUID {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

func joinIDs(ids []uuid.UUID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func uuidOf(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// propose raises the differences between what the pull read of the objects
// OpenRails holds and what OpenRails records as the import's proposals, one
// page of customers at a time. customers bounds it to their book; nil runs
// over the PSP's whole book and its held records, and resolves the earlier
// proposals no read raises any more. An NMI roster implausibly small against
// the local live book fails the import and raises nothing.
func (im *Importer) propose(ctx context.Context, row gen.BillingPspImport, scope merchants.PSPScope, customers []uuid.UUID) error {
	q := im.DB.Gen(ctx)
	provider := reconcile.Provider(scope.Rail)
	counts, err := q.CountPSPImportRecords(ctx, gen.CountPSPImportRecordsParams{MerchantID: row.MerchantID, ImportID: row.ID})
	if err != nil {
		return err
	}
	roster := 0
	for _, c := range counts {
		if c.Kind == string(billing.PSPImportSubscription) {
			roster += int(c.Records)
		}
	}
	// An empty roster is indistinguishable from the wrong account: it proves
	// no absence. CCBill's roster lists active members only.
	exhaustive := roster > 0 && provider != reconcile.ProviderCCBill
	if provider == reconcile.ProviderNMI && exhaustive && customers == nil {
		live, err := q.CountPSPLiveSubscriptions(ctx, gen.CountPSPLiveSubscriptionsParams{MerchantID: row.MerchantID, PspID: row.PspID})
		if err != nil {
			return err
		}
		if tripped, reason := (reconcile.RosterBreaker{}).Implausible(provider, roster, int(live)); tripped {
			return PermanentError{Err: errors.New(reason), Code: billing.PSPImportRosterImplausible}
		}
	}
	basisRows, err := q.ListPSPImportBasis(ctx, gen.ListPSPImportBasisParams{MerchantID: row.MerchantID, ImportID: row.ID})
	if err != nil {
		return err
	}
	basis := make(map[uuid.UUID]int64, len(basisRows))
	for _, b := range basisRows {
		basis[b.SubscriptionID] = b.RowVersion
	}
	store := &reconcile.PGStore{DB: im.DB, Now: im.now}
	run, err := store.CreateRun(ctx, reconcile.ModeAdvisory, []reconcile.Provider{provider}, nil, nil)
	if err != nil {
		return err
	}
	finish := func(err error) error {
		status, msg := "completed", ""
		if err != nil {
			status, msg = "failed", err.Error()
		}
		if ferr := store.FinishRun(ctx, run, status, nil, msg); err == nil {
			err = ferr
		}
		return err
	}
	binding := reconcile.PSPBinding{ID: scope.ID, Rail: scope.Rail, AccountID: scope.AccountID}
	page := func(ids []uuid.UUID) error {
		recs, err := q.ListPSPImportHeldRecords(ctx, gen.ListPSPImportHeldRecordsParams{MerchantID: row.MerchantID, ImportID: row.ID, CustomerIds: ids})
		if err != nil {
			return err
		}
		snap, excluded, err := im.snapshotOf(ctx, row, provider, recs, exhaustive)
		if err != nil {
			return err
		}
		res, err := reconcile.Propose(ctx, im.DB, reconcile.ProposeParams{Binding: binding, Snapshot: snap, Coverage: reconcile.PSPCoverage{Declared: 1, Pulled: 1, Binding: binding},
			Basis: basis, Customers: ids, Excluded: excluded, PullID: row.ID, RunID: run, Contacts: im.Directory, Now: im.now()})
		if err != nil {
			return err
		}
		return im.noteMoved(ctx, row, res.Moved)
	}
	if customers != nil {
		for start := 0; start < len(customers); start += proposeCustomers {
			if err := page(customers[start:min(start+proposeCustomers, len(customers))]); err != nil {
				return finish(err)
			}
		}
		return finish(nil)
	}
	for after := uuid.Nil; ; {
		ids, err := q.ListPSPImportProposeCustomers(ctx, gen.ListPSPImportProposeCustomersParams{MerchantID: row.MerchantID, PspID: row.PspID, ImportID: row.ID,
			AfterCustomerID: after, RowLimit: proposeCustomers})
		if err != nil {
			return finish(err)
		}
		if len(ids) == 0 {
			break
		}
		progress.Mark(ctx, fmt.Sprintf("psp import %s reconcile after %s", billing.PSPImportID(row.ID), after))
		if err := page(ids); err != nil {
			return finish(err)
		}
		after = ids[len(ids)-1]
	}
	if exhaustive {
		if _, err := store.AutoResolveVanished(ctx, scope.ID, run, reconcile.StateRosterFindingTypes()); err != nil {
			return finish(err)
		}
	}
	return finish(nil)
}

// snapshotOf is what the pull read of held records, as the reconcile diff
// reads a provider: their remote values at the roster read. Subscriptions
// OpenRails holds whose record conflicts are excluded.
func (im *Importer) snapshotOf(ctx context.Context, row gen.BillingPspImport, provider reconcile.Provider, recs []gen.BillingPspImportRecord, exhaustive bool) (*reconcile.RemoteSnapshot, map[string]bool, error) {
	snap := &reconcile.RemoteSnapshot{Provider: provider, Capabilities: reconcile.Capabilities{Subscriptions: true, Transactions: true, Refunds: true,
		Vault: provider == reconcile.ProviderNMI}}
	if row.PulledAt != nil {
		snap.FetchedAt = *row.PulledAt
	}
	snap.Coverage.SubscriptionsExhaustive = exhaustive
	var conflicts []string
	for _, r := range recs {
		if r.State == string(billing.PSPImportConflict) {
			conflicts = append(conflicts, r.Ref)
			continue
		}
		switch billing.PSPImportRecordKind(r.Kind) {
		case billing.PSPImportCustomer:
			var data customerData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				return nil, nil, err
			}
			snap.PaymentMethods = append(snap.PaymentMethods, data.Remote...)
		case billing.PSPImportSubscription:
			var data subscriptionData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				return nil, nil, err
			}
			if data.Remote != nil {
				snap.Subscriptions = append(snap.Subscriptions, *data.Remote)
			}
		case billing.PSPImportTransaction:
			var data transactionData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				return nil, nil, err
			}
			if data.Remote != nil {
				t := *data.Remote
				if t.SubscriptionID == "" {
					t.SubscriptionID = deref(r.SubscriptionRef)
				}
				snap.Transactions = append(snap.Transactions, t)
			}
		}
	}
	excluded := map[string]bool{}
	if len(conflicts) > 0 {
		held, err := im.DB.Gen(ctx).ListPSPHeldSubscriptions(ctx, gen.ListPSPHeldSubscriptionsParams{MerchantID: row.MerchantID, PspID: row.PspID, Refs: conflicts})
		if err != nil {
			return nil, nil, err
		}
		for _, s := range held {
			excluded[s.Ref] = true
		}
	}
	return snap, excluded, nil
}

// movedReason is a held subscription's reason when its row changed after the
// pull read it.
const movedReason = "the subscription changed after the pull read it: its state is not proposed; reconcile again"

// noteMoved says so on the held records of subscriptions that moved.
func (im *Importer) noteMoved(ctx context.Context, row gen.BillingPspImport, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	recs, err := im.DB.Gen(ctx).ListPSPImportRecordsByRef(ctx, gen.ListPSPImportRecordsByRefParams{MerchantID: row.MerchantID, ImportID: row.ID,
		Kind: string(billing.PSPImportSubscription), Refs: refs})
	if err != nil {
		return err
	}
	var updates []linkUpdate
	for _, r := range recs {
		if r.State == string(billing.PSPImportHeld) && r.CustomerID != nil {
			updates = append(updates, linkUpdate{id: r.ID, state: billing.PSPImportHeld, customer: *r.CustomerID, via: heldVia, reason: movedReason})
		}
	}
	return im.writeLinks(ctx, row, updates)
}

// approveCorrections approves the import's corrections under one approval
// run in the name of the person who asked to apply it, then hands the held
// rows a host book landed, unchanged since the pull read them, to the
// provider's evidence.
func (im *Importer) approveCorrections(ctx context.Context, row gen.BillingPspImport) error {
	q := im.DB.Gen(ctx)
	flips, err := q.ListPSPImportFlips(ctx, gen.ListPSPImportFlipsParams{MerchantID: row.MerchantID, ImportID: row.ID})
	if err != nil {
		return err
	}
	if im.Proposals != nil && row.ApplyRequestedBy != nil {
		ids, err := q.ListPullCorrections(ctx, gen.ListPullCorrectionsParams{MerchantID: row.MerchantID, PullID: row.ID})
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			progress.Mark(ctx, fmt.Sprintf("psp import %s approve %d corrections", billing.PSPImportID(row.ID), len(ids)))
			if _, err := im.Proposals.ApproveSet(ctx, ids, *row.ApplyRequestedBy, "applied with "+billing.PSPImportID(row.ID).String()); err != nil {
				return err
			}
		}
	}
	if len(flips) == 0 {
		return nil
	}
	_, err = q.TakeProviderFacts(ctx, gen.TakeProviderFactsParams{MerchantID: row.MerchantID, Ids: flips})
	return err
}

// basisNow records held subscriptions at their current row version: rows an
// import first finds held at its Apply.
func (im *Importer) basisNow(ctx context.Context, row gen.BillingPspImport, subs []heldSubscription) ([]uuid.UUID, error) {
	var ids, customers []uuid.UUID
	for _, s := range subs {
		if s.host {
			ids = append(ids, s.id)
			customers = appendDistinct(customers, s.customer)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	sort.Slice(customers, func(i, j int) bool { return customers[i].String() < customers[j].String() })
	return customers, im.DB.Gen(ctx).SetPSPImportBasis(ctx, gen.SetPSPImportBasisParams{MerchantID: row.MerchantID, ImportID: row.ID, SubscriptionIds: ids})
}
