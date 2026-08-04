package riverjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/pkg/merchant"
)

const KindLedgerIntegrity = "openrails.ledger_integrity"

// Finding types (reconciliation_findings' CHECK regex:
// (pull|derive|life|consistency).seg[.seg]).
const (
	FindingLedgerConservation = "consistency.ledger.conservation"
	FindingLedgerCounterDrift = "consistency.ledger.counter_drift"
)

type LedgerIntegrityArgs struct{}

func (LedgerIntegrityArgs) Kind() string { return KindLedgerIntegrity }

// LedgerIntegrityWorker runs or#833's two ledger invariants per merchant and
// raises an operator finding on divergence.
//
// Why a periodic FULL check, when the standing rule is "work scales with
// activity, not records": for this failure mode there IS no activity signal.
// `ledger_accounts.{credits,debits}_posted` is a MAINTAINED PROJECTION written
// by a SECURITY DEFINER insert trigger. The only way it can diverge from
// `ledger_transfers` is a write that BYPASSED that trigger — a superuser
// session, a COPY, a restore, a migration that disabled triggers. Those emit no
// event, touch no watermark and raise no error; every balance read is simply
// wrong from then on. Nothing can be pushed, so something has to look.
//
// Cadence: DAILY. It is the slowest cadence that still bounds the damage —
// balances, entitlement decisions and invoices computed off a drifted counter
// compound for exactly one day before an operator hears about it — and the
// drift sources are rare, human-initiated maintenance events, so nothing is
// gained by looking more often. The cost per merchant is one aggregate over
// ledger_accounts plus one grouped pass over that merchant's transfer log, off
// the request path, on the maintenance queue.
//
// or#824/or#861: no bare-context sweep. `ledger_accounts` and `ledger_transfers`
// FORCE RLS, so a no-GUC pass would read NOTHING and report a perfectly clean
// fleet forever — the exact silence this job exists to break. The merchant list
// comes from the global control-plane `merchants` table via GenDirectory(); both
// checks then run inside each merchant's own RunInMerchantScope.
type LedgerIntegrityWorker struct {
	river.WorkerDefaults[LedgerIntegrityArgs]
	DB    *db.DB
	Clock clockwork.Clock
}

func (LedgerIntegrityWorker) Kind() string { return KindLedgerIntegrity }

func (w LedgerIntegrityWorker) Work(ctx context.Context, job *river.Job[LedgerIntegrityArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindLedgerIntegrity)

	merchantIDs, err := w.DB.GenDirectory().ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("ledger integrity: list merchants: %w", err)
	}

	var checked, breached int
	for _, mid := range merchantIDs {
		merchantID := merchant.ID(mid)
		if err := w.DB.RunInMerchantScope(ctx, merchantID, "ledger integrity audit", func(mctx context.Context) error {
			// Scoped to this merchant twice over: the RLS policy on the
			// connection, and the explicit merchant predicate.
			report, cerr := ledger.CheckIntegrity(mctx, w.DB.Qx(mctx), mid)
			if cerr != nil {
				return cerr
			}
			checked++
			if report.OK() {
				return w.resolveLedgerFindings(mctx, merchantID)
			}
			breached++
			return w.raiseLedgerFindings(mctx, merchantID, report)
		}); err != nil {
			// One merchant's failure must not abort the audit of the rest.
			logger.WithError(err).WithField("merchant_id", merchantID.String()).
				Error("ledger integrity: merchant audit failed; continuing")
			continue
		}
	}
	logger.WithFields(log.Fields{
		"merchants_checked": checked, "merchants_breached": breached,
	}).Info("ledger integrity audit complete")
	return nil
}

// raiseLedgerFindings files one standing finding per breach. A drifted counter
// is a MONEY correctness fault, so it is critical: every balance read against
// that account has been wrong since the drift appeared.
func (w LedgerIntegrityWorker) raiseLedgerFindings(ctx context.Context, mid merchant.ID, report ledger.IntegrityReport) error {
	q := w.DB.Gen(ctx)
	for _, b := range report.Conservation {
		evidence, _ := json.Marshal(map[string]any{
			"currency": b.Currency, "net": b.Net, "accounts": b.Accounts, "detail": b.String(),
		})
		action := fmt.Sprintf(
			"the %s ledger does not net to zero (%d micros across %d accounts). Double entry means every transfer credits and debits the same amount, so a non-zero sum is money created or destroyed inside the ledger — a one-sided counter write, or a transfer applied to only one leg. Do NOT settle or invoice off these balances until it is explained; `openrails ledger-audit --merchant=%s` reproduces it",
			b.Currency, b.Net, b.Accounts, mid.String())
		if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
			MerchantID:        mid.UUID(),
			FindingType:       FindingLedgerConservation,
			SubjectKey:        strings.ToLower(b.Currency),
			Severity:          "critical",
			Status:            "requires_review",
			RecommendedAction: &action,
			Evidence:          evidence,
		}); err != nil {
			return fmt.Errorf("raise conservation finding: %w", err)
		}
	}
	for _, d := range report.Counters {
		evidence, _ := json.Marshal(map[string]any{
			"account_id": d.AccountID, "account_type": d.AccountType, "currency": d.Currency,
			"stored_credits": d.StoredCredits, "logged_credits": d.LoggedCredits,
			"stored_debits": d.StoredDebits, "logged_debits": d.LoggedDebits,
			"detail": d.String(),
		})
		action := fmt.Sprintf(
			"account %s (%s/%s) disagrees with the transfer log: credits stored=%d logged=%d, debits stored=%d logged=%d. The counters are a trigger-maintained projection, so this means a write bypassed the trigger (superuser session, COPY, restore, a migration that disabled triggers) — the append-only log is the truth and every balance read on this account is wrong. Reconcile from ledger_transfers before trusting it",
			d.AccountID, d.AccountType, d.Currency,
			d.StoredCredits, d.LoggedCredits, d.StoredDebits, d.LoggedDebits)
		if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
			MerchantID:        mid.UUID(),
			FindingType:       FindingLedgerCounterDrift,
			SubjectKey:        d.AccountID.String(),
			Severity:          "critical",
			Status:            "requires_review",
			RecommendedAction: &action,
			Evidence:          evidence,
		}); err != nil {
			return fmt.Errorf("raise counter drift finding: %w", err)
		}
	}
	return nil
}

// resolveLedgerFindings closes this merchant's standing ledger findings once
// the invariants hold again — the check is precise, so a repaired ledger must
// go quiet rather than leave a permanently red board.
func (w LedgerIntegrityWorker) resolveLedgerFindings(ctx context.Context, mid merchant.ID) error {
	_, err := w.DB.Qx(ctx).Exec(ctx, `
		UPDATE openrails.reconciliation_findings
		   SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(),
		       notified_at = NULL, notified_severity = NULL, updated_at = now()
		 WHERE merchant_id = $1::uuid
		   AND finding_type = ANY($2::text[])
		   AND status = 'requires_review'`,
		mid.UUID(), []string{FindingLedgerConservation, FindingLedgerCounterDrift})
	return err
}
