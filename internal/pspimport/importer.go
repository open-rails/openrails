// Package pspimport imports a merchant's subscribers straight from its PSPs:
// it pulls each provider object read-only, stages it with the identity facts
// the provider holds, links it to a customer by the merchant's rules and
// mappings, and lands the linked ones through billingimport. Objects no rule
// links stay staged and listed; nothing is guessed.
package pspimport

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
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Importer runs PSP imports for the merchant in ctx.
type Importer struct {
	DB        *db.DB
	Merchants *merchants.Service
	// NMI builds the import's NMI clients; they are made read-only.
	NMI       *railresolve.NMIFactory
	Directory identity.Directory
	Clock     clockwork.Clock
}

// ErrNotFound is an import or record the merchant does not have.
var ErrNotFound = apperr.New(404, billing.CodeResourceNotFound, "PSP import not found")

// Step is what an import's job does next.
type Step string

const (
	StepPull  Step = "pull"
	StepApply Step = "apply"
)

func (im *Importer) now() time.Time { return timeutil.FirstClock(im.Clock).Now().UTC() }

// Create stages a new import of one PSP; the caller queues its pull.
func (im *Importer) Create(ctx context.Context, p billing.CreatePSPImportParams) (*billing.PSPImport, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := compileRules(p.Rules); err != nil {
		return nil, err
	}
	if err := checkProducts(p.Products); err != nil {
		return nil, err
	}
	scope, err := im.psp(ctx, mid, p.PSP)
	if err != nil {
		return nil, err
	}
	if _, ok := sources[scope.Rail]; !ok {
		return nil, apperr.Invalidf("PSP %q is on %s, which has no import yet", scope.Key, scope.Rail).WithParam("psp")
	}
	rules, products, err := encodeConfig(p.Rules, p.Products)
	if err != nil {
		return nil, err
	}
	var since *time.Time
	if p.HistorySince != nil {
		t := p.HistorySince.UTC()
		since = &t
	}
	row, err := im.DB.Gen(ctx).CreatePSPImport(ctx, gen.CreatePSPImportParams{MerchantID: mid.UUID(), PspID: scope.ID, Rail: scope.Rail,
		HistorySince: since, Rules: rules, Products: products})
	if err != nil {
		return nil, err
	}
	return im.view(ctx, row)
}

// psp resolves a PSP reference to one of the merchant's PSPs.
func (im *Importer) psp(ctx context.Context, mid billing.MerchantID, ref billing.PSPRef) (merchants.PSPScope, error) {
	if ref.IsZero() || (ref.ID != nil && !ref.ID.IsZero() && strings.TrimSpace(ref.Key) != "") {
		return merchants.PSPScope{}, apperr.Invalidf("psp names one PSP by id or key").WithParam("psp")
	}
	var scope merchants.PSPScope
	var found bool
	var err error
	if ref.ID != nil && !ref.ID.IsZero() {
		scope, found, err = im.Merchants.PSPScopeByID(ctx, mid, ref.ID.UUID())
	} else {
		scope, found, err = im.Merchants.PSPScopeByKey(ctx, mid, ref.Key)
	}
	if err != nil {
		return scope, err
	}
	if !found {
		return scope, apperr.New(404, "psp_not_found", fmt.Sprintf("PSP %s not found", ref.String())).WithParam("psp")
	}
	return scope, nil
}

func encodeConfig(rules []billing.PSPImportRule, products []billing.PSPImportProduct) ([]byte, []byte, error) {
	if rules == nil {
		rules = DefaultRules
	}
	if products == nil {
		products = []billing.PSPImportProduct{}
	}
	r, err := json.Marshal(rules)
	if err != nil {
		return nil, nil, err
	}
	p, err := json.Marshal(products)
	return r, p, err
}

// Get reads one import with its records counted.
func (im *Importer) Get(ctx context.Context, id billing.PSPImportID) (*billing.PSPImport, error) {
	row, err := im.row(ctx, id)
	if err != nil {
		return nil, err
	}
	return im.view(ctx, row)
}

func (im *Importer) row(ctx context.Context, id billing.PSPImportID) (gen.BillingPspImport, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingPspImport{}, err
	}
	row, err := im.DB.Gen(ctx).GetPSPImport(ctx, gen.GetPSPImportParams{MerchantID: mid.UUID(), ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrNotFound
	}
	return row, err
}

