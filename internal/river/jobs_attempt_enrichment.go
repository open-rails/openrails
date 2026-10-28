package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/railresolve"
)

const (
	KindAttemptEnrichment = "openrails.attempt_enrichment"

	// AttemptEnrichmentInterval is how often NMI attempts are enriched.
	AttemptEnrichmentInterval = time.Hour
	// enrichAfter: NMI's Query API indexes a transaction with some lag.
	enrichAfter = 10 * time.Minute
	// enrichGiveUp: a transaction the report still lacks after this, or whose
	// PSP has no client, is marked read with nothing, so it is not asked for
	// again.
	enrichGiveUp = 48 * time.Hour
	// enrichWindow: a day of passes past the give-up; an older attempt is
	// marked read with nothing, unread.
	enrichWindow = enrichGiveUp + 24*time.Hour

	enrichMerchantBatch = 200
	enrichRowBatch      = 1000
)

// AttemptEnrichmentArgs runs one enrichment pass.
type AttemptEnrichmentArgs struct{}

func (AttemptEnrichmentArgs) Kind() string { return KindAttemptEnrichment }

// AttemptEnrichmentWorker fills NMI attempts from the Query API's transaction
// report: the card's BIN and brand, AVS/CVV the reply lacked, the issuer's raw
// answer, and whether a network token was used. Each attempt is read once, up
// to nmi.MaxQueryIDs per query.
type AttemptEnrichmentWorker struct {
	river.WorkerDefaults[AttemptEnrichmentArgs]
	DB          *db.DB
	Clock       clockwork.Clock
	NMIResolver railresolve.NMIClientResolver
}

func (AttemptEnrichmentWorker) Kind() string { return KindAttemptEnrichment }

func (w *AttemptEnrichmentWorker) Work(ctx context.Context, _ *river.Job[AttemptEnrichmentArgs]) error {
	if w.NMIResolver == nil {
		return errors.New("attempt enrichment: nmi resolver not wired")
	}
	now := w.Clock.Now().UTC()
	before := now.Add(-enrichAfter)
	merchantIDs, err := w.DB.GenDirectory().ListUnenrichedAttemptMerchants(ctx, gen.ListUnenrichedAttemptMerchantsParams{
		Before: before, MerchantLimit: enrichMerchantBatch,
	})
	if err != nil {
		return fmt.Errorf("attempt enrichment: list merchants: %w", err)
	}
	var workErr error
	for _, mid := range merchantIDs {
		err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(mid), "attempt enrichment", func(ctx context.Context) error {
			return w.enrich(ctx, mid, before, now)
		})
		if err != nil {
			workErr = errors.Join(workErr, fmt.Errorf("merchant %s: %w", mid, err))
		}
	}
	return workErr
}

func (w *AttemptEnrichmentWorker) enrich(ctx context.Context, mid uuid.UUID, before, now time.Time) error {
	rows, err := w.DB.Gen(ctx).ListUnenrichedNMIAttempts(ctx, gen.ListUnenrichedNMIAttemptsParams{
		MerchantID: mid, Before: before, RowLimit: enrichRowBatch,
	})
	if err != nil {
		return err
	}
	// Attempts past the window are not read: they are marked read with
	// nothing, so none stays unenriched for good.
	byPSP := map[uuid.UUID][]gen.ListUnenrichedNMIAttemptsRow{}
	var errs error
	for _, r := range rows {
		if now.Sub(r.AttemptedAt) >= enrichWindow {
			errs = errors.Join(errs, attempts.Enrich(ctx, w.DB.Gen(ctx), mid, r.ID, decline.Evidence{}, now))
			continue
		}
		byPSP[r.PspID] = append(byPSP[r.PspID], r)
	}
	for psp, rows := range byPSP {
		client, ok, err := w.NMIResolver.ResolveNMIClient(ctx, mid, &psp)
		if err != nil && !errors.Is(err, railresolve.ErrRailNotArmed) {
			errs = errors.Join(errs, fmt.Errorf("psp %s: nmi client: %w", psp, err))
			continue
		}
		if err != nil || !ok || client == nil {
			// The PSP has no credentials: its attempts wait out the give-up,
			// then are marked read with nothing.
			log.WithContext(ctx).WithError(err).WithField("psp_id", psp).Warn("attempt enrichment: PSP has no NMI credentials; attempts past the give-up are marked read")
			errs = errors.Join(errs, w.giveUp(ctx, mid, rows, now))
			continue
		}
		for len(rows) > 0 {
			n := min(len(rows), nmi.MaxQueryIDs)
			errs = errors.Join(errs, w.enrichBatch(ctx, client, mid, rows[:n], now))
			rows = rows[n:]
		}
	}
	return errs
}

// giveUp marks the attempts past the give-up read with nothing.
func (w *AttemptEnrichmentWorker) giveUp(ctx context.Context, mid uuid.UUID, rows []gen.ListUnenrichedNMIAttemptsRow, now time.Time) error {
	q := w.DB.Gen(ctx)
	for _, r := range rows {
		if now.Sub(r.AttemptedAt) < enrichGiveUp {
			continue
		}
		if err := attempts.Enrich(ctx, q, mid, r.ID, decline.Evidence{}, now); err != nil {
			return err
		}
	}
	return nil
}

// enrichBatch reads one batch of transactions and fills their attempts.
func (w *AttemptEnrichmentWorker) enrichBatch(ctx context.Context, client *nmi.NMIClient, mid uuid.UUID, rows []gen.ListUnenrichedNMIAttemptsRow, now time.Time) error {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.TransactionID)
	}
	report, err := client.TransactionReport(ctx, nmi.QueryFilter{TransactionID: strings.Join(ids, ",")})
	if err != nil {
		return err
	}
	found := make(map[string]nmi.QueryTransaction, len(report.Transactions))
	for _, t := range report.Transactions {
		found[strings.TrimSpace(t.TransactionID)] = t
	}
	q := w.DB.Gen(ctx)
	for _, r := range rows {
		var answer decline.Evidence
		if t, ok := found[r.TransactionID]; ok {
			action, _ := t.Authorization()
			answer = t.Evidence(action)
		} else if now.Sub(r.AttemptedAt) < enrichGiveUp {
			continue // not indexed yet
		}
		if err := attempts.Enrich(ctx, q, mid, r.ID, answer, now); err != nil {
			return err
		}
	}
	return nil
}
