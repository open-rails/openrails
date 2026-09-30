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

	"github.com/open-rails/openrails/internal/billing/decline"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	KindAttemptEnrichment = "openrails.attempt_enrichment"

	// AttemptEnrichmentInterval is how often NMI attempts are enriched.
	AttemptEnrichmentInterval = time.Hour
	// enrichAfter: NMI's Query API indexes a transaction with some lag.
	enrichAfter = 10 * time.Minute
	// enrichWindow: older attempts are left as they are.
	enrichWindow = 30 * 24 * time.Hour
	// enrichGiveUp: a transaction the report still lacks after this is
	// marked read with nothing, so it is not asked for again.
	enrichGiveUp = 48 * time.Hour

	enrichMerchantBatch = 200
	enrichRowBatch      = 1000
)

// AttemptEnrichmentArgs runs one enrichment pass.
type AttemptEnrichmentArgs struct{}

func (AttemptEnrichmentArgs) Kind() string { return KindAttemptEnrichment }

// AttemptEnrichmentWorker fills NMI attempts from the Query API's transaction
// report (#1114): the card's BIN and brand, AVS/CVV the reply lacked, the
// issuer's raw answer, and whether a network token was used. Each attempt is
// read once, up to nmi.MaxQueryIDs per query.
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
	since, before := now.Add(-enrichWindow), now.Add(-enrichAfter)
	merchantIDs, err := w.DB.GenDirectory().ListUnenrichedAttemptMerchants(ctx, gen.ListUnenrichedAttemptMerchantsParams{
		Since: since, Before: before, MerchantLimit: enrichMerchantBatch,
	})
	if err != nil {
		return fmt.Errorf("attempt enrichment: list merchants: %w", err)
	}
	var workErr error
	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		err := w.DB.RunInMerchantScope(ctx, merchant.ID(*mid), "attempt enrichment", func(ctx context.Context) error {
			return w.enrich(ctx, *mid, since, before, now)
		})
		if err != nil {
			workErr = errors.Join(workErr, fmt.Errorf("merchant %s: %w", *mid, err))
		}
	}
	return workErr
}

func (w *AttemptEnrichmentWorker) enrich(ctx context.Context, mid uuid.UUID, since, before, now time.Time) error {
	rows, err := w.DB.Gen(ctx).ListUnenrichedNMIAttempts(ctx, gen.ListUnenrichedNMIAttemptsParams{
		MerchantID: mid, Since: since, Before: before, RowLimit: enrichRowBatch,
	})
	if err != nil {
		return err
	}
	byPSP := map[uuid.UUID][]gen.ListUnenrichedNMIAttemptsRow{}
	for _, r := range rows {
		byPSP[r.PspID] = append(byPSP[r.PspID], r)
	}
	var errs error
	for psp, rows := range byPSP {
		client, ok, err := w.NMIResolver.ResolveNMIClient(ctx, mid, &psp)
		if err != nil || !ok || client == nil {
			errs = errors.Join(errs, fmt.Errorf("psp %s: nmi client unavailable: %v", psp, err))
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
