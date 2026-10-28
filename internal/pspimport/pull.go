package pspimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/railresolve"
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
		return &nmiSource{client: client, currency: "USD", now: im.now}, nil
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
			return nil, notReadable(fmt.Errorf("CCBill account %q is not clientAccnum-clientSubacc", scope.AccountID))
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

// secret reads one of the PSP's credentials. One the configuration does not
// declare, or declares empty, is merchants.ErrSecretNotFound.
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
		return "", fmt.Errorf("secret %s is empty: %w", name, merchants.ErrSecretNotFound)
	}
	return strings.TrimSpace(sec.Value), nil
}

// clientError classifies a failure to build a PSP's read client: a credential
// the configuration does not hold cannot be read by retrying; anything else
// (the secret store unreachable) can.
func clientError(scope merchants.PSPScope, err error) error {
	var permanent PermanentError
	switch {
	case errors.As(err, &permanent):
		return err
	case errors.Is(err, merchants.ErrSecretNotFound) || errors.Is(err, railresolve.ErrRailNotArmed):
		return notReadable(fmt.Errorf("PSP %s is not readable: %w", scope.Key, err))
	}
	return fmt.Errorf("PSP %s credentials: %w", scope.Key, err)
}

// historyWindow bounds one transaction read and its commit.
const historyWindow = 30 * 24 * time.Hour

// stageBatch bounds one staging write.
const stageBatch = 500

// Pull reads the import's PSP: what OpenRails holds of it (the basis), its
// catalog and roster once, then its transaction history window by window,
// each committed with how far it reached, so a retried job resumes. It then
// links what it staged, marks what OpenRails holds held and proposes every
// difference in that; it lands nothing. A relink reads nothing: it links and
// proposes from what the pull read. A provider refusing the credentials is
// permanent, ErrCredentialsRejected; so is a PSP or credential the
// configuration no longer declares.
func (im *Importer) Pull(ctx context.Context, id billing.PSPImportID, relink bool) error {
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
		return notReadable(errors.New("the import's PSP is no longer declared"))
	}
	err = im.pull(ctx, row, scope, relink)
	if err == nil || !rejected(err) {
		return err
	}
	// Credentials replaced while this pull read the old ones: the change's
	// re-arm found the import still pulling, so retry with the new ones.
	if now, found, rerr := im.Merchants.PSPScopeByID(ctx, mid, row.PspID); rerr == nil && found && now.Revision != scope.Revision {
		return fmt.Errorf("%w: %w", errConfigChanged, err)
	}
	return PermanentError{Err: fmt.Errorf("%w: %w", ErrCredentialsRejected, err), Code: billing.PSPImportCredentialsRejected}
}

// errConfigChanged is a pull whose PSP's configuration changed under it.
var errConfigChanged = errors.New("the PSP's configuration changed during the pull")

