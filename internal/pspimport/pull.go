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
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/progress"
)

// sources are the rails an import reads, each building its read-only source.
var sources = map[string]func(ctx context.Context, im *Importer, mid billing.MerchantID, scope merchants.PSPScope) (source, error){
	"nmi": func(ctx context.Context, im *Importer, mid billing.MerchantID, scope merchants.PSPScope) (source, error) {
		if im.NMI == nil || im.Merchants == nil {
			return nil, errors.New("nmi clients are not wired")
		}
		client, err := im.NMI.Client(ctx, im.Merchants.Secrets(), mid, scope)
		if err != nil {
			return nil, err
		}
		client.ReadOnly = true
		return &nmiSource{client: client, currency: "USD"}, nil
	},
	"stripe": func(ctx context.Context, im *Importer, mid billing.MerchantID, scope merchants.PSPScope) (source, error) {
		key, err := im.secret(ctx, mid, scope, "secret_key")
		if err != nil {
			return nil, err
		}
		fetcher := reconcile.NewStripeFetcher(key)
		fetcher.HTTPClient = im.Stripe.ReadOnlyClient(30 * time.Second)
		return &stripeSource{fetcher: fetcher}, nil
	},
	"ccbill": func(ctx context.Context, im *Importer, mid billing.MerchantID, scope merchants.PSPScope) (source, error) {
		acc, sub, ok := strings.Cut(strings.TrimSpace(scope.AccountID), "-")
		if !ok || acc == "" || sub == "" {
			return nil, fmt.Errorf("CCBill account %q is not clientAccnum-clientSubacc", scope.AccountID)
		}
		username, err := im.secret(ctx, mid, scope, "datalink_username")
		if err != nil {
			return nil, err
		}
		password, err := im.secret(ctx, mid, scope, "datalink_password")
		if err != nil {
			return nil, err
		}
		dl := ccbill.NewDataLinkClient(&config.CCBillConfig{ClientAccNum: acc, ClientSubAcc: sub, DataLinkUsername: username, DataLinkPassword: password,
			Sandbox: scope.Environment == config.EnvironmentSandbox})
		dl.ReadOnly = true
		if im.CCBillDataLinkURL != "" {
			dl.BaseURL, dl.LoopbackFixture = im.CCBillDataLinkURL, true
		}
		return &ccbillSource{dl: dl}, nil
	},
}

// secret reads one of the PSP's credentials.
func (im *Importer) secret(ctx context.Context, mid billing.MerchantID, scope merchants.PSPScope, name string) (string, error) {
	if im.Merchants == nil {
		return "", errors.New("merchant configuration is not wired")
	}
	ref, err := scope.SecretRef(name)
	if err != nil {
		return "", err
	}
	sec, err := merchants.ReadSecretRef(ctx, im.Merchants.Secrets(), mid, ref)
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	if strings.TrimSpace(sec.Value) == "" {
		return "", fmt.Errorf("secret %s is empty", name)
	}
	return strings.TrimSpace(sec.Value), nil
}

// historyWindow bounds one transaction read and its commit.
const historyWindow = 30 * 24 * time.Hour

// stageBatch bounds one staging write.
const stageBatch = 500

// Pull reads the import's PSP: its roster once, then its transaction history
// window by window, each committed with how far it reached, so a retried job
// resumes. It then links what it staged.
func (im *Importer) Pull(ctx context.Context, id billing.PSPImportID) error {
	row, err := im.row(ctx, id)
	if err != nil || row.Status != string(billing.PSPImportPulling) {
		return err
	}
	mid := billing.MerchantID(row.MerchantID)
	scope, found, err := im.Merchants.PSPScopeByID(ctx, mid, row.PspID)
	if err != nil {
		return err
	}
	if !found {
		return Permanent(errors.New("the import's PSP is no longer declared"))
	}
	build, ok := sources[scope.Rail]
	if !ok {
		return Permanent(fmt.Errorf("%s has no import", scope.Rail))
	}
	src, err := build(ctx, im, mid, scope)
	if err != nil {
		return Permanent(fmt.Errorf("PSP %s is not readable: %w", scope.Key, err))
	}
	q := im.DB.Gen(ctx)
	if row.PulledAt == nil {
		observed := im.now()
		recs, err := src.roster(ctx)
		if err != nil {
			return err
		}
		if err := im.stage(ctx, row, recs); err != nil {
			return err
		}
		since := row.HistorySince
		if since == nil {
			since = &observed
			for _, r := range recs {
				if r.kind == billing.PSPImportSubscription && !r.occurredAt.IsZero() && r.occurredAt.Before(*since) {
					t := r.occurredAt
					since = &t
				}
			}
		}
		if err := q.SetPSPImportRoster(ctx, gen.SetPSPImportRosterParams{MerchantID: row.MerchantID, ID: row.ID, PulledAt: observed, HistorySince: since}); err != nil {
			return err
		}
		row.PulledAt, row.HistoryThrough = &observed, since
	}
	attrib, err := im.attribution(ctx, row)
	if err != nil {
		return err
	}
	for through := *row.HistoryThrough; through.Before(*row.PulledAt); {
		until := through.Add(historyWindow)
		if until.After(*row.PulledAt) {
			until = *row.PulledAt
		}
		progress.Mark(ctx, fmt.Sprintf("psp import %s history %s", id, through.Format(time.DateOnly)))
		recs, err := src.history(ctx, through, until)
		if err != nil {
			return err
		}
		for i := range recs {
			attrib.place(&recs[i])
		}
		if err := im.stage(ctx, row, recs); err != nil {
			return err
		}
		if err := q.SetPSPImportHistoryThrough(ctx, gen.SetPSPImportHistoryThroughParams{MerchantID: row.MerchantID, ID: row.ID, HistoryThrough: until}); err != nil {
			return err
		}
		through = until
	}
	if err := im.link(ctx, row, nil); err != nil {
		return err
	}
	_, err = q.SetPSPImportStatus(ctx, gen.SetPSPImportStatusParams{MerchantID: row.MerchantID, ID: row.ID, Status: string(billing.PSPImportPulled)})
	return err
}

