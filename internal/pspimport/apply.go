package pspimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/progress"
)

// applyCustomers is how many customers one billing import book carries.
const applyCustomers = 50

// Apply lands the import's linked records through billingimport, a book of
// whole customers at a time at the pull's horizon. A book the import refuses
// is retried one customer at a time, so one customer's refusal blocks only
// its own records.
func (im *Importer) Apply(ctx context.Context, id billing.PSPImportID) error {
	row, err := im.row(ctx, id)
	if err != nil || row.Status != string(billing.PSPImportApplying) {
		return err
	}
	scope, found, err := im.Merchants.PSPScopeByID(ctx, billing.MerchantID(row.MerchantID), row.PspID)
	if err != nil {
		return err
	}
	if !found {
		return Permanent(errors.New("the import's PSP is no longer declared"))
	}
	prices, err := im.providerPrices(ctx, row)
	if err != nil {
		return err
	}
	l := lander{im: im, row: row, scope: scope, prices: prices, asOf: *row.PulledAt}
	after := uuid.Nil
	for {
		customers, err := im.DB.Gen(ctx).ListPSPImportLinkedCustomers(ctx, gen.ListPSPImportLinkedCustomersParams{MerchantID: row.MerchantID, ImportID: row.ID, AfterCustomerID: after, RowLimit: applyCustomers})
		if err != nil {
			return err
		}
		if len(customers) == 0 {
			break
		}
		progress.Mark(ctx, fmt.Sprintf("psp import %s apply after %s", id, after))
		records, err := im.DB.Gen(ctx).ListPSPImportCustomerRecords(ctx, gen.ListPSPImportCustomerRecordsParams{MerchantID: row.MerchantID, ImportID: row.ID, CustomerIds: customers})
		if err != nil {
			return err
		}
		if err := l.land(ctx, group(records)); err != nil {
			return err
		}
		after = customers[len(customers)-1]
	}
	return im.DB.Gen(ctx).SetPSPImportApplied(ctx, gen.SetPSPImportAppliedParams{MerchantID: row.MerchantID, ID: row.ID, AppliedAt: im.now()})
}

// providerPrices are the provider prices of the plans the linked
// subscriptions name, by plan.
func (im *Importer) providerPrices(ctx context.Context, row gen.BillingPspImport) (map[string]billing.PriceID, error) {
	plans, err := im.DB.Gen(ctx).ListPSPImportPlans(ctx, gen.ListPSPImportPlansParams{MerchantID: row.MerchantID, ImportID: row.ID})
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(plans))
	for _, p := range plans {
		if p != nil {
			refs = append(refs, *p)
		}
	}
	out := map[string]billing.PriceID{}
	if len(refs) == 0 {
		return out, nil
	}
	rows, err := im.DB.Gen(ctx).ListPSPProviderPrices(ctx, gen.ListPSPProviderPricesParams{MerchantID: row.MerchantID, PspID: row.PspID, Refs: refs})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.Ref] = billing.PriceID(r.PublicID)
	}
	return out, nil
}

// customerGroup is one customer's linked and landed records.
type customerGroup struct {
	customer uuid.UUID
	records  []gen.BillingPspImportRecord
}

func group(records []gen.BillingPspImportRecord) []customerGroup {
	var out []customerGroup
	for _, r := range records {
		if r.CustomerID == nil {
			continue
		}
		if len(out) == 0 || out[len(out)-1].customer != *r.CustomerID {
			out = append(out, customerGroup{customer: *r.CustomerID})
		}
		out[len(out)-1].records = append(out[len(out)-1].records, r)
	}
	return out
}

type lander struct {
	im     *Importer
	row    gen.BillingPspImport
	scope  merchants.PSPScope
	prices map[string]billing.PriceID
	asOf   time.Time
}

// outcome is what landing did to one record.
type outcome struct {
	state  billing.PSPImportRecordState
	reason string
}

// declaration is a book and how its results map back to records.
type declaration struct {
	book billing.DeclaredBilling
	// customers are the vault records whose cards the book declares.
	customers []uuid.UUID
	// subscriptions maps a source id to its record, txns to its transactions.
	subscriptions map[string]uuid.UUID
	txns          map[string][]uuid.UUID
	outcomes      map[uuid.UUID]outcome
}

