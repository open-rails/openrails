// Package pspimport imports a merchant's subscribers straight from its PSPs:
// it observes the PSP's catalog, pulls each provider object read-only, stages
// it with the identity facts the provider holds, links it to a customer by
// the merchant's rules and mappings, and lands the linked ones through
// billingimport on the provider prices mapping their plans. Objects no rule
// links, and subscriptions whose plan no one mapped, stay staged and listed;
// nothing is guessed.
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
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/transient"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Importer runs PSP imports for the merchant in ctx.
type Importer struct {
	DB        *db.DB
	Merchants *merchants.Service
	// NMI builds the import's NMI clients; they are made read-only.
	NMI *railresolve.NMIFactory
	// Stripe builds the import's write-blocked Stripe clients.
	Stripe *stripeapi.Factory
	// Objects records the PSP's catalog the pull observes.
	Objects *providerobjects.Observer
	// CCBillDataLinkURL points DataLink at a loopback fake (sandbox runs).
	CCBillDataLinkURL string
	Directory         identity.Directory
	// Alerts tells the merchant's operator when a provider refuses a PSP's
	// credentials.
	Alerts reconcile.FindingNotifier
	Clock  clockwork.Clock
}

// ErrNotFound is an import or record the merchant does not have.
var ErrNotFound = apperr.New(404, billing.CodeResourceNotFound, "PSP import not found")

// Step is what an import's job does next.
type Step string

const (
	StepPull  Step = "pull"
	StepApply Step = "apply"
)

// JobKind is the River job that runs one step of an import.
const JobKind = "openrails.psp_import"

// JobAttempts bounds a pull's attempts, each resuming where the last stopped:
// NextRetry waits attempt⁴ seconds, at most an hour, so a provider that does
// not answer is retried for about 14 months.
const JobAttempts = 10_000

// ApplyAttempts bounds a landing's attempts; a person applies again.
const ApplyAttempts = 10

// RetryBudget is how many attempts a step makes at a failure that is not
// Transient (an answer it cannot read, a row the database refuses) before the
// import fails with it.
const RetryBudget = 5

// JobQueue is the River queue import steps run on, apart from renewals and
// dunning.
const JobQueue = "psp_import"

// Transient reports whether a later attempt of a step can outlive err: a
// provider, Vault or the database that did not answer, or a configuration
// that changed under the step.
func Transient(err error) bool {
	return transient.Is(err) || errors.Is(err, merchantdocs.ErrUnavailable) || errors.Is(err, errConfigChanged)
}

// RetryCap is the longest wait between two attempts of a step.
const RetryCap = time.Hour

// RetryDelay is how long a step waits after its attempt-th failed attempt:
// attempt⁴ seconds, at most RetryCap.
func RetryDelay(attempt int) time.Duration {
	if attempt > 7 { // 8⁴ s is past the hour
		return RetryCap
	}
	return min(time.Duration(attempt*attempt*attempt*attempt)*time.Second, RetryCap)
}

// JobArgs runs one step of an import: its pull or its landing. It is queued in
// the transaction that admits the step, so a committed step always has its job.
type JobArgs struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	ImportID   uuid.UUID `json:"import_id"`
	Step       Step      `json:"step"`
}

func (JobArgs) Kind() string { return JobKind }

func (a JobArgs) InsertOpts() river.InsertOpts {
	if a.Step == StepApply {
		return river.InsertOpts{Queue: JobQueue, MaxAttempts: ApplyAttempts}
	}
	return river.InsertOpts{Queue: JobQueue, MaxAttempts: JobAttempts}
}

func (im *Importer) now() time.Time { return timeutil.FirstClock(im.Clock).Now().UTC() }

// Create stages a new import of one PSP and queues its pull with it.
func (im *Importer) Create(ctx context.Context, p billing.CreatePSPImportParams) (*billing.PSPImport, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := compileRules(p.Rules); err != nil {
		return nil, err
	}
	scope, err := im.psp(ctx, mid, p.PSP)
	if err != nil {
		return nil, err
	}
	if _, ok := sources[scope.Rail]; !ok {
		return nil, apperr.Invalidf("PSP %q is on %s, which has no import yet", scope.Key, scope.Rail).WithParam("psp")
	}
	rules, err := encodeRules(p.Rules)
	if err != nil {
		return nil, err
	}
	var since *time.Time
	if p.HistorySince != nil {
		t := p.HistorySince.UTC()
		since = &t
	}
	var row gen.BillingPspImport
	err = im.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := im.DB.NewWithPgxTx(tx)
		var err error
		if row, err = d.Gen(ctx).CreatePSPImport(ctx, gen.CreatePSPImportParams{MerchantID: mid.UUID(), PspID: scope.ID, Rail: scope.Rail,
			HistorySince: since, Rules: rules}); err != nil {
			return err
		}
		_, err = d.InsertRiverJob(ctx, JobArgs{MerchantID: mid.UUID(), ImportID: row.ID, Step: StepPull}, nil)
		return err
	})
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

func encodeRules(rules []billing.PSPImportRule) ([]byte, error) {
	if rules == nil {
		rules = DefaultRules
	}
	return json.Marshal(rules)
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
	params := gen.ListPSPImportsPageParams{MerchantID: mid.UUID(), AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit)}
	if !p.PSP.IsZero() {
		scope, err := im.psp(ctx, mid, p.PSP)
		if err != nil {
			return out, err
		}
		params.PspID = &scope.ID
	}
	switch p.Origin {
	case "":
	case billing.PSPImportOnboarding, billing.PSPImportManual:
		params.Origin = new(string(p.Origin))
	default:
		return out, apperr.Invalidf("origin is onboarding or manual").WithParam("origin")
	}
	rows, err := im.DB.Gen(ctx).ListPSPImportsPage(ctx, params)
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

