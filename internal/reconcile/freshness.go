package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/alerting/alertjob"
)

// Known-through per PSP: the instant through which OpenRails' record of a
// PSP is complete. A PSP whose reads succeed is current (complete through
// now). A failed read opens a stale episode: the overdue detector and the
// passes that infer from absence skip the PSP's subscriptions, which keep
// their last known state, until a catch-up reads its gap. The evidence is
// psp_freshness, webhook_health and the imports; nothing is configured.

// PSPFailure is why a PSP's reads fail.
type PSPFailure string

const (
	// PSPNotConfigured: no credentials resolve to read with.
	PSPNotConfigured PSPFailure = "not_configured"
	// PSPCredentialsRefused: the provider refused the credentials.
	PSPCredentialsRefused PSPFailure = "credentials_refused"
	// PSPUnreachable: the provider did not answer, or answered an error.
	PSPUnreachable PSPFailure = "unreachable"
)

// ErrPSPNotArmed is a read of a PSP whose credentials do not resolve.
var ErrPSPNotArmed = errors.New("PSP is not armed")

// ReadFailureOf classifies a failed provider read.
func ReadFailureOf(err error) PSPFailure {
	switch {
	case errors.Is(err, ErrPSPNotArmed):
		return PSPNotConfigured
	case errors.Is(err, nmi.ErrCredentialsRejected), errors.Is(err, nmi.ErrEnvironmentMismatch),
		errors.Is(err, ccbill.ErrDataLinkAuth), errors.Is(err, stripeapi.ErrKeyRefused):
		return PSPCredentialsRefused
	default:
		return PSPUnreachable
	}
}

// RenewalDeadline is how long past its period end a renewal of rail may go
// unseen before it is overdue: NMIRebillDeadline for NMI, PeriodGrace for
// the providers that retry on their own.
func RenewalDeadline(rail string) time.Duration {
	if rail == string(ProviderNMI) {
		return NMIRebillDeadline
	}
	return PeriodGrace
}

// KindPSPCatchUp is the job that catches a stale PSP up.
const KindPSPCatchUp = "openrails.psp_catch_up"

// CatchUpArgs asks whether a PSP can be read and, if it is stale and can,
// reads its gap. Queued by an import, by a read that succeeds while the PSP
// is stale, and every hour for each stale PSP.
type CatchUpArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	PSPID      uuid.UUID `json:"psp_id" river:"unique"`
}

func (CatchUpArgs) Kind() string { return KindPSPCatchUp }

// InsertOpts coalesces a PSP's catch-ups while one is outstanding, on the
// billing queue.
func (CatchUpArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
		rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

// RecordPSPRead records one provider read of psp at `at`; readErr nil is a
// success. A failure opens a stale episode; a success clears the failure
// and, while stale, queues the catch-up. Merchant-scoped.
func RecordPSPRead(ctx context.Context, database *db.DB, mid, psp uuid.UUID, at time.Time, readErr error) error {
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := database.NewWithPgxTx(tx)
		q := txdb.Gen(ctx)
		if readErr != nil {
			if _, err := q.RecordPSPReadFailed(ctx, gen.RecordPSPReadFailedParams{MerchantID: mid, PspID: psp, FailedAt: at.UTC(), Failure: string(ReadFailureOf(readErr))}); err != nil {
				return err
			}
			return notifyPSPStale(ctx, txdb, mid, psp, at)
		}
		stale, err := q.RecordPSPReadSucceeded(ctx, gen.RecordPSPReadSucceededParams{MerchantID: mid, PspID: psp, ReadAt: at.UTC()})
		if err != nil || stale == nil {
			return err
		}
		_, err = txdb.InsertRiverJob(ctx, CatchUpArgs{MerchantID: mid, PSPID: psp}, nil)
		return err
	})
}

// recordPSPReads records a read of each PSP the subscriptions belong to; a
// failure to record is logged, never the read's.
func recordPSPReads(ctx context.Context, database *db.DB, mid uuid.UUID, psp uuid.UUID, at time.Time, readErr error) {
	if err := RecordPSPRead(ctx, database, mid, psp, at, readErr); err != nil {
		database.Log.WarnContext(ctx, "known-through: could not record the PSP read", logx.Err(err), "psp_id", psp)
	}
}

// RecordPSPImported records that a declared book landed psp's data as of
// asOf, in the book's transaction, and queues a look at whether the PSP can
// be read.
func RecordPSPImported(ctx context.Context, txdb *db.DB, mid, psp uuid.UUID, asOf time.Time) error {
	if err := txdb.Gen(ctx).RecordPSPImported(ctx, gen.RecordPSPImportedParams{MerchantID: mid, PspID: psp, ImportedThrough: asOf.UTC()}); err != nil {
		return err
	}
	_, err := txdb.InsertRiverJob(ctx, CatchUpArgs{MerchantID: mid, PSPID: psp}, nil)
	return err
}

