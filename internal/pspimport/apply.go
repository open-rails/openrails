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
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/progress"
)

// applyCustomers is how many customers one billing import book carries.
const applyCustomers = 50

// Apply lands the import's linked records through billingimport as the
// provider's book, a book of whole customers at a time at the pull's
// horizon, then approves the import's corrections in the name of the person
// who asked and hands the held rows it read to the provider's evidence. A
// book the import refuses is retried one customer at a time, so one
// customer's refusal blocks only its own records.
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
	// Records OpenRails came to hold after the pull are held now. A host
	// book's row among them is diffed at its current version; a provider
	// row is left to its newer evidence.
	newly, err := im.classify(ctx, row)
	if err != nil {
		return err
	}
	customers, err := im.basisNow(ctx, row, newly)
	if err != nil {
		return err
	}
	if len(customers) > 0 {
		if err := im.propose(ctx, row, scope, customers, false); err != nil {
			return err
		}
	}
	prices, pspPrices, err := im.providerPrices(ctx, row)
	if err != nil {
		return err
	}
	l := lander{im: im, row: row, scope: scope, prices: prices, pspPrices: pspPrices, asOf: *row.PulledAt}
	if scope.Rail == "nmi" {
		// Refunds can be unmatched or linked to another customer. Inspect the
		// whole staged import before landing any standalone charge.
		l.refunds = map[string][]importRefund{}
		if err := im.eachRecord(ctx, row, billing.PSPImportTransaction, func(r gen.BillingPspImportRecord) error {
			var data transactionData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				return err
			}
			if data.Success && data.Type == "refund" {
				if data.OriginalTransactionID == "" {
					l.unqualifiedRefund = true
				} else {
					l.refunds[data.OriginalTransactionID] = append(l.refunds[data.OriginalTransactionID], importRefund{record: r, data: data})
				}
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
	if err := im.approveCorrections(ctx, row); err != nil {
		return err
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

// providerPrices are the live provider prices mapping the plans the linked
// subscriptions name, by plan. A rail whose subscriptions name no plan
// (CCBill) instead takes every live provider price of the PSP, archived ones
// included, by price.
func (im *Importer) providerPrices(ctx context.Context, row gen.BillingPspImport) (map[string]priceTerms, map[uuid.UUID]priceTerms, error) {
	if !planned[row.Rail] {
		rows, err := im.DB.Gen(ctx).ListPSPProviderPricesOfPSP(ctx, gen.ListPSPProviderPricesOfPSPParams{MerchantID: row.MerchantID, PspID: row.PspID})
		if err != nil {
			return nil, nil, err
		}
		out := make(map[uuid.UUID]priceTerms, len(rows))
		for _, r := range rows {
			// A subscription lands only on a recurring price.
			if t := termsOf(r.PublicID, r.Amount, r.Currency, r.BillingIntervalHours, r.BillingIntervalMonths); t.period() > 0 {
				out[r.ID] = t
			}
		}
		return nil, out, nil
	}
	out := map[string]priceTerms{}
	plans, err := im.DB.Gen(ctx).ListPSPImportPlans(ctx, gen.ListPSPImportPlansParams{MerchantID: row.MerchantID, ImportID: row.ID})
	if err != nil {
		return nil, nil, err
	}
	refs := make([]string, 0, len(plans))
	for _, p := range plans {
		if p != nil {
			refs = append(refs, *p)
		}
	}
	if len(refs) == 0 {
		return out, nil, nil
	}
	rows, err := im.DB.Gen(ctx).ListPSPProviderPrices(ctx, gen.ListPSPProviderPricesParams{MerchantID: row.MerchantID, PspID: row.PspID, Refs: refs})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		out[r.Ref] = termsOf(r.PublicID, r.Amount, r.Currency, r.BillingIntervalHours, r.BillingIntervalMonths)
	}
	return out, nil, nil
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
	im    *Importer
	row   gen.BillingPspImport
	scope merchants.PSPScope
	// prices map plans to provider prices; pspPrices are a plan-less rail's.
	prices            map[string]priceTerms
	pspPrices         map[uuid.UUID]priceTerms
	asOf              time.Time
	refunds           map[string][]importRefund
	unqualifiedRefund bool
}

// outcome is what landing did to one record.
type outcome struct {
	state  billing.PSPImportRecordState
	reason string
}

// declaration is a book and how its results map back to records.
type declaration struct {
	book billing.DeclaredBilling
	// customers are the vault records whose cards the book declares, and
	// reasons why one is not the customer's link at the PSP.
	customers []uuid.UUID
	reasons   map[uuid.UUID]string
	// pspCustomers maps a declared PSP customer's source id to its record.
	pspCustomers map[string]uuid.UUID
	// subscriptions maps a source id to its record, txns to its transactions.
	subscriptions map[string]uuid.UUID
	txns          map[string][]uuid.UUID
	receipts      map[string][]uuid.UUID
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
	return billingimport.Import(ctx, billingimport.Options{DB: im.DB, MerchantID: billing.MerchantID(row.MerchantID), Book: book, Clock: im.Clock,
		Authority: reconcile.ProviderFacts})
}

// record maps subscription and receipt results onto their staged records.
func (d *declaration) record(res billing.BillingImportResult) {
	for _, id := range d.customers {
		d.outcomes[id] = outcome{state: billing.PSPImportImported, reason: d.reasons[id]}
	}
	for src, id := range d.pspCustomers {
		if reason := res.Reasons[src]; reason != "" {
			d.outcomes[id] = outcome{state: billing.PSPImportImported, reason: reason}
		}
	}
	landed := map[string]bool{}
	for _, src := range append(append([]string{}, res.Imported...), res.Skipped...) {
		landed[src] = true
	}
	for src, ids := range d.receipts {
		for _, id := range ids {
			if landed[src] {
				d.outcomes[id] = outcome{state: billing.PSPImportImported}
			} else {
				d.outcomes[id] = outcome{billing.PSPImportBlocked, res.Reasons[src] + "; relink the staged records before applying again"}
			}
		}
	}
	for src, id := range d.subscriptions {
		if res.Reasons[src] == reconcile.SkippedHeld {
			d.outcomes[id] = outcome{billing.PSPImportBlocked, "OpenRails came to hold it after the pull; reconcile the PSP again"}
			for _, t := range d.txns[src] {
				d.outcomes[t] = outcome{billing.PSPImportBlocked, "its subscription is held; reconcile the PSP again"}
			}
			continue
		}
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
	d := declaration{receipts: map[string][]uuid.UUID{}, subscriptions: map[string]uuid.UUID{}, txns: map[string][]uuid.UUID{}, outcomes: map[uuid.UUID]outcome{},
		reasons: map[uuid.UUID]string{}, pspCustomers: map[string]uuid.UUID{}, book: billing.DeclaredBilling{AsOf: l.asOf, DefaultPSP: billing.PSPRef{ID: &pspID}}}
	for _, g := range groups {
		customer := billing.CustomerID(g.customer)
		d.book.Customers = append(d.book.Customers, billing.DeclaredCustomer{Customer: customer})
		if err := l.declarePSPCustomer(&d, customer, g.records); err != nil {
			return d, err
		}
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
				if r.State != string(billing.PSPImportHeld) {
					subs[r.Ref] = r
				}
			}
		}
		rebills := rebillsOf(g.records)
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
			if r.Kind != string(billing.PSPImportTransaction) || r.State != string(billing.PSPImportLinked) && !(l.scope.Rail == "nmi" && r.State == string(billing.PSPImportImported)) {
				continue
			}
			var data transactionData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				return d, err
			}
			if l.scope.Rail == "nmi" && data.Type == "refund" {
				d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "refund requires its qualified original payment group"}
				continue
			}
			if r.State == string(billing.PSPImportImported) && len(l.refunds[data.TransactionID]) == 0 {
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
				if !data.Success || data.Type != "" && data.Type != "sale" {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "history without a subscription requires a confirmed successful sale"}
					continue
				}
				if l.unqualifiedRefund {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "a successful refund has no original payment ID; create a fresh import to read its history before importing standalone payments"}
					continue
				}
				refunds, refundIDs, reason := l.historicalRefunds(g.customer, data)
				if reason != "" {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, reason}
					continue
				}
				if data.StripeInvoice != nil {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "invoice history requires its subscription; it cannot be replaced by a standalone payment"}
					continue
				}
				src := "history:" + r.ID.String()
				d.receipts[src] = append([]uuid.UUID{r.ID}, refundIDs...)
				d.book.HistoricalPayments = append(d.book.HistoricalPayments, billing.DeclaredHistoricalPayment{
					SourceID: src, Customer: customer, Rail: l.scope.Rail, TransactionID: data.TransactionID,
					Amount: data.Amount, Currency: data.Currency, PaidAt: data.OccurredAt, Refunds: refunds,
				})
				continue
			}
			src, reason := declareSub(sub)
			if reason != "" {
				d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "its subscription does not land: " + reason}
				continue
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
			receiptSource := ""
			if l.scope.Rail == "nmi" && data.Success && (data.Type == "" || data.Type == "sale") {
				if l.unqualifiedRefund {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, "a successful refund has no original payment ID; create a fresh import before importing payment history"}
					continue
				}
				refunds, refundIDs, reason := l.historicalRefunds(g.customer, data)
				for _, refund := range l.refunds[data.TransactionID] {
					if ref := deref(refund.record.SubscriptionRef); ref != "" && ref != subRef {
						reason = "the refund names a different subscription from its original payment"
					}
				}
				if reason != "" {
					d.outcomes[r.ID] = outcome{billing.PSPImportBlocked, reason}
					continue
				}
				receiptSource = "receipt:" + r.ID.String()
				d.receipts[receiptSource] = []uuid.UUID{r.ID}
				for i, refund := range refunds {
					source := "receipt:" + refundIDs[i].String()
					d.receipts[source] = []uuid.UUID{refundIDs[i]}
					d.book.Transactions = append(d.book.Transactions, billing.DeclaredTransaction{
						SourceID: source, RailSubscriptionID: subRef, TransactionID: refund.TransactionID, OriginalTransactionID: data.TransactionID,
						Type: "refund", Success: true, Amount: refund.Amount, Currency: data.Currency, OccurredAt: refund.RefundedAt,
					})
				}
			}
			typ := data.Type
			if typ == "sale" {
				typ = ""
			}
			d.book.Transactions = append(d.book.Transactions, billing.DeclaredTransaction{SourceID: receiptSource, RailSubscriptionID: subRef, TransactionID: data.TransactionID, Type: typ,
				Success: data.Success, Amount: data.Amount, Currency: data.Currency, OccurredAt: data.OccurredAt, StripeInvoice: data.StripeInvoice})
			if receiptSource == "" {
				d.txns[src] = append(d.txns[src], r.ID)
			}
		}
	}
	return d, nil
}