// List reads the merchant's imports, newest first.
func (im *Importer) List(ctx context.Context, p billing.PSPImportListParams) (billing.ListPage[billing.PSPImport], error) {
	var out billing.ListPage[billing.PSPImport]
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	if p.IDs != nil {
		rows, err := im.DB.Gen(ctx).ListPSPImportsByIDs(ctx, gen.ListPSPImportsByIDsParams{MerchantID: mid.UUID(), Ids: uuidutil.Of(p.IDs)})
		if err != nil {
			return out, err
		}
		for _, row := range rows {
			v, err := im.view(ctx, row)
			if err != nil {
				return out, err
			}
			out.Items = append(out.Items, *v)
		}
		return out, nil
	}
	page := p.PageRequest
	limit, err := pagination.Limit(page)
	if err != nil {
		return out, err
	}
	afterAt, afterID, err := pagination.After(page.Cursor)
	if err != nil {
		return out, err
	}
	rows, err := im.DB.Gen(ctx).ListPSPImportsPage(ctx, gen.ListPSPImportsPageParams{MerchantID: mid.UUID(), AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit)})
	if err != nil {
		return out, err
	}
	items := make([]billing.PSPImport, 0, len(rows))
	for _, row := range rows {
		v, err := im.view(ctx, row)
		if err != nil {
			return out, err
		}
		items = append(items, *v)
	}
	return pagination.Cut(items, limit, func(v billing.PSPImport) any {
		return pagination.TimeID{At: v.CreatedAt, ID: v.ID.UUID()}
	}), nil
}

// Update replaces the import's rules or products.
func (im *Importer) Update(ctx context.Context, id billing.PSPImportID, p billing.UpdatePSPImportParams) (*billing.PSPImport, error) {
	row, err := im.row(ctx, id)
	if err != nil {
		return nil, err
	}
	var rules []billing.PSPImportRule
	var products []billing.PSPImportProduct
	if err := json.Unmarshal(row.Rules, &rules); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(row.Products, &products); err != nil {
		return nil, err
	}
	if p.Rules != nil {
		if _, err := compileRules(*p.Rules); err != nil {
			return nil, err
		}
		rules = *p.Rules
	}
	if p.Products != nil {
		if err := checkProducts(*p.Products); err != nil {
			return nil, err
		}
		products = *p.Products
	}
	r, pr, err := encodeConfig(rules, products)
	if err != nil {
		return nil, err
	}
	row, err = im.DB.Gen(ctx).UpdatePSPImportConfig(ctx, gen.UpdatePSPImportConfigParams{MerchantID: row.MerchantID, ID: row.ID, Rules: r, Products: pr})
	if err != nil {
		return nil, err
	}
	return im.view(ctx, row)
}

// Delete removes an import and its staged records. What it landed stays.
func (im *Importer) Delete(ctx context.Context, id billing.PSPImportID) error {
	row, err := im.row(ctx, id)
	if err != nil {
		return err
	}
	if busy(row.Status) {
		return apperr.Conflictf("the import is %s; delete it when its job finishes", row.Status)
	}
	_, err = im.DB.Gen(ctx).DeletePSPImport(ctx, gen.DeletePSPImportParams{MerchantID: row.MerchantID, ID: row.ID})
	return err
}

func busy(status string) bool {
	return status == string(billing.PSPImportPulling) || status == string(billing.PSPImportApplying)
}