// PSPFreshness reads the known-through of the merchant's PSPs at now (one,
// when psp is set), each keyed by its id, with what awaits each.
func PSPFreshness(ctx context.Context, q *gen.Queries, mid uuid.UUID, psp *uuid.UUID, now time.Time) (map[uuid.UUID]billing.PSPFreshness, []gen.ListPSPFreshnessRow, error) {
	rows, err := q.ListPSPFreshness(ctx, gen.ListPSPFreshnessParams{MerchantID: mid, PspID: psp})
	if err != nil {
		return nil, nil, fmt.Errorf("known-through: %w", err)
	}
	awaiting, err := q.ListPSPAwaiting(ctx, gen.ListPSPAwaitingParams{MerchantID: mid, Now: now})
	if err != nil {
		return nil, nil, fmt.Errorf("known-through: awaiting: %w", err)
	}
	out := make(map[uuid.UUID]billing.PSPFreshness, len(rows))
	for _, row := range rows {
		f := PSPFreshnessOf(row, now)
		f.Awaiting = []billing.PSPAwaiting{}
		for _, a := range awaiting {
			if a.PspID == row.PspID {
				f.Awaiting = append(f.Awaiting, billing.PSPAwaiting{Currency: catalog.Currency(a.Currency), Subscriptions: a.Subscriptions, MRR: a.Mrr})
			}
		}
		out[row.PspID] = f
	}
	return out, rows, nil
}

// KnownThroughs is every PSP's known-through at now, in the merchant's PSP
// order, superseded PSPs without data aside; with the earliest known-through
// of a stale PSP with data (nil when each is current) and, when every PSP
// with data is stale, the latest of theirs.
func KnownThroughs(ctx context.Context, q *gen.Queries, mid uuid.UUID, now time.Time) (list []billing.PSPKnownThrough, complete, through *time.Time, err error) {
	fresh, rows, err := PSPFreshness(ctx, q, mid, nil, now)
	if err != nil {
		return nil, nil, nil, err
	}
	list = []billing.PSPKnownThrough{}
	current := false
	for _, row := range rows {
		f := fresh[row.PspID]
		if row.SupersededAt != nil && !f.HasData {
			continue
		}
		list = append(list, billing.PSPKnownThrough{PSPID: billing.PSPID(row.PspID), Key: row.Key, Rail: billing.Rail(row.Rail), PSPFreshness: f})
		if !f.HasData {
			continue
		}
		if f.Current {
			current = true
			continue
		}
		at := time.Time{}
		if f.KnownThrough != nil {
			at = *f.KnownThrough
		}
		if complete == nil || at.Before(*complete) {
			complete = &at
		}
		if through == nil || at.After(*through) {
			through = &at
		}
	}
	if current {
		through = nil
	}
	return list, complete, through, nil
}

// PSPFreshnessOf is a PSP's known-through at now from its evidence.
func PSPFreshnessOf(row gen.ListPSPFreshnessRow, now time.Time) billing.PSPFreshness {
	f := billing.PSPFreshness{LastReadAt: row.ReadAt, FailingSince: row.FailingSince, StaleSince: row.StaleSince, HasData: row.HasData, Awaiting: []billing.PSPAwaiting{}}
	if len(row.CatchUp) > 0 {
		var c billing.PSPCatchUp
		if json.Unmarshal(row.CatchUp, &c) == nil {
			f.CatchUp = &c
		}
	}
	if row.StaleSince == nil {
		at := now.UTC()
		f.KnownThrough, f.Source, f.Current = &at, billing.KnownThroughNow, true
		if row.WebhooksSilent {
			f.Cause, f.Severity = billing.PSPWebhooksSilent, billing.PSPSeverityWarning
		}
		return f
	}
	f.KnownThrough, f.Source = latestEvidence(row)
	f.Cause = staleCause(row)
	f.Severity = staleSeverity(row, f.Cause, now)
	return f
}

// latestEvidence is the latest instant something told us of the PSP.
func latestEvidence(row gen.ListPSPFreshnessRow) (*time.Time, billing.PSPKnownThroughSource) {
	var at *time.Time
	source := billing.KnownThroughNone
	for _, e := range []struct {
		at     *time.Time
		source billing.PSPKnownThroughSource
	}{{row.ImportedThrough, billing.KnownThroughImport}, {row.LastAcceptedAt, billing.KnownThroughWebhook},
		{row.LastPullAt, billing.KnownThroughRead}, {row.ReadAt, billing.KnownThroughRead}} {
		if e.at != nil && (at == nil || e.at.After(*at)) {
			v := e.at.UTC()
			at, source = &v, e.source
		}
	}
	return at, source
}

