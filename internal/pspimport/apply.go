package pspimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
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
	prices, unpriced, err := im.providerPrices(ctx, row)
	if err != nil {
		return err
	}
	l := lander{im: im, row: row, scope: scope, prices: prices, unpriced: unpriced, asOf: *row.PulledAt}
	if scope.Rail == "nmi" {
		// Refunds can be unmatched or linked to another customer. Inspect the
		// whole staged import before landing any standalone charge.
		l.refunded = map[string]bool{}
		if err := im.eachRecord(ctx, row, billing.PSPImportTransaction, func(r gen.BillingPspImportRecord) error {
			var data transactionData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				return err
			}
			if data.Success && data.Type == "refund" {
				l.refunded[data.TransactionID] = true
			}
			return nil
		}); err != nil {
			return err
		}
	}
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

// priceTerms are a provider price and the terms it caches.
type priceTerms struct {
	id       billing.PriceID
	amount   int64
	currency string
	hours    int
	months   int
}

func termsOf(id string, amount int64, currency string, hours, months *int32) priceTerms {
	t := priceTerms{id: billing.PriceID(id), amount: amount, currency: currency}
	if hours != nil {
		t.hours = int(*hours)
	}
	if months != nil {
		t.months = int(*months)
	}
	return t
}

// providerPrices are the provider prices of the plans the linked
// subscriptions name, by plan. A rail whose subscriptions name no plan
// (CCBill) takes the one provider price the import's product holds on the
// PSP, keyed "".
func (im *Importer) providerPrices(ctx context.Context, row gen.BillingPspImport) (map[string]priceTerms, string, error) {
	out := map[string]priceTerms{}
	if !planned[row.Rail] {
		var products []billing.PSPImportProduct
		if err := json.Unmarshal(row.Products, &products); err != nil {
			return nil, "", err
		}
		entry := productEntry(products, "")
		if entry.Product == "" {
			return out, "the import names no product for its subscriptions", nil
		}
		rows, err := im.DB.Gen(ctx).ListProductProviderPrices(ctx, gen.ListProductProviderPricesParams{MerchantID: row.MerchantID, PspID: row.PspID, ProductKey: entry.Product})
		if err != nil {
			return nil, "", err
		}
		for _, r := range rows {
			out["price:"+r.Key] = termsOf(r.PublicID, r.Amount, r.Currency, r.BillingIntervalHours, r.BillingIntervalMonths)
		}
		if entry.Price != "" {
			rows = slices.DeleteFunc(rows, func(r gen.ListProductProviderPricesRow) bool { return r.Key != entry.Price })
		}
		if len(rows) != 1 {
			return out, fmt.Sprintf("its rebills match none of product %q's provider prices on this PSP, and %d match %q: name one by its key (%s has no catalog to read)", entry.Product, len(rows), entry.Price, row.Rail), nil
		}
		r := rows[0]
		out[""] = termsOf(r.PublicID, r.Amount, r.Currency, r.BillingIntervalHours, r.BillingIntervalMonths)
		return out, "", nil
	}
	plans, err := im.DB.Gen(ctx).ListPSPImportPlans(ctx, gen.ListPSPImportPlansParams{MerchantID: row.MerchantID, ImportID: row.ID})
	if err != nil {
		return nil, "", err
	}
	refs := make([]string, 0, len(plans))
	for _, p := range plans {
		if p != nil {
			refs = append(refs, *p)
		}
	}
	if len(refs) == 0 {
		return out, "", nil
	}
	rows, err := im.DB.Gen(ctx).ListPSPProviderPrices(ctx, gen.ListPSPProviderPricesParams{MerchantID: row.MerchantID, PspID: row.PspID, Refs: refs})
	if err != nil {
		return nil, "", err
	}
	for _, r := range rows {
		out[r.Ref] = termsOf(r.PublicID, r.Amount, r.Currency, r.BillingIntervalHours, r.BillingIntervalMonths)
	}
	return out, "", nil
}

// planned are the rails whose subscriptions name a provider plan or price.
var planned = map[string]bool{"nmi": true, "stripe": true}

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
	prices map[string]priceTerms
	// unpriced says why no subscription of a plan-less rail has a price.
	unpriced string
	asOf     time.Time
	refunded map[string]bool
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
	historical    map[string]uuid.UUID
	outcomes      map[uuid.UUID]outcome
}