// declarePSPCustomer links the customer to its customer object at a PSP whose
// rail has one (a Stripe cus_), so later provider events on it attribute to
// the customer. Of several, it picks the one a live subscription bills, else
// the newest; the others say why they are not linked.
func (l *lander) declarePSPCustomer(d *declaration, customer billing.CustomerID, records []gen.BillingPspImportRecord) error {
	if !rails.HasRemoteCustomer(models.Rail(l.scope.Rail)) {
		return nil
	}
	billed := map[string]bool{}
	var linked []gen.BillingPspImportRecord
	for _, r := range records {
		switch billing.PSPImportRecordKind(r.Kind) {
		case billing.PSPImportCustomer:
			if r.State == string(billing.PSPImportLinked) {
				linked = append(linked, r)
			}
		case billing.PSPImportSubscription:
			var data subscriptionData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				return err
			}
			if !data.Ended && r.CustomerRef != nil {
				billed[*r.CustomerRef] = true
			}
		}
	}
	if len(linked) == 0 {
		return nil
	}
	created := func(r gen.BillingPspImportRecord) time.Time {
		if r.OccurredAt == nil {
			return time.Time{}
		}
		return *r.OccurredAt
	}
	newer := func(a, b gen.BillingPspImportRecord) bool {
		return created(a).After(created(b)) || created(a).Equal(created(b)) && a.Ref > b.Ref
	}
	pick := linked[0]
	for _, r := range linked[1:] {
		if billed[r.Ref] != billed[pick.Ref] {
			if billed[r.Ref] {
				pick = r
			}
			continue
		}
		if newer(r, pick) {
			pick = r
		}
	}
	pspID := billing.PSPID(l.scope.ID)
	src := "pspimport:" + l.scope.Rail + ":customer:" + pick.Ref
	d.book.PSPCustomers = append(d.book.PSPCustomers, billing.DeclaredPSPCustomer{SourceID: src, Customer: customer, Rail: l.scope.Rail,
		PSP: billing.PSPRef{ID: &pspID}, RemoteCustomerRef: pick.Ref})
	d.pspCustomers[src] = pick.ID
	for _, r := range linked {
		if r.ID != pick.ID {
			d.reasons[r.ID] = fmt.Sprintf("the customer's link at this PSP is %s; %s was not linked", pick.Ref, r.Ref)
		}
	}
	return nil
}