// attribution indexes the import's staged subscriptions.
func (im *Importer) attribution(ctx context.Context, row gen.BillingPspImport) (*attribution, error) {
	out := newAttribution()
	err := im.eachRecord(ctx, row, billing.PSPImportSubscription, func(r gen.BillingPspImportRecord) error {
		var f facts
		if err := json.Unmarshal(r.Facts, &f); err != nil {
			return err
		}
		var started time.Time
		if r.OccurredAt != nil {
			started = *r.OccurredAt
		}
		out.addSubscription(r.Ref, deref(r.CustomerRef), f, started)
		return nil
	})
	return out, err
}

// eachRecord walks one kind of the import's records in id order.
func (im *Importer) eachRecord(ctx context.Context, row gen.BillingPspImport, kind billing.PSPImportRecordKind, fn func(gen.BillingPspImportRecord) error) error {
	after := uuid.Nil
	for {
		page, err := im.DB.Gen(ctx).ListPSPImportRecordsByKind(ctx, gen.ListPSPImportRecordsByKindParams{MerchantID: row.MerchantID, ImportID: row.ID, Kind: string(kind), AfterID: after, RowLimit: stageBatch})
		if err != nil {
			return err
		}
		for _, r := range page {
			if err := fn(r); err != nil {
				return err
			}
		}
		if len(page) < stageBatch {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

// stage upserts what a source read.
func (im *Importer) stage(ctx context.Context, row gen.BillingPspImport, recs []staged) error {
	for start := 0; start < len(recs); start += stageBatch {
		chunk := recs[start:min(start+stageBatch, len(recs))]
		p := gen.UpsertPSPImportRecordsParams{MerchantID: row.MerchantID, ImportID: row.ID}
		seen := map[string]bool{}
		for _, r := range chunk {
			key := string(r.kind) + "\x1f" + r.ref
			if r.ref == "" || seen[key] {
				continue
			}
			seen[key] = true
			f, err := json.Marshal(r.facts)
			if err != nil {
				return err
			}
			d, err := json.Marshal(r.data)
			if err != nil {
				return err
			}
			p.Kinds, p.Refs = append(p.Kinds, string(r.kind)), append(p.Refs, r.ref)
			p.CustomerRefs, p.SubscriptionRefs = append(p.CustomerRefs, r.customerRef), append(p.SubscriptionRefs, r.subscriptionRef)
			p.OccurredAts, p.Facts, p.Data = append(p.OccurredAts, r.occurredAt.UTC()), append(p.Facts, f), append(p.Data, d)
		}
		if len(p.Refs) == 0 {
			continue
		}
		if err := im.DB.Gen(ctx).UpsertPSPImportRecords(ctx, p); err != nil {
			return fmt.Errorf("stage records: %w", err)
		}
	}
	return nil
}

// PermanentError is a job failure retrying cannot fix.
type PermanentError struct{ Err error }

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as one a retry cannot fix.
func Permanent(err error) error { return PermanentError{Err: err} }

// Run runs one step of the import in ctx's merchant scope.
func (im *Importer) Run(ctx context.Context, mid billing.MerchantID, id billing.PSPImportID, step Step) error {
	return im.DB.RunInMerchantScope(ctx, mid, "psp import "+string(step), func(ctx context.Context) error {
		if _, err := merchant.Require(ctx); err != nil {
			return err
		}
		switch step {
		case StepPull:
			return im.Pull(ctx, id)
		case StepApply:
			return im.Apply(ctx, id)
		}
		return Permanent(fmt.Errorf("unknown step %q", step))
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