// ListRecords reads an import's staged records in id order.
func (im *Importer) ListRecords(ctx context.Context, id billing.PSPImportID, p billing.PSPImportRecordListParams) (billing.ListPage[billing.PSPImportRecord], error) {
	var out billing.ListPage[billing.PSPImportRecord]
	row, err := im.row(ctx, id)
	if err != nil {
		return out, err
	}
	if p.IDs != nil {
		rows, err := im.DB.Gen(ctx).ListPSPImportRecordsByIDs(ctx, gen.ListPSPImportRecordsByIDsParams{MerchantID: row.MerchantID, ImportID: row.ID, Ids: uuidutil.Of(p.IDs)})
		if err != nil {
			return out, err
		}
		for _, r := range rows {
			out.Items = append(out.Items, recordView(r))
		}
		return out, nil
	}
	limit, err := pagination.Limit(p.PageRequest)
	if err != nil {
		return out, err
	}
	var after recordPosition
	if _, err := pagination.Decode(p.Cursor, &after); err != nil {
		return out, err
	}
	params := gen.ListPSPImportRecordsPageParams{MerchantID: row.MerchantID, ImportID: row.ID, AfterID: after.ID, RowLimit: pagination.Fetch(limit)}
	if p.State != "" {
		params.State = new(string(p.State))
	}
	if p.Kind != "" {
		params.Kind = new(string(p.Kind))
	}
	rows, err := im.DB.Gen(ctx).ListPSPImportRecordsPage(ctx, params)
	if err != nil {
		return out, err
	}
	items := make([]billing.PSPImportRecord, 0, len(rows))
	for _, r := range rows {
		items = append(items, recordView(r))
	}
	return pagination.Cut(items, limit, func(v billing.PSPImportRecord) any { return recordPosition{ID: v.ID.UUID()} }), nil
}

// recordPosition is a record list's keyset position: records list in id order.
type recordPosition struct {
	ID uuid.UUID `json:"i"`
}

// SetMappings writes the merchant's mappings; a null customer deletes one.
func (im *Importer) SetMappings(ctx context.Context, p billing.SetPSPImportMappingsParams) (*billing.PSPImportMappingsResult, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if len(p.Mappings) == 0 || len(p.Mappings) > billing.MaxBatchItems {
		return nil, apperr.Invalidf("mappings holds 1 to %d items", billing.MaxBatchItems).WithParam("mappings")
	}
	var set, del gen.UpsertPSPImportMappingsParams
	seen := map[string]bool{}
	for i, m := range p.Mappings {
		param := fmt.Sprintf("mappings[%d]", i)
		if !mappingName.MatchString(m.Mapping) {
			return nil, apperr.Invalidf("%s.mapping %q is not a mapping name", param, m.Mapping).WithParam(param + ".mapping")
		}
		if m.Key != strings.TrimSpace(m.Key) || m.Key == "" || len(m.Key) > 256 {
			return nil, apperr.Invalidf("%s.key must be 1 to 256 bytes without surrounding space", param).WithParam(param + ".key")
		}
		if seen[m.Mapping+"\x1f"+m.Key] {
			return nil, apperr.Invalidf("%s repeats a key", param).WithParam(param + ".key")
		}
		seen[m.Mapping+"\x1f"+m.Key] = true
		if m.Customer == nil || m.Customer.IsZero() {
			del.Mappings, del.Keys = append(del.Mappings, m.Mapping), append(del.Keys, m.Key)
			continue
		}
		set.Mappings, set.Keys, set.CustomerIds = append(set.Mappings, m.Mapping), append(set.Keys, m.Key), append(set.CustomerIds, m.Customer.UUID())
	}
	out := &billing.PSPImportMappingsResult{}
	err = im.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := im.DB.NewWithPgxTx(tx).Gen(ctx)
		if len(set.Keys) > 0 {
			set.MerchantID = mid.UUID()
			n, err := q.UpsertPSPImportMappings(ctx, set)
			if err != nil {
				return err
			}
			out.Set = int(n)
		}
		if len(del.Keys) > 0 {
			n, err := q.DeletePSPImportMappings(ctx, gen.DeletePSPImportMappingsParams{MerchantID: mid.UUID(), Mappings: del.Mappings, Keys: del.Keys})
			if err != nil {
				return err
			}
			out.Deleted = int(n)
		}
		return nil
	})
	return out, err
}