// subscription declares one staged subscription; reason says why it cannot.
func (l *lander) subscription(customer billing.CustomerID, r gen.BillingPspImportRecord, vaults map[string]customerData, rebill rebills) (billing.DeclaredSubscription, priceTerms, string, error) {
	var data subscriptionData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return billing.DeclaredSubscription{}, priceTerms{}, "", err
	}
	price, reason := l.priceOf(r, data.Plan, rebill, rebill.cadence(data.NextBillingAt))
	switch {
	case reason != "":
		return billing.DeclaredSubscription{}, price, reason, nil
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

// rebills are a subscription's latest two currency-less charges: the
// latest's amount and when each was.
type rebills struct {
	decimal        string
	at, previousAt time.Time
}

// rebillsOf is each subscription's latest rebills.
func rebillsOf(records []gen.BillingPspImportRecord) map[string]rebills {
	out := map[string]rebills{}
	for _, r := range records {
		if r.Kind != string(billing.PSPImportTransaction) || r.SubscriptionRef == nil {
			continue
		}
		var data transactionData
		if json.Unmarshal(r.Data, &data) != nil || data.Decimal == "" || data.Type != "sale" || !data.Success {
			continue
		}
		ref, at := *r.SubscriptionRef, data.OccurredAt
		switch rb := out[ref]; {
		case at.After(rb.at):
			out[ref] = rebills{decimal: data.Decimal, at: at, previousAt: rb.at}
		case at.Before(rb.at) && at.After(rb.previousAt):
			rb.previousAt = at
			out[ref] = rb
		}
	}
	return out
}

// cadence is how often the subscription rebills, as observed, in whole
// calendar days: from its latest rebill to the next the provider schedules
// (or to the expiry of a canceled one), else between its latest two rebills;
// 0 when unknown. The provider schedules dates, not times: a rebill counts by
// its date, so the time of day it ran never shifts the cadence.
func (rb rebills) cadence(next *time.Time) time.Duration {
	at, previous := calendarDay(rb.at), calendarDay(rb.previousAt)
	switch {
	case rb.at.IsZero():
		return 0
	case next != nil && calendarDay(*next).After(at):
		return calendarDay(*next).Sub(at)
	case !rb.previousAt.IsZero() && at.After(previous):
		return at.Sub(previous)
	}
	return 0
}

// calendarDay is t's date, as the provider stated it (parsed as UTC).
func calendarDay(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// period is a price's billing interval, 0 for a price that does not recur.
// A calendar month counts its average length.
func (t priceTerms) period() time.Duration {
	if t.months > 0 {
		return time.Duration(t.months) * 730 * time.Hour
	}
	return time.Duration(t.hours) * time.Hour
}

// agrees reports whether an observed cadence is the price's interval, give
// or take a tenth of it (at least two days): calendar months and date-only
// provider schedules drift by days.
func (t priceTerms) agrees(cadence time.Duration) bool {
	period := t.period()
	tolerance := max(period/10, 48*time.Hour)
	diff := cadence - period
	return -tolerance <= diff && diff <= tolerance
}

// priceOf is the provider price a subscription lands on, or why it has none.
// A plan lands on the live provider price mapping it. A plan-less (CCBill)
// subscription lands on the price picked for it by hand, else on the one
// recurring provider price of the PSP its latest rebill equals, read in that
// price's currency, whose interval agrees with the cadence it rebills at
// when that is known.
func (l *lander) priceOf(r gen.BillingPspImportRecord, plan string, rebill rebills, cadence time.Duration) (priceTerms, string) {
	if planned[l.scope.Rail] {
		if plan == "" {
			return priceTerms{}, "the provider names no plan for it"
		}
		if price, ok := l.prices[plan]; ok {
			return price, ""
		}
		return priceTerms{}, fmt.Sprintf("plan %q is unmapped: map it, then link and apply again", plan)
	}
	const pick = "pick its price with LinkPSPImport prices"
	if r.ManualPriceID != nil {
		if price, ok := l.pspPrices[*r.ManualPriceID]; ok {
			return price, ""
		}
		return priceTerms{}, "its picked price is no longer a live recurring provider price of this PSP: " + pick
	}
	if rebill.decimal == "" {
		return priceTerms{}, "it has no rebill: 0 match among this PSP's recurring provider prices; " + pick
	}
	var match []priceTerms
	for _, t := range l.pspPrices {
		minor, err := moneyutil.DecimalToRailMinor(t.currency, rebill.decimal)
		if err != nil {
			continue
		}
		if native, err := moneyutil.RailMinorToNative(t.currency, minor); err == nil && native == t.amount && (cadence == 0 || t.agrees(cadence)) {
			match = append(match, t)
		}
	}
	if len(match) != 1 {
		every := ""
		if cadence > 0 {
			every = fmt.Sprintf(" every %d days", int(cadence/(24*time.Hour)))
		}
		return priceTerms{}, fmt.Sprintf("its latest rebill %s%s: %d match among this PSP's recurring provider prices; %s", rebill.decimal, every, len(match), pick)
	}
	return match[0], ""
}
