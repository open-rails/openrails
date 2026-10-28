package pspimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Link stores the merchant's manual links and price picks, then relinks the
// import in the background: its job links every staged record not yet
// imported by the import's rules, marks the ones OpenRails holds held and
// proposes again. The import is pulling until it has. An import a later
// pull of its PSP superseded is not linked again.
func (im *Importer) Link(ctx context.Context, id billing.PSPImportID, p billing.LinkPSPImportParams) (*billing.PSPImport, error) {
	if len(p.Links) > billing.MaxBatchItems {
		return nil, apperr.Invalidf("links holds at most %d items", billing.MaxBatchItems).WithParam("links")
	}
	if len(p.Prices) > billing.MaxBatchItems {
		return nil, apperr.Invalidf("prices holds at most %d items", billing.MaxBatchItems).WithParam("prices")
	}
	row, err := im.row(ctx, id)
	if err != nil {
		return nil, err
	}
	var out gen.BillingPspImport
	err = im.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := im.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		locked, err := q.LockPSPImport(ctx, gen.LockPSPImportParams{MerchantID: row.MerchantID, ID: row.ID})
		if err != nil {
			return err
		}
		if locked.Status != string(billing.PSPImportPulled) && locked.Status != string(billing.PSPImportApplied) {
			return apperr.Conflictf("the import is %s; link it once it is pulled", locked.Status)
		}
		if err := superseded(ctx, q, locked); err != nil {
			return err
		}
		if err := im.pickPrices(ctx, q, locked, p.Prices); err != nil {
			return err
		}
		if err := setManualLinks(ctx, q, locked, p.Links); err != nil {
			return err
		}
		if out, err = q.SetPSPImportStatus(ctx, gen.SetPSPImportStatusParams{MerchantID: row.MerchantID, ID: row.ID, Status: string(billing.PSPImportPulling)}); err != nil {
			return inFlight(err)
		}
		_, err = d.InsertRiverJob(ctx, JobArgs{MerchantID: row.MerchantID, ImportID: row.ID, Step: StepRelink}, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return im.view(ctx, out)
}

// superseded refuses to link or apply an import once a later import of its
// PSP pulled: that one read the provider since, and its proposals are the
// current ones.
func superseded(ctx context.Context, q *gen.Queries, row gen.BillingPspImport) error {
	later, err := q.GetPSPImportSuccessor(ctx, gen.GetPSPImportSuccessorParams{MerchantID: row.MerchantID, PspID: row.PspID, CreatedAt: row.CreatedAt})
	if err != nil || len(later) == 0 {
		return err
	}
	return apperr.Conflictf("import %s of the PSP pulled after this one; link or apply that one", billing.PSPImportID(later[0]))
}

// pickPrices stores the provider prices picked by hand for subscriptions
// whose provider names no plan: each a live recurring provider price bound
// to the import's PSP.
func (im *Importer) pickPrices(ctx context.Context, q *gen.Queries, row gen.BillingPspImport, picks []billing.PSPImportPriceLink) error {
	if len(picks) == 0 {
		return nil
	}
	if planned[row.Rail] {
		return apperr.Invalidf("prices: a %s subscription lands on the provider price mapping its plan; a price is picked only on a rail whose subscriptions name none (CCBill)", row.Rail).WithParam("prices")
	}
	live, err := q.ListPSPProviderPricesOfPSP(ctx, gen.ListPSPProviderPricesOfPSPParams{MerchantID: row.MerchantID, PspID: row.PspID})
	if err != nil {
		return err
	}
	byRef := make(map[billing.PriceID]uuid.UUID, len(live))
	for _, r := range live {
		if termsOf(r.PublicID, r.Amount, r.Currency, r.BillingIntervalHours, r.BillingIntervalMonths).period() > 0 {
			byRef[billing.PriceID(r.PublicID)] = r.ID
		}
	}
	p := gen.SetPSPImportRecordManualPricesParams{MerchantID: row.MerchantID, ImportID: row.ID}
	for i, pick := range picks {
		param := fmt.Sprintf("prices[%d]", i)
		if pick.Record.IsZero() {
			return apperr.Invalidf("%s.record is required", param).WithParam(param + ".record")
		}
		price := uuid.Nil
		if pick.Price != nil && !pick.Price.IsZero() {
			id, ok := byRef[*pick.Price]
			if !ok {
				return apperr.Invalidf("%s.price %s is not a live recurring provider price of the import's PSP", param, *pick.Price).WithParam(param + ".price")
			}
			price = id
		}
		p.Ids, p.PriceIds = append(p.Ids, pick.Record.UUID()), append(p.PriceIds, price)
	}
	records, err := q.ListPSPImportRecordsByIDs(ctx, gen.ListPSPImportRecordsByIDsParams{MerchantID: row.MerchantID, ImportID: row.ID, Ids: p.Ids})
	if err != nil {
		return err
	}
	kinds := make(map[uuid.UUID]string, len(records))
	for _, r := range records {
		kinds[r.ID] = r.Kind
	}
	for i, id := range p.Ids {
		param := fmt.Sprintf("prices[%d].record", i)
		switch kind, ok := kinds[id]; {
		case !ok:
			return apperr.New(404, billing.CodeResourceNotFound, "a priced record is not one of the import's").WithParam(param)
		case kind != string(billing.PSPImportSubscription):
			return apperr.Invalidf("%s is a %s: a price is picked for a subscription", param, kind).WithParam(param)
		}
	}
	_, err = q.SetPSPImportRecordManualPrices(ctx, p)
	return err
}

// candidate is one customer a rule, a link or a parent object names.
type candidate struct {
	customer uuid.UUID
	via      string
}

type linkUpdate struct {
	id       uuid.UUID
	state    billing.PSPImportRecordState
	customer uuid.UUID
	via      string
	reason   string
}

// linker evaluates rules with its lookups cached for one pass.
type linker struct {
	im     *Importer
	mid    billing.MerchantID
	rules  []rule
	mapped map[string]map[string]uuid.UUID
	dir    map[string][]uuid.UUID
}

// setManualLinks stores the records a person linked by hand.
func setManualLinks(ctx context.Context, q *gen.Queries, row gen.BillingPspImport, links []billing.PSPImportLink) error {
	if len(links) > 0 {
		p := gen.SetPSPImportRecordManualLinksParams{MerchantID: row.MerchantID, ImportID: row.ID}
		for i, l := range links {
			if l.Record.IsZero() {
				return apperr.Invalidf("links[%d].record is required", i).WithParam(fmt.Sprintf("links[%d].record", i))
			}
			customer := uuid.Nil
			if l.Customer != nil {
				customer = l.Customer.UUID()
			}
			p.Ids, p.CustomerIds = append(p.Ids, l.Record.UUID()), append(p.CustomerIds, customer)
		}
		n, err := q.SetPSPImportRecordManualLinks(ctx, p)
		if err != nil {
			return err
		}
		if int(n) != len(links) {
			return apperr.New(404, billing.CodeResourceNotFound, "a linked record is not one of the import's").WithParam("links")
		}
	}
	return nil
}

// link links every staged record not yet imported by the import's rules and
// manual links, then marks the ones OpenRails holds held. It returns the held
// subscriptions.
func (im *Importer) link(ctx context.Context, row gen.BillingPspImport) ([]heldSubscription, error) {
	mid := billing.MerchantID(row.MerchantID)
	var declared []billing.PSPImportRule
	if err := json.Unmarshal(row.Rules, &declared); err != nil {
		return nil, err
	}
	rules, err := compileRules(declared)
	if err != nil {
		return nil, err
	}
	l := &linker{im: im, mid: mid, rules: rules, mapped: map[string]map[string]uuid.UUID{}, dir: map[string][]uuid.UUID{}}
	custOf := map[string]uuid.UUID{}
	subOf := map[string]uuid.UUID{}
	// conflicted are the customer and subscription refs whose rules disagree:
	// what belongs to them is not linked either.
	conflicted := map[string]bool{}
	subsOfVault := map[string][]candidate{}
	var unmatchedVaults []gen.BillingPspImportRecord

	if err := l.pass(ctx, row, billing.PSPImportCustomer, func(r gen.BillingPspImportRecord) ([]candidate, string) { return nil, "" },
		func(r gen.BillingPspImportRecord, u linkUpdate) {
			switch {
			case u.customer != uuid.Nil:
				custOf[r.Ref] = u.customer
			case u.state == billing.PSPImportUnmatched:
				unmatchedVaults = append(unmatchedVaults, r)
			case u.state == billing.PSPImportConflict:
				conflicted["customer:"+r.Ref] = true
			}
		}); err != nil {
		return nil, err
	}
	if err := l.pass(ctx, row, billing.PSPImportSubscription, func(r gen.BillingPspImportRecord) ([]candidate, string) {
		vault := "customer:" + deref(r.CustomerRef)
		if conflicted[vault] {
			return nil, vault + " is in conflict"
		}
		if c, ok := custOf[deref(r.CustomerRef)]; ok {
			return []candidate{{c, vault}}, ""
		}
		return nil, ""
	}, func(r gen.BillingPspImportRecord, u linkUpdate) {
		switch {
		case u.customer != uuid.Nil:
			subOf[r.Ref] = u.customer
			if vault := deref(r.CustomerRef); vault != "" {
				subsOfVault[vault] = append(subsOfVault[vault], candidate{u.customer, "subscription:" + r.Ref})
			}
		case u.state == billing.PSPImportConflict:
			conflicted["subscription:"+r.Ref] = true
		}
	}); err != nil {
		return nil, err
	}
	// A vault no rule linked belongs to the customer all its subscriptions name.
	var adopted []linkUpdate
	for _, r := range unmatchedVaults {
		subs := subsOfVault[r.Ref]
		if len(subs) == 0 || len(distinct(subs)) != 1 {
			continue
		}
		custOf[r.Ref] = subs[0].customer
		adopted = append(adopted, linkUpdate{id: r.ID, state: billing.PSPImportLinked, customer: subs[0].customer, via: subs[0].via})
	}
	if err := im.writeLinks(ctx, row, adopted); err != nil {
		return nil, err
	}
	if err := l.pass(ctx, row, billing.PSPImportTransaction, func(r gen.BillingPspImportRecord) ([]candidate, string) {
		sub, vault := "subscription:"+deref(r.SubscriptionRef), "customer:"+deref(r.CustomerRef)
		for _, parent := range []string{sub, vault} {
			if conflicted[parent] {
				return nil, parent + " is in conflict"
			}
		}
		var out []candidate
		if c, ok := subOf[deref(r.SubscriptionRef)]; ok {
			out = append(out, candidate{c, sub})
		}
		if c, ok := custOf[deref(r.CustomerRef)]; ok {
			out = append(out, candidate{c, vault})
		}
		return out, ""
	}, func(gen.BillingPspImportRecord, linkUpdate) {}); err != nil {
		return nil, err
	}
	return im.classify(ctx, row)
}

// pass links one kind of record, page by page.
func (l *linker) pass(ctx context.Context, row gen.BillingPspImport, kind billing.PSPImportRecordKind, parents func(gen.BillingPspImportRecord) ([]candidate, string), done func(gen.BillingPspImportRecord, linkUpdate)) error {
	var page []gen.BillingPspImportRecord
	flush := func() error {
		if err := l.prefetch(ctx, page); err != nil {
			return err
		}
		updates := make([]linkUpdate, 0, len(page))
		for _, r := range page {
			u := linkUpdate{id: r.ID}
			if r.State == string(billing.PSPImportImported) && r.CustomerID != nil {
				u.state, u.customer = billing.PSPImportImported, *r.CustomerID
			} else {
				cands, conflict := parents(r)
				u, err := l.decide(ctx, r, cands, conflict)
				if err != nil {
					return err
				}
				updates = append(updates, u)
				done(r, u)
				continue
			}
			done(r, u)
		}
		page = page[:0]
		return l.im.writeLinks(ctx, row, updates)
	}
	err := l.im.eachRecord(ctx, row, kind, func(r gen.BillingPspImportRecord) error {
		page = append(page, r)
		if len(page) >= stageBatch {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

// decide links one record: a manual link wins; otherwise every candidate a
// rule or a parent object names must agree.
func (l *linker) decide(ctx context.Context, r gen.BillingPspImportRecord, parents []candidate, parentConflict string) (linkUpdate, error) {
	u := linkUpdate{id: r.ID, state: billing.PSPImportUnmatched}
	if r.ManualCustomerID != nil {
		u.state, u.customer, u.via = billing.PSPImportLinked, *r.ManualCustomerID, "manual"
		return u, nil
	}
	if parentConflict != "" {
		u.state, u.reason = billing.PSPImportConflict, parentConflict
		return u, nil
	}
	var f facts
	if err := json.Unmarshal(r.Facts, &f); err != nil {
		return u, err
	}
	cands := append([]candidate(nil), parents...)
	var notes []string
	for i, rule := range l.rules {
		via := fmt.Sprintf("rule:%d", i+1)
		for _, value := range f[rule.Fact] {
			key := rule.key(value)
			if key == "" {
				continue
			}
			switch rule.Via {
			case billing.PSPImportViaMapping:
				if c, ok := l.mapped[rule.Mapping][key]; ok {
					cands = append(cands, candidate{c, via})
				}
			case billing.PSPImportViaCustomerID:
				if c, ok := customerIDKey(key); ok {
					cands = append(cands, candidate{c, via})
				}
			case billing.PSPImportViaDirectory:
				if len(parents) > 0 {
					continue // the object it belongs to already names its customer
				}
				found, err := l.directory(ctx, rule.Fact, key)
				if err != nil {
					return u, err
				}
				if len(found) > 1 {
					notes = append(notes, fmt.Sprintf("%s: %d customers hold %s %q", via, len(found), rule.Fact, key))
					continue
				}
				for _, c := range found {
					cands = append(cands, candidate{c, via})
				}
			}
		}
	}
	switch set := distinct(cands); {
	case len(set) == 1 && len(notes) == 0:
		u.state, u.customer, u.via = billing.PSPImportLinked, cands[0].customer, cands[0].via
	case len(set) > 1 || len(notes) > 0:
		for _, c := range cands {
			notes = append(notes, fmt.Sprintf("%s names %s", c.via, c.customer))
		}
		sort.Strings(notes)
		u.state, u.reason = billing.PSPImportConflict, strings.Join(notes, "; ")
	}
	return u, nil
}

func distinct(cands []candidate) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, c := range cands {
		out[c.customer] = true
	}
	return out
}

// prefetch loads the mappings a page of records looks up, one query per mapping.
func (l *linker) prefetch(ctx context.Context, page []gen.BillingPspImportRecord) error {
	keys := map[string][]string{}
	for _, r := range page {
		var f facts
		if err := json.Unmarshal(r.Facts, &f); err != nil {
			return err
		}
		for _, rule := range l.rules {
			if rule.Via != billing.PSPImportViaMapping {
				continue
			}
			for _, v := range f[rule.Fact] {
				if k := rule.key(v); k != "" {
					if _, cached := l.mapped[rule.Mapping][k]; !cached {
						keys[rule.Mapping] = append(keys[rule.Mapping], k)
					}
				}
			}
		}
	}
	for mapping, ks := range keys {
		rows, err := l.im.DB.Gen(ctx).LookupPSPImportMappings(ctx, gen.LookupPSPImportMappingsParams{MerchantID: l.mid.UUID(), Mapping: mapping, Keys: ks})
		if err != nil {
			return err
		}
		if l.mapped[mapping] == nil {
			l.mapped[mapping] = map[string]uuid.UUID{}
		}
		for _, row := range rows {
			l.mapped[mapping][row.Key] = row.CustomerID
		}
	}
	return nil
}

// directoryLimit bounds one directory search: more holders is ambiguous anyway.
const directoryLimit = 10

// directory is the customers whose directory email or username is key.
func (l *linker) directory(ctx context.Context, fact, key string) ([]uuid.UUID, error) {
	cacheKey := fact + "\x1f" + strings.ToLower(key)
	if found, ok := l.dir[cacheKey]; ok {
		return found, nil
	}
	if l.im.Directory == nil {
		return nil, nil
	}
	contacts, err := l.im.Directory.Search(ctx, l.mid, key, directoryLimit)
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	seen := map[uuid.UUID]bool{}
	var found []uuid.UUID
	for _, c := range contacts {
		value := c.Email
		if fact == FactUsername {
			value = c.Username
		}
		if strings.EqualFold(strings.TrimSpace(value), key) && !seen[c.CustomerID] {
			seen[c.CustomerID] = true
			found = append(found, c.CustomerID)
		}
	}
	l.dir[cacheKey] = found
	return found, nil
}

func (im *Importer) writeLinks(ctx context.Context, row gen.BillingPspImport, updates []linkUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	p := gen.SetPSPImportRecordLinksParams{MerchantID: row.MerchantID, ImportID: row.ID}
	for _, u := range updates {
		p.Ids, p.States, p.CustomerIds = append(p.Ids, u.id), append(p.States, string(u.state)), append(p.CustomerIds, u.customer)
		p.LinkedVias, p.Reasons = append(p.LinkedVias, u.via), append(p.Reasons, u.reason)
	}
	err := im.DB.Gen(ctx).SetPSPImportRecordLinks(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