func (l *lander) land(ctx context.Context, groups []customerGroup) error {
	// An existing subscription that is unmatched, blocked or linked to
	// someone else is not missing history. Do not bypass that decision.
	var refs []string
	for _, g := range groups {
		for _, r := range g.records {
			if r.Kind == string(billing.PSPImportTransaction) && r.SubscriptionRef != nil {
				refs = append(refs, *r.SubscriptionRef)
			}
		}
	}
	known := map[string]bool{}
	if len(refs) > 0 {
		rows, err := l.im.DB.Gen(ctx).ListPSPImportRecordsByRef(ctx, gen.ListPSPImportRecordsByRefParams{
			MerchantID: l.row.MerchantID, ImportID: l.row.ID, Kind: string(billing.PSPImportSubscription), Refs: refs,
		})
		if err != nil {
			return err
		}
		for _, r := range rows {
			known[r.Ref] = true
		}
	}
	d, err := l.declare(groups, known)
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
	for src, id := range d.historical {
		if landed[src] {
			d.outcomes[id] = outcome{state: billing.PSPImportImported}
		} else {
			d.outcomes[id] = outcome{billing.PSPImportBlocked, res.Reasons[src]}
		}
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

func (l *lander) declare(groups []customerGroup, known map[string]bool) (declaration, error) {
	pspID := billing.PSPID(l.scope.ID)
	d := declaration{historical: map[string]uuid.UUID{}, subscriptions: map[string]uuid.UUID{}, txns: map[string][]uuid.UUID{}, outcomes: map[uuid.UUID]outcome{},
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
		rebills := latestRebills(g.records)
		declared := map[string]*billing.PaymentMethodRef{}
		priced := map[string]priceTerms{}
		declareSub := func(r gen.BillingPspImportRecord) (string, string) {
			src := "pspimport:" + l.scope.Rail + ":" + r.Ref
			if _, done := declared[r.Ref]; done {
				return src, ""
			}
			sub, terms, reason, err := l.subscription(customer, r, vaults, rebills[r.Ref])
			if err != nil {
				return src, err.Error()
			}
			if reason != "" {
				return src, reason
			}
			sub.SourceID = src
			d.book.Subscriptions = append(d.book.Subscriptions, sub)
			d.subscriptions[src] = r.ID
			declared[r.Ref], priced[r.Ref] = sub.PaymentMethod, terms
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
				if l.scope.Rail != "nmi" {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "history without a subscription is currently supported only for NMI"}
					continue
				}
				if known[subRef] {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "its subscription exists but is not linked to this customer"}
					continue
				}
				var data transactionData
				if err := json.Unmarshal(r.Data, &data); err != nil {
					return d, err
				}
				if !data.Success || data.Type != "" && data.Type != "sale" {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "history without a subscription requires a confirmed successful sale"}
					continue
				}
				if l.refunded[data.TransactionID] {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "this payment has a recorded refund; importing payment and refund history together is not yet supported"}
					continue
				}
				if data.StripeInvoice != nil {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "invoice history requires its subscription; it cannot be replaced by a standalone payment"}
					continue
				}
				src := "history:" + r.ID.String()
				d.historical[src] = r.ID
				d.book.HistoricalPayments = append(d.book.HistoricalPayments, billing.DeclaredHistoricalPayment{
					SourceID: src, Customer: customer, Rail: l.scope.Rail, TransactionID: data.TransactionID,
					Amount: data.Amount, Currency: data.Currency, PaidAt: data.OccurredAt,
				})
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
			if data.Currency == "" {
				price := priced[subRef]
				amount, reason := denominate(price, data.Decimal)
				if reason != "" {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, reason}
					continue
				}
				data.Amount, data.Currency = amount, price.currency
			}
			if inv := data.StripeInvoice; inv != nil {
				if pm := declared[subRef]; pm == nil || pm.RailCustomerRef != inv.CustomerReference {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "its subscription bills no saved card of the invoice's customer, so the invoice cannot bind"}
					continue
				}
			}
			typ := data.Type
			if typ == "sale" {
				typ = ""
			}
			d.book.Transactions = append(d.book.Transactions, billing.DeclaredTransaction{RailSubscriptionID: subRef, TransactionID: data.TransactionID, Type: typ,
				Success: data.Success, Amount: data.Amount, Currency: data.Currency, OccurredAt: data.OccurredAt, StripeInvoice: data.StripeInvoice})
			d.txns[src] = append(d.txns[src], r.ID)
		}
	}
	return d, nil
}