// staleCause is why a stale PSP is, from its evidence.
func staleCause(row gen.ListPSPFreshnessRow) billing.PSPStaleCause {
	failure := ""
	if row.Failure != nil {
		failure = *row.Failure
	}
	switch PSPFailure(failure) {
	case PSPCredentialsRefused:
		return billing.PSPCredentialsRefused
	case PSPUnreachable:
		return billing.PSPUnreachable
	case PSPNotConfigured:
		if row.ReadAt == nil && row.LastPullAt == nil && row.ImportedThrough != nil {
			return billing.PSPImported
		}
		return billing.PSPNotConfigured
	}
	return billing.PSPCatchingUp
}

// staleSeverity follows impact: critical when the provider refuses the
// credentials; a warning once a PSP with live subscriptions has been
// unreadable past its rail's renewal deadline; information otherwise (an
// import being onboarded, a gap inside the deadline, a catch-up).
func staleSeverity(row gen.ListPSPFreshnessRow, cause billing.PSPStaleCause, now time.Time) billing.PSPStaleSeverity {
	switch cause {
	case billing.PSPCredentialsRefused:
		return billing.PSPSeverityCritical
	case billing.PSPNotConfigured, billing.PSPUnreachable:
		if row.LiveSubscriptions && row.StaleSince != nil && now.Sub(*row.StaleSince) >= RenewalDeadline(row.Rail) {
			return billing.PSPSeverityWarning
		}
	}
	return billing.PSPSeverityInfo
}

// notifyPSPStale queues the stale episode's psp.stale event and alert once,
// when its severity first reaches warning or critical. In the caller's
// transaction.
func notifyPSPStale(ctx context.Context, txdb *db.DB, mid, psp uuid.UUID, now time.Time) error {
	q := txdb.Gen(ctx)
	_, rows, err := PSPFreshness(ctx, q, mid, &psp, now)
	if err != nil || len(rows) != 1 {
		return err
	}
	row := rows[0]
	f := PSPFreshnessOf(row, now)
	if row.StaleSince == nil || row.NotifiedAt != nil || f.Severity == billing.PSPSeverityInfo {
		return nil
	}
	if n, err := q.MarkPSPStaleNotified(ctx, gen.MarkPSPStaleNotifiedParams{MerchantID: mid, PspID: psp, StaleSince: *row.StaleSince, NotifiedAt: now.UTC()}); err != nil || n == 0 {
		return err
	}
	return queuePSPNotice(ctx, txdb, mid, row, f, billing.HostEventPSPStale, now)
}

// queuePSPNotice writes the crossing's host event and queues its alert.
func queuePSPNotice(ctx context.Context, txdb *db.DB, mid uuid.UUID, row gen.ListPSPFreshnessRow, f billing.PSPFreshness, kind billing.HostEventType, at time.Time) error {
	event := billing.PSPHostEvent{PSPID: billing.PSPID(row.PspID), Key: row.Key, Rail: billing.Rail(row.Rail), Cause: f.Cause, Severity: f.Severity,
		KnownThrough: f.KnownThrough, CatchUp: f.CatchUp}
	if row.StaleSince != nil {
		event.StaleSince = row.StaleSince.UTC()
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if err := txdb.Gen(ctx).EnqueuePSPHostEvent(ctx, gen.EnqueuePSPHostEventParams{MerchantID: mid, EventType: string(kind), PspID: row.PspID,
		OccurredAt: at.UTC(), Data: data, DedupeKey: string(kind) + ":" + row.PspID.String() + ":" + event.StaleSince.Format(time.RFC3339Nano)}); err != nil {
		return err
	}
	alert := &alertjob.PSPAlert{ID: row.PspID, Key: row.Key, Rail: row.Rail, Cause: string(f.Cause), Severity: string(f.Severity),
		StaleSince: event.StaleSince, KnownThrough: f.KnownThrough, Recovered: kind == billing.HostEventPSPRecovered}
	if c := f.CatchUp; c != nil && alert.Recovered {
		alert.Renewed, alert.Failed, alert.Canceled, alert.Unconfirmed = c.Renewed, c.Failed, c.Canceled, c.Unconfirmed
	}
	_, err = txdb.InsertRiverJob(ctx, alertjob.DeliveryArgs{MerchantID: mid, PSP: alert}, nil)
	return err
}