func (l *lander) land(ctx context.Context, groups []customerGroup) error {
	d, err := l.declare(groups)
	if err != nil {
		return err
	}
	res, err := l.im.importBook(ctx, l.row, d.book)
	var refusal *apperr.Error
	if errors.As(err, &refusal) && refusal.Status < 500 && len(groups) > 1 {
		for _, g := range groups {
			if err := l.land(ctx, []customerGroup{g}); err != nil {
				return err
			}
		}
		return nil
	}
	switch {
	case errors.As(err, &refusal) && refusal.Status < 500:
		for _, g := range groups {
			for _, r := range g.records {
				if r.State == string(billing.PSPImportLinked) {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "the billing import refused this customer: " + refusal.Error()}
				}
			}
		}
	case err != nil:
		return err
	default:
		d.record(res)
	}
	return l.im.writeOutcomes(ctx, l.row, d.outcomes)
}

func (im *Importer) importBook(ctx context.Context, row gen.BillingPspImport, book billing.DeclaredBilling) (billing.BillingImportResult, error) {
	if len(book.Customers) == 0 {
		return billing.BillingImportResult{}, nil
	}
	return billingimport.Import(ctx, billingimport.Options{DB: im.DB, MerchantID: billing.MerchantID(row.MerchantID), Book: book, Clock: im.Clock})
}

// record maps the import's per-subscription results onto the records.
func (d *declaration) record(res billing.BillingImportResult) {
	for _, id := range d.customers {
		d.outcomes[id] = outcome{state: billing.PSPImportImported}
	}
	landed := map[string]bool{}
	for _, src := range append(append([]string{}, res.Imported...), res.Skipped...) {
		landed[src] = true
	}
	for src, id := range d.subscriptions {
		if landed[src] {
			if _, decided := d.outcomes[id]; !decided {
				d.outcomes[id] = outcome{state: billing.PSPImportImported}
			}
			for _, t := range d.txns[src] {
				d.outcomes[t] = outcome{state: billing.PSPImportImported}
			}
			continue
		}
		reason := res.Reasons[src]
		if reason == "" {
			reason = "the billing import did not land it"
		}
		if _, decided := d.outcomes[id]; !decided {
			d.outcomes[id] = outcome{billing.PSPImportBlocked, reason}
		}
		for _, t := range d.txns[src] {
			d.outcomes[t] = outcome{billing.PSPImportBlocked, "its subscription did not land: " + reason}
		}
	}
}

func (l *lander) declare(groups []customerGroup) (declaration, error) {
	pspID := billing.PSPID(l.scope.ID)
	d := declaration{subscriptions: map[string]uuid.UUID{}, txns: map[string][]uuid.UUID{}, outcomes: map[uuid.UUID]outcome{},
		book: billing.DeclaredBilling{AsOf: l.asOf, DefaultPSP: billing.PSPRef{ID: &pspID}}}
	for _, g := range groups {
		customer := billing.CustomerID(g.customer)
		d.book.Customers = append(d.book.Customers, billing.DeclaredCustomer{Customer: customer})
		vaults := map[string]customerData{}
		subs := map[string]gen.BillingPspImportRecord{}
		for _, r := range g.records {
			switch billing.PSPImportRecordKind(r.Kind) {
			case billing.PSPImportCustomer:
				var data customerData
				if err := json.Unmarshal(r.Data, &data); err != nil {
					return d, err
				}
				vaults[r.Ref] = data
				if r.State != string(billing.PSPImportLinked) {
					continue
				}
				for _, c := range data.Cards {
					pm := billing.DeclaredPaymentMethod{Customer: customer, Rail: l.scope.Rail, PSP: billing.PSPRef{ID: &pspID}, RailCustomerRef: r.Ref, RailMethodRef: c.MethodRef, Card: c.details()}
					if r.OccurredAt != nil {
						pm.CreatedAt = *r.OccurredAt
					}
					d.book.PaymentMethods = append(d.book.PaymentMethods, pm)
				}
				d.customers = append(d.customers, r.ID)
			case billing.PSPImportSubscription:
				subs[r.Ref] = r
			}
		}
		declared := map[string]bool{}
		declareSub := func(r gen.BillingPspImportRecord) (string, string) {
			src := "pspimport:" + l.scope.Rail + ":" + r.Ref
			if declared[r.Ref] {
				return src, ""
			}
			sub, reason, err := l.subscription(customer, r, vaults)
			if err != nil {
				return src, err.Error()
			}
			if reason != "" {
				return src, reason
			}
			sub.SourceID = src
			d.book.Subscriptions = append(d.book.Subscriptions, sub)
			d.subscriptions[src] = r.ID
			declared[r.Ref] = true
			return src, ""
		}
		for _, r := range g.records {
			if r.Kind != string(billing.PSPImportSubscription) || r.State != string(billing.PSPImportLinked) {
				continue
			}
			if _, reason := declareSub(r); reason != "" {
				d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, reason}
			}
		}
		for _, r := range g.records {
			if r.Kind != string(billing.PSPImportTransaction) || r.State != string(billing.PSPImportLinked) {
				continue
			}
			subRef := deref(r.SubscriptionRef)
			sub, ok := subs[subRef]
			if subRef == "" || !ok {
				d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "no subscription of this customer names it: history without a subscription does not land yet"}
				continue
			}
			src, reason := declareSub(sub)
			if reason != "" {
				d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "its subscription does not land: " + reason}
				continue
			}
			var data transactionData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				return d, err
			}
			typ := data.Type
			if typ == "sale" {
				typ = ""
			}
			d.book.Transactions = append(d.book.Transactions, billing.DeclaredTransaction{RailSubscriptionID: subRef, TransactionID: data.TransactionID, Type: typ,
				Success: data.Success, Amount: data.Amount, Currency: data.Currency, OccurredAt: data.OccurredAt})
			d.txns[src] = append(d.txns[src], r.ID)
		}
	}
	return d, nil
}