// Update replaces the import's rules.
func (im *Importer) Update(ctx context.Context, id billing.PSPImportID, p billing.UpdatePSPImportParams) (*billing.PSPImport, error) {
	row, err := im.row(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Rules == nil {
		return im.view(ctx, row)
	}
	if _, err := compileRules(*p.Rules); err != nil {
		return nil, err
	}
	r, err := encodeRules(*p.Rules)
	if err != nil {
		return nil, err
	}
	row, err = im.DB.Gen(ctx).UpdatePSPImportRules(ctx, gen.UpdatePSPImportRulesParams{MerchantID: row.MerchantID, ID: row.ID, Rules: r})
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
		out.Items, err = im.recordViews(ctx, rows)
		return out, err
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
	items, err := im.recordViews(ctx, rows)
	if err != nil {
		return out, err
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

// RequestApply checks an import can land, marks it applying and queues its
// landing in the same transaction.
func (im *Importer) RequestApply(ctx context.Context, id billing.PSPImportID) (*billing.PSPImport, error) {
	var out gen.BillingPspImport
	err := im.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := im.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
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
		if out, err = q.SetPSPImportStatus(ctx, gen.SetPSPImportStatusParams{MerchantID: row.MerchantID, ID: row.ID, Status: string(billing.PSPImportApplying)}); err != nil {
			return err
		}
		_, err = d.InsertRiverJob(ctx, JobArgs{MerchantID: row.MerchantID, ImportID: row.ID, Step: StepApply}, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return im.view(ctx, out)
}

// Fail records why an import's job gave up, with the PermanentError's code
// when cause has one. A provider refusing the credentials also opens the
// PSP's credentials finding and alerts the merchant's operator.
func (im *Importer) Fail(ctx context.Context, id billing.PSPImportID, cause error) error {
	row, err := im.row(ctx, id)
	if err != nil {
		return err
	}
	msg := strings.TrimSpace(cause.Error())
	if msg == "" {
		msg = "failed"
	}
	params := gen.SetPSPImportStatusParams{MerchantID: row.MerchantID, ID: row.ID, Status: string(billing.PSPImportFailed), Error: &msg}
	var permanent PermanentError
	if errors.As(cause, &permanent) && permanent.Code != "" {
		params.ErrorCode = new(string(permanent.Code))
	}
	pspKey := billing.PSPID(row.PspID).String()
	if scope, found, err := im.Merchants.PSPScopeByID(ctx, billing.MerchantID(row.MerchantID), row.PspID); err == nil && found {
		pspKey = scope.Key
	}
	var opened *gen.BillingReconciliationFinding
	err = im.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := im.DB.NewWithPgxTx(tx).Gen(ctx)
		if _, err := q.SetPSPImportStatus(ctx, params); err != nil {
			return err
		}
		if permanent.Code != billing.PSPImportCredentialsRejected {
			return nil
		}
		f, err := openCredentialsFinding(ctx, q, row, pspKey, msg)
		opened = &f
		return err
	})
	if err != nil || opened == nil || im.Alerts == nil {
		return err
	}
	if err := im.Alerts.NotifyFinding(ctx, reconcile.FindingRecordFromRow(*opened)); err != nil && ctx.Err() == nil {
		log.WithContext(ctx).WithError(err).WithField("finding_id", opened.ID.String()).Warn("psp import: alerting the operator failed; the finding stays in the console")
	}
	return nil
}

func (im *Importer) view(ctx context.Context, row gen.BillingPspImport) (*billing.PSPImport, error) {
	out := &billing.PSPImport{ID: billing.PSPImportID(row.ID), PSP: billing.PSPID(row.PspID), Rail: row.Rail, Origin: billing.PSPImportOrigin(row.Origin),
		Status: billing.PSPImportStatus(row.Status), HistorySince: row.HistorySince, HistoryThrough: row.HistoryThrough, PulledAt: row.PulledAt, AppliedAt: row.AppliedAt,
		Error: row.Error, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Counts: []billing.PSPImportCount{}}
	if row.ErrorCode != nil {
		out.ErrorCode = new(billing.PSPImportErrorCode(*row.ErrorCode))
	}
	if err := json.Unmarshal(row.Rules, &out.Rules); err != nil {
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

// recordViews are records as the API shows them, manual prices by their ids.
func (im *Importer) recordViews(ctx context.Context, rows []gen.BillingPspImportRecord) ([]billing.PSPImportRecord, error) {
	var priced []uuid.UUID
	for _, r := range rows {
		if r.ManualPriceID != nil {
			priced = append(priced, *r.ManualPriceID)
		}
	}
	refs, err := db.PriceReferences(ctx, im.DB.Gen(ctx), priced)
	if err != nil {
		return nil, err
	}
	out := make([]billing.PSPImportRecord, 0, len(rows))
	for _, r := range rows {
		v := recordView(r)
		if r.ManualPriceID != nil {
			if ref, ok := refs[*r.ManualPriceID]; ok {
				v.ManualPrice = &ref
			}
		}
		out = append(out, v)
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