// RequestApply checks an import can land and marks it applying; the caller
// queues the job.
func (im *Importer) RequestApply(ctx context.Context, id billing.PSPImportID) (*billing.PSPImport, error) {
	var out gen.BillingPspImport
	err := im.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := im.DB.NewWithPgxTx(tx).Gen(ctx)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		row, err := q.LockPSPImport(ctx, gen.LockPSPImportParams{MerchantID: mid.UUID(), ID: id.UUID()})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if busy(row.Status) || row.PulledAt == nil || row.HistoryThrough == nil || row.HistoryThrough.Before(*row.PulledAt) {
			return apperr.Conflictf("the import is %s; it lands once its pull finished", row.Status)
		}
		out, err = q.SetPSPImportStatus(ctx, gen.SetPSPImportStatusParams{MerchantID: row.MerchantID, ID: row.ID, Status: string(billing.PSPImportApplying)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return im.view(ctx, out)
}

// Fail records why an import's job gave up.
func (im *Importer) Fail(ctx context.Context, id billing.PSPImportID, cause error) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	msg := strings.TrimSpace(cause.Error())
	if msg == "" {
		msg = "failed"
	}
	_, err = im.DB.Gen(ctx).SetPSPImportStatus(ctx, gen.SetPSPImportStatusParams{MerchantID: mid.UUID(), ID: id.UUID(), Status: string(billing.PSPImportFailed), Error: &msg})
	return err
}

// Planned is the provider plans the import's linked subscriptions name and
// the product each grants ("" for none).
func (im *Importer) Planned(ctx context.Context, id billing.PSPImportID) (merchants.PSPScope, map[string]string, error) {
	row, err := im.row(ctx, id)
	if err != nil {
		return merchants.PSPScope{}, nil, err
	}
	scope, found, err := im.Merchants.PSPScopeByID(ctx, billing.MerchantID(row.MerchantID), row.PspID)
	if err != nil || !found {
		return scope, nil, errors.Join(err, errors.New("the import's PSP is gone"))
	}
	var products []billing.PSPImportProduct
	if err := json.Unmarshal(row.Products, &products); err != nil {
		return scope, nil, err
	}
	plans, err := im.DB.Gen(ctx).ListPSPImportPlans(ctx, gen.ListPSPImportPlansParams{MerchantID: row.MerchantID, ImportID: row.ID})
	if err != nil {
		return scope, nil, err
	}
	out := make(map[string]string, len(plans))
	for _, p := range plans {
		if p != nil {
			out[*p] = productFor(products, *p)
		}
	}
	return scope, out, nil
}

func (im *Importer) view(ctx context.Context, row gen.BillingPspImport) (*billing.PSPImport, error) {
	out := &billing.PSPImport{ID: billing.PSPImportID(row.ID), PSP: billing.PSPID(row.PspID), Rail: row.Rail, Status: billing.PSPImportStatus(row.Status),
		HistorySince: row.HistorySince, HistoryThrough: row.HistoryThrough, PulledAt: row.PulledAt, AppliedAt: row.AppliedAt, Error: row.Error,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Counts: []billing.PSPImportCount{}}
	if err := json.Unmarshal(row.Rules, &out.Rules); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(row.Products, &out.Products); err != nil {
		return nil, err
	}
	counts, err := im.DB.Gen(ctx).CountPSPImportRecords(ctx, gen.CountPSPImportRecordsParams{MerchantID: row.MerchantID, ImportID: row.ID})
	if err != nil {
		return nil, err
	}
	for _, c := range counts {
		out.Counts = append(out.Counts, billing.PSPImportCount{Kind: billing.PSPImportRecordKind(c.Kind), State: billing.PSPImportRecordState(c.State), Records: int(c.Records)})
	}
	return out, nil
}

func recordView(r gen.BillingPspImportRecord) billing.PSPImportRecord {
	out := billing.PSPImportRecord{ID: billing.PSPImportRecordID(r.ID), Import: billing.PSPImportID(r.ImportID), Kind: billing.PSPImportRecordKind(r.Kind), Ref: r.Ref,
		CustomerRef: r.CustomerRef, SubscriptionRef: r.SubscriptionRef, OccurredAt: r.OccurredAt, State: billing.PSPImportRecordState(r.State),
		LinkedVia: r.LinkedVia, Reason: r.Reason, UpdatedAt: r.UpdatedAt, Facts: []billing.PSPImportFact{}}
	if r.CustomerID != nil {
		c := billing.CustomerID(*r.CustomerID)
		out.Customer = &c
	}
	var f facts
	_ = json.Unmarshal(r.Facts, &f)
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, v := range f[name] {
			out.Facts = append(out.Facts, billing.PSPImportFact{Name: name, Value: v})
		}
	}
	return out
}
