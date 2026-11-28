package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// catchUpPage is how many subscriptions a catch-up reads at a time.
const catchUpPage = 200

// ErrPSPConfigUnavailable is a catch-up that could not read the merchant's
// configuration: a later attempt may arm the PSP.
var ErrPSPConfigUnavailable = errors.New("merchant configuration unavailable")

// CatchUp looks at one PSP. One that cannot be read has its failure recorded:
// its stale episode opens or continues, and its notice is queued once the
// episode matters. A stale one that can be read has its gap read first, every
// subscription the overdue detector skipped and every unverified one, before
// detection resumes; the episode then ends with what the gap hid.
func (v *Verifier) CatchUp(ctx context.Context, mid billing.MerchantID, psp uuid.UUID) error {
	v.init()
	return v.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(ctx context.Context) error {
		now := v.Clock.Now().UTC()
		armed, provider, arming := v.Builder.ArmPSP(ctx, mid, psp)
		switch arming {
		case PSPArmingUnavailable:
			return fmt.Errorf("catch-up PSP %s: %w", psp, ErrPSPConfigUnavailable)
		case PSPUnarmed:
			return RecordPSPRead(ctx, v.DB, mid.UUID(), psp, now, ErrPSPNotArmed)
		}
		_, rows, err := PSPFreshness(ctx, v.DB.Gen(ctx), mid.UUID(), &psp, now)
		if err != nil || len(rows) != 1 || rows[0].StaleSince == nil {
			return err
		}
		episode := *rows[0].StaleSince
		from, _ := latestEvidence(rows[0])
		lc := v.lifecycle()
		nmiBefore, otherBefore := now.Add(-NMIRebillDeadline), now.Add(-PeriodGrace)
		afterAt, afterID := time.Time{}, uuid.Nil
		for {
			page, err := v.DB.Gen(ctx).ListCatchUpSubscriptions(ctx, gen.ListCatchUpSubscriptionsParams{
				MerchantID: mid.UUID(), PspID: psp, NmiOverdueBefore: nmiBefore, OverdueBefore: otherBefore,
				AfterDueAt: afterAt, AfterID: afterID, RowLimit: catchUpPage,
			})
			if err != nil {
				return fmt.Errorf("catch-up PSP %s: %w", psp, err)
			}
			if len(page) == 0 {
				break
			}
			subs, err := loadSubscriptions(ctx, v.DB, page)
			if err != nil {
				return err
			}
			if readErr := readPSP(ctx, v.DB, lc, armed, provider, psp, subs, now); readErr != nil {
				recordReads(ctx, v.DB, mid, subs, now, readErr)
				return RecordPSPRead(ctx, v.DB, mid.UUID(), psp, now, readErr)
			}
			recordReads(ctx, v.DB, mid, subs, now, nil)
			last := page[len(page)-1]
			afterID = last.ID
			afterAt = time.Time{}
			if last.CurrentPeriodEndsAt != nil {
				afterAt = *last.CurrentPeriodEndsAt
			}
			if len(page) < catchUpPage {
				break
			}
		}
		start := episode
		if from != nil {
			start = *from
		}
		return v.completeCatchUp(ctx, mid.UUID(), psp, episode, start, now)
	})
}

// completeCatchUp ends the stale episode with what its gap hid, and tells the
// merchant it recovered when it was told it went stale.
func (v *Verifier) completeCatchUp(ctx context.Context, mid, psp uuid.UUID, episode, from, through time.Time) error {
	return v.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := v.DB.NewWithPgxTx(tx)
		q := txdb.Gen(ctx)
		row, err := q.LockPSPFreshness(ctx, gen.LockPSPFreshnessParams{MerchantID: mid, PspID: psp})
		if err != nil {
			return err
		}
		if row.StaleSince == nil || !row.StaleSince.Equal(episode) {
			return nil // another catch-up ended it
		}
		counts, err := q.PSPCatchUpSummary(ctx, gen.PSPCatchUpSummaryParams{MerchantID: mid, PspID: psp, FromAt: from, Through: through})
		if err != nil {
			return err
		}
		summary := billing.PSPCatchUp{From: from.UTC(), Through: through.UTC(), Renewed: counts.Renewed, Failed: counts.Failed, Canceled: counts.Canceled, Unconfirmed: counts.Unconfirmed}
		data, err := json.Marshal(summary)
		if err != nil {
			return err
		}
		_, rows, err := PSPFreshness(ctx, q, mid, &psp, through)
		if err != nil || len(rows) != 1 {
			return err
		}
		ended := rows[0]
		before := PSPFreshnessOf(ended, through)
		if _, err := q.CompletePSPCatchUp(ctx, gen.CompletePSPCatchUpParams{MerchantID: mid, PspID: psp, StaleSince: episode, Through: through, CatchUp: data}); err != nil {
			return err
		}
		v.Log.InfoContext(ctx, "known-through: PSP caught up", "psp_id", psp, "from", from, "through", through,
			"renewed", summary.Renewed, "failed", summary.Failed, "canceled", summary.Canceled, "unconfirmed", summary.Unconfirmed)
		if row.NotifiedAt == nil {
			return nil
		}
		before.CatchUp = &summary
		return queuePSPNotice(ctx, txdb, mid, ended, before, billing.HostEventPSPRecovered, through)
	})
}

// loadSubscriptions loads the catch-up page's rows.
func loadSubscriptions(ctx context.Context, database *db.DB, page []gen.ListCatchUpSubscriptionsRow) ([]*models.Subscription, error) {
	repo := subscriptions.NewSubscriptionRepo(database)
	out := make([]*models.Subscription, 0, len(page))
	for _, r := range page {
		sub, err := repo.GetByID(ctx, r.ID)
		if err != nil {
			return nil, fmt.Errorf("catch-up: load %s: %w", r.ID, err)
		}
		out = append(out, sub)
	}
	return out, nil
}