// subscription declares one staged subscription; reason says why it cannot.
func (l *lander) subscription(customer billing.CustomerID, r gen.BillingPspImportRecord, vaults map[string]customerData, rebill string) (billing.DeclaredSubscription, priceTerms, string, error) {
	var data subscriptionData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return billing.DeclaredSubscription{}, priceTerms{}, "", err
	}
	price, ok := l.prices[data.Plan]
	if matched, found := l.priceOfRebill(data.Plan, rebill); found {
		price, ok = matched, true
	}
	switch {
	case !ok && l.unpriced != "":
		return billing.DeclaredSubscription{}, price, l.unpriced, nil
	case !ok && data.Plan == "":
		return billing.DeclaredSubscription{}, price, "the provider names no plan for it", nil
	case !ok:
		return billing.DeclaredSubscription{}, price, fmt.Sprintf("plan %q has no provider price: name the product it grants in the import's products", data.Plan), nil
	case data.StartedAt.IsZero():
		return billing.DeclaredSubscription{}, price, "the provider states no start date", nil
	}
	if data.Amount == 0 {
		// The provider's record states no terms: the declared price does.
		data.Amount, data.Currency, data.IntervalHours, data.IntervalMonths = price.amount, price.currency, price.hours, price.months
	}
	sub := billing.DeclaredSubscription{Customer: customer, Price: price.id, Rail: l.scope.Rail, RailSubscriptionID: r.Ref, StartedAt: data.StartedAt,
		PaidThrough: data.NextBillingAt, Evidence: data.Evidence}
	if vault, ok := vaults[deref(r.CustomerRef)]; ok {
		method := ""
		for _, c := range vault.Cards {
			if c.MethodRef == data.MethodRef || data.MethodRef == "" && c.Primary {
				method = c.MethodRef
			}
		}
		if method != "" {
			sub.PaymentMethod = &billing.PaymentMethodRef{Rail: l.scope.Rail, RailCustomerRef: deref(r.CustomerRef), RailMethodRef: method}
		}
	}
	scheduled := l.scope.Rail == "nmi" || l.scope.Rail == "ccbill"
	switch {
	case data.Ended:
		at := l.asOf
		if data.EndedAt != nil {
			at = *data.EndedAt
		}
		sub.Cancel = billing.CancelEvidence{Kind: "provider_terminated", At: at}
		if data.UserCanceled {
			sub.Cancel.Kind = "user_canceled"
		}
	case data.Paused:
		// A paused schedule bills nothing: access runs to its paid date.
		sub.Cancel = billing.CancelEvidence{Kind: "user_canceled", At: l.asOf}
	case scheduled && data.NextBillingAt != nil && (data.IntervalHours > 0) != (data.IntervalMonths > 0):
		sub.AcceptedSchedule = &billing.DeclaredProviderSchedule{SourceReference: l.scope.Rail + "_subscription:" + r.Ref, Amount: data.Amount, Currency: data.Currency,
			IntervalHours: data.IntervalHours, IntervalMonths: data.IntervalMonths, FirstDueAt: *data.NextBillingAt}
	}
	return sub, price, "", nil
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

// denominate reads a currency-less amount in its subscription's price
// currency.
func denominate(price priceTerms, decimal string) (int64, string) {
	if price.id == "" {
		return 0, "its subscription has no price to denominate it"
	}
	minor, err := moneyutil.DecimalToRailMinor(price.currency, decimal)
	if err != nil {
		return 0, fmt.Sprintf("its amount %q is not exact in %s", decimal, price.currency)
	}
	native, err := moneyutil.RailMinorToNative(price.currency, minor)
	if err != nil {
		return 0, err.Error()
	}
	return native, ""
}

// latestRebills is each subscription's latest currency-less charge amount.
func latestRebills(records []gen.BillingPspImportRecord) map[string]string {
	out, at := map[string]string{}, map[string]time.Time{}
	for _, r := range records {
		if r.Kind != string(billing.PSPImportTransaction) || r.SubscriptionRef == nil {
			continue
		}
		var data transactionData
		if json.Unmarshal(r.Data, &data) != nil || data.Decimal == "" || data.Type != "sale" || !data.Success {
			continue
		}
		if ref := *r.SubscriptionRef; data.OccurredAt.After(at[ref]) {
			out[ref], at[ref] = data.Decimal, data.OccurredAt
		}
	}
	return out
}

// priceOfRebill is the one provider price a plan-less subscription's rebill
// amount equals, read in each price's own currency.
func (l *lander) priceOfRebill(plan, rebill string) (priceTerms, bool) {
	if plan != "" || rebill == "" {
		return priceTerms{}, false
	}
	var match []priceTerms
	for key, t := range l.prices {
		if !strings.HasPrefix(key, "price:") {
			continue
		}
		minor, err := moneyutil.DecimalToRailMinor(t.currency, rebill)
		if err != nil {
			continue
		}
		if native, err := moneyutil.RailMinorToNative(t.currency, minor); err == nil && native == t.amount {
			match = append(match, t)
		}
	}
	if len(match) != 1 {
		return priceTerms{}, false
	}
	return match[0], true
}