// subscription declares one staged subscription; reason says why it cannot.
func (l *lander) subscription(customer billing.CustomerID, r gen.BillingPspImportRecord, vaults map[string]customerData) (billing.DeclaredSubscription, string, error) {
	var data subscriptionData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return billing.DeclaredSubscription{}, "", err
	}
	price, ok := l.prices[data.Plan]
	switch {
	case data.Plan == "":
		return billing.DeclaredSubscription{}, "the provider names no plan for it", nil
	case !ok:
		return billing.DeclaredSubscription{}, fmt.Sprintf("plan %q has no provider price: name the product it grants in the import's products", data.Plan), nil
	case data.StartedAt.IsZero():
		return billing.DeclaredSubscription{}, "the provider states no start date", nil
	}
	sub := billing.DeclaredSubscription{Customer: customer, Price: price, Rail: l.scope.Rail, RailSubscriptionID: r.Ref, StartedAt: data.StartedAt,
		PaidThrough: data.NextBillingAt, Evidence: data.Evidence}
	if vault, ok := vaults[deref(r.CustomerRef)]; ok {
		for _, c := range vault.Cards {
			if c.Primary {
				sub.PaymentMethod = &billing.PaymentMethodRef{Rail: l.scope.Rail, RailCustomerRef: deref(r.CustomerRef), RailMethodRef: c.MethodRef}
			}
		}
	}
	switch {
	case data.Paused:
		// A paused schedule bills nothing: access runs to its paid date.
		sub.Cancel = billing.CancelEvidence{Kind: "user_canceled", At: l.asOf}
	case data.NextBillingAt != nil && (data.IntervalHours > 0) != (data.IntervalMonths > 0):
		sub.AcceptedSchedule = &billing.DeclaredProviderSchedule{SourceReference: l.scope.Rail + "_subscription:" + r.Ref, Amount: data.Amount, Currency: data.Currency,
			IntervalHours: data.IntervalHours, IntervalMonths: data.IntervalMonths, FirstDueAt: *data.NextBillingAt}
	}
	return sub, "", nil
}

func (c cardData) details() *billing.CardDetails {
	out := &billing.CardDetails{}
	if c.Brand != "" {
		out.Brand = new(c.Brand)
	}
	if c.Last4 != "" {
		out.Last4 = new(c.Last4)
	}
	if c.ExpMonth > 0 {
		out.ExpMonth, out.ExpYear = new(c.ExpMonth), new(c.ExpYear)
	}
	return out
}

func (im *Importer) writeOutcomes(ctx context.Context, row gen.BillingPspImport, outcomes map[uuid.UUID]outcome) error {
	if len(outcomes) == 0 {
		return nil
	}
	p := gen.SetPSPImportRecordOutcomesParams{MerchantID: row.MerchantID, ImportID: row.ID}
	for id, o := range outcomes {
		reason := o.reason
		if len(reason) > 2000 {
			reason = reason[:2000]
		}
		p.Ids, p.States, p.Reasons = append(p.Ids, id), append(p.States, string(o.state)), append(p.Reasons, strings.TrimSpace(reason))
	}
	return im.DB.Gen(ctx).SetPSPImportRecordOutcomes(ctx, p)
}

// MissingProviderPrices are the plans of an import no provider price of its
// PSP mirrors yet.
func (im *Importer) MissingProviderPrices(ctx context.Context, id billing.PSPImportID, plans []string) ([]string, error) {
	row, err := im.row(ctx, id)
	if err != nil || len(plans) == 0 {
		return nil, err
	}
	rows, err := im.DB.Gen(ctx).ListPSPProviderPrices(ctx, gen.ListPSPProviderPricesParams{MerchantID: row.MerchantID, PspID: row.PspID, Refs: plans})
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Ref] = true
	}
	var out []string
	for _, p := range plans {
		if !have[p] {
			out = append(out, p)
		}
	}
	return out, nil
}
