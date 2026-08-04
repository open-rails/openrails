package riverjobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/pkg/merchant"
)

const KindFindingsDigest = "openrails.findings_digest"

// FindingsDigestArgs triggers one low-severity reconciliation-findings digest
// pass (#787): select merchants with an undigested low-severity requires_review
// finding (indexed — work scales with backlog, not the merchant directory),
// and for each, push ONE digest notification if the #736-style cadence gate is
// due. Distinct from AlertEvalWorker: findings are not a metric-threshold rule.
type FindingsDigestArgs struct{}

func (FindingsDigestArgs) Kind() string { return KindFindingsDigest }

// FindingsDigestWorker is the slow-cadence findings-digest sweep. Merchant
// selection goes through 0021's SECURITY DEFINER scan (ids only); each
// merchant's digest then runs inside its own scope. One merchant's failure is
// logged and skipped — never aborts the sweep.
//
// or#877 (found by FC-16): the per-merchant leg used to hand DigestFindings a
// merchant.WithID context only — a context VALUE, not a pinned connection — so
// every read it made (the cadence watermark, the pending-findings count) ran on
// the base pool and came back empty. The sweep selected merchants with a
// backlog and then digested nothing, for all of them.
type FindingsDigestWorker struct {
	river.WorkerDefaults[FindingsDigestArgs]
	DB     *db.DB
	Alerts *alerting.Service
}

func (FindingsDigestWorker) Kind() string { return KindFindingsDigest }

func (w FindingsDigestWorker) Work(ctx context.Context, _ *river.Job[FindingsDigestArgs]) error {
	if w.Alerts == nil {
		return nil
	}
	merchantIDs, err := w.Alerts.ArmedFindingsDigestMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("findings digest: list armed merchants: %w", err)
	}
	logger := log.WithContext(ctx).WithField("worker", KindFindingsDigest)

	var swept int
	var digested int64
	for _, mid := range merchantIDs {
		if err := w.DB.RunInMerchantScope(ctx, merchant.ID(mid), "findings digest", func(mctx context.Context) error {
			stats, err := w.Alerts.DigestFindings(mctx)
			if err != nil {
				return err
			}
			swept++
			digested += stats.Digested
			return nil
		}); err != nil {
			logger.WithError(err).WithField("merchant_id", mid).
				Error("findings digest: merchant failed; continuing")
		}
	}
	if digested > 0 {
		logger.WithFields(log.Fields{"merchants": swept, "digested": digested}).
			Info("findings digest pass completed")
	}
	return nil
}