func (im *Importer) pull(ctx context.Context, row gen.BillingPspImport, scope merchants.PSPScope, relink bool) error {
	id := billing.PSPImportID(row.ID)
	mid := billing.MerchantID(row.MerchantID)
	var src source
	// A read that finished (a relink) reads the provider no more.
	if row.PulledAt == nil || row.HistoryThrough == nil || row.HistoryThrough.Before(*row.PulledAt) {
		build, ok := sources[scope.Rail]
		if !ok {
			return notReadable(fmt.Errorf("%s has no import", scope.Rail))
		}
		var err error
		if src, err = build(ctx, im, mid, scope); err != nil {
			return clientError(scope, err)
		}
	}
	// The first answer the provider gives closes the PSP's credentials
	// finding.
	answered := false
	read := func() error {
		if answered {
			return nil
		}
		answered = true
		return im.resolveCredentialsFinding(ctx, row)
	}
	q := im.DB.Gen(ctx)
	if row.PulledAt == nil {
		// What OpenRails holds before the roster read is the basis of every
		// state change the pull proposes.
		if err := q.RecordPSPImportBasis(ctx, gen.RecordPSPImportBasisParams{MerchantID: row.MerchantID, ImportID: row.ID, PspID: row.PspID}); err != nil {
			return err
		}
		observed := im.now()
		cat, err := im.listCatalog(ctx, scope)
		if err != nil {
			return err
		}
		recs, err := src.roster(ctx)
		if err != nil {
			return err
		}
		if err := read(); err != nil {
			return err
		}
		// A roster the breaker refuses records nothing, the catalog's
		// absences included: wrong credentials list another account.
		plausible, err := im.plausibleRoster(ctx, row, scope, recs)
		if err != nil {
			return err
		}
		if err := cat.record(ctx, plausible); err != nil {
			return err
		}
		if err := im.stage(ctx, row, recs); err != nil {
			return err
		}
		if err := cat.fill(ctx, recs); err != nil {
			return err
		}
		since := row.HistorySince
		if since == nil {
			since = &observed
			for _, r := range recs {
				if !r.occurredAt.IsZero() && r.occurredAt.Before(*since) {
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
		if err := read(); err != nil {
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
	newly, err := im.link(ctx, row)
	if err != nil {
		return err
	}
	// A host book's row first held at a relink is diffed at its current
	// version; the pull's basis stands for the rest.
	if relink {
		if err := im.basisAdded(ctx, row, newly); err != nil {
			return err
		}
	}
	// Only a read of the roster resolves what it no longer raises.
	if err := im.propose(ctx, row, scope, nil, !relink); err != nil {
		return err
	}
	_, err = q.SetPSPImportStatus(ctx, gen.SetPSPImportStatusParams{MerchantID: row.MerchantID, ID: row.ID, Status: string(billing.PSPImportPulled)})
	return err
}

// catalogPull is a pull's read of the PSP's catalog: what its lists held,
// and what they no longer hold.
type catalogPull struct {
	im             *Importer
	scope          merchants.PSPScope
	reader         providerobjects.Reader
	at             time.Time
	objects        []providerobjects.Object
	listed, absent map[string]bool
}

func catalogKey(kind providerobjects.Kind, ref string) string { return providerobjects.Key(kind, ref) }

// listCatalog lists the PSP's whole catalog (Stripe products and prices,
// active and not; NMI plans); record records it. A rail without a catalog
// (CCBill) gives nil.
func (im *Importer) listCatalog(ctx context.Context, scope merchants.PSPScope) (*catalogPull, error) {
	if im.Objects == nil {
		return nil, Permanent(errors.New("the provider catalog observer is not wired"))
	}
	reader, err := im.Objects.Reader(ctx, scope)
	if errors.Is(err, providerobjects.ErrNoCatalog) {
		return nil, nil
	}
	if err != nil {
		return nil, clientError(scope, err)
	}
	at := im.Objects.Now()
	objects, err := reader.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("provider catalog: %w", err)
	}
	c := &catalogPull{im: im, scope: scope, reader: reader, at: at, objects: objects, listed: map[string]bool{}, absent: map[string]bool{}}
	for _, obj := range objects {
		c.listed[catalogKey(obj.Kind, obj.Ref)] = true
	}
	return c, nil
}

// record records what the list read. With absences, a complete list also
// marks gone what an earlier list held and this one lacks.
func (c *catalogPull) record(ctx context.Context, absences bool) error {
	if c == nil {
		return nil
	}
	if !absences {
		return c.im.Objects.Record(ctx, c.scope, c.at, c.objects, providerobjects.ViaPull)
	}
	absent, err := c.im.Objects.RecordList(ctx, c.scope, c.at, c.objects)
	if err != nil {
		return err
	}
	c.absent = absent
	return nil
}

// fill reads by id what the lists lacked: each price a staged subscription
// names (a Stripe inline price; an NMI plan gone from the gateway, recorded
// from the subscription's copy), each object a provider price of the PSP
// binds (recorded from its stored terms when gone and never observed) and
// each stored object of the PSP. One object that cannot be read is logged
// and left for the next read; it does not fail the pull.
func (c *catalogPull) fill(ctx context.Context, recs []staged) error {
	if c == nil {
		return nil
	}
	read := map[string]bool{}
	observe := func(kind providerobjects.Kind, ref string, stated *providerobjects.Object) error {
		key := catalogKey(kind, ref)
		if ref == "" || c.listed[key] || c.absent[key] || read[key] {
			return nil
		}
		read[key] = true
		progress.Mark(ctx, fmt.Sprintf("psp import catalog %s %s", kind, ref))
		if _, err := c.im.Objects.Observe(ctx, c.reader, c.scope, kind, ref, stated, providerobjects.ViaRead); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{"psp": c.scope.Key, "kind": kind, "ref": ref}).Warn("psp import: provider catalog object not read; left for the next read")
		}
		return nil
	}
	for _, r := range recs {
		data, ok := r.data.(subscriptionData)
		if r.kind != billing.PSPImportSubscription || !ok {
			continue
		}
		if err := observe(providerobjects.Price, data.Plan, r.stated); err != nil {
			return err
		}
	}
	bound, err := c.im.Objects.Bound(ctx, c.scope)
	if err != nil {
		return err
	}
	for _, stated := range bound {
		if err := observe(providerobjects.Price, stated.Ref, stated); err != nil {
			return err
		}
	}
	stored, err := c.im.Objects.Refs(ctx, c.scope)
	if err != nil {
		return err
	}
	for _, kind := range []providerobjects.Kind{providerobjects.Product, providerobjects.Price} {
		for _, ref := range stored[kind] {
			if err := observe(kind, ref, nil); err != nil {
				return err
			}
		}
	}
	return nil
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

// PermanentError is a job failure retrying cannot fix. Code names one only a
// change to the PSP's configuration fixes.
type PermanentError struct {
	Err  error
	Code billing.PSPImportErrorCode
}

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as one a retry cannot fix.
func Permanent(err error) error { return PermanentError{Err: err} }

// notReadable is a PSP the configuration gives no way to read: gone, on a
// rail without an import, or missing a credential.
func notReadable(err error) error {
	return PermanentError{Err: err, Code: billing.PSPImportNotReadable}
}

// Run runs one step of the import in ctx's merchant scope.
func (im *Importer) Run(ctx context.Context, mid billing.MerchantID, id billing.PSPImportID, step Step) error {
	return im.DB.RunInMerchantScope(ctx, mid, "psp import "+string(step), func(ctx context.Context) error {
		if _, err := merchant.Require(ctx); err != nil {
			return err
		}
		switch step {
		case StepPull:
			return im.Pull(ctx, id, false)
		case StepRelink:
			return im.Pull(ctx, id, true)
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
