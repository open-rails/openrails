package riverjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/progress"
)

const KindLedgerIntegrity = "openrails.ledger_integrity"

// Finding types (reconciliation_findings' CHECK: family.seg[.seg]).
const (
	FindingLedgerConservation = "consistency.ledger.conservation"
	FindingLedgerCounterDrift = "consistency.ledger.counter_drift"
)

type LedgerIntegrityArgs struct{}

func (LedgerIntegrityArgs) Kind() string { return KindLedgerIntegrity }

// LedgerIntegrityWorker checks the two ledger invariants per merchant and
// raises an operator finding on divergence.
//
// It is a periodic full check because this failure has no activity signal:
// ledger_accounts' credits_posted/debits_posted are a projection maintained
// by the transfer insert trigger, and only a write that bypassed it (a
// superuser session, COPY, a restore, a migration with triggers disabled)
// makes them diverge from ledger_transfers. Daily bounds how long a drifted
// balance compounds; the sources are rare maintenance events. Both checks run
// inside each merchant's RunInMerchantScope.
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
		merchantID := billing.MerchantID(mid)
		progress.Mark(ctx, "ledger integrity merchant "+merchantID.String())
		if err := w.DB.RunInMerchantScope(ctx, merchantID, "ledger integrity audit", func(mctx context.Context) error {
			// The connection is pinned to this merchant; the explicit
			// merchant predicate scopes the check.
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
func (w LedgerIntegrityWorker) raiseLedgerFindings(ctx context.Context, mid billing.MerchantID, report ledger.IntegrityReport) error {
	q := w.DB.Gen(ctx)
	for _, b := range report.Conservation {
		evidence, _ := json.Marshal(map[string]any{
			"currency": b.Currency, "net": b.Net, "accounts": b.Accounts, "detail": b.String(),
		})
		action := fmt.Sprintf(
			"the %s ledger does not net to zero (%s across %d accounts). Double entry means every transfer credits and debits the same amount, so a non-zero sum is money created or destroyed inside the ledger — a one-sided counter write, or a transfer applied to only one leg. Do NOT settle or invoice off these balances until it is explained; `openrails ledger-audit --merchant=id:%s` reproduces it",
			b.Currency, moneyutil.FormatAmount(b.Net, b.Currency), b.Accounts, mid.String())
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
			"account %s (%s) disagrees with the transfer log: credits stored=%s logged=%s, debits stored=%s logged=%s. The counters are a trigger-maintained projection, so this means a write bypassed the trigger (superuser session, COPY, restore, a migration that disabled triggers) — the append-only log is the truth and every balance read on this account is wrong. Reconcile from ledger_transfers before trusting it",
			d.AccountID, d.AccountType,
			moneyutil.FormatAmount(d.StoredCredits, d.Currency), moneyutil.FormatAmount(d.LoggedCredits, d.Currency),
			moneyutil.FormatAmount(d.StoredDebits, d.Currency), moneyutil.FormatAmount(d.LoggedDebits, d.Currency))
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
func (w LedgerIntegrityWorker) resolveLedgerFindings(ctx context.Context, mid billing.MerchantID) error {
	return w.DB.Gen(ctx).AutoResolveReviewFindingsByType(ctx, gen.AutoResolveReviewFindingsByTypeParams{
		MerchantID: mid.UUID(), FindingTypes: []string{FindingLedgerConservation, FindingLedgerCounterDrift},
	})
}
