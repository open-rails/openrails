package riverjobs

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
)

const KindBillingReconciliation = "billing.reconciliation"

type BillingReconciliationArgs struct{}

func (BillingReconciliationArgs) Kind() string { return KindBillingReconciliation }

// BillingReconciliationWorker is the alert-first reconciliation + orphan-hold
// cleanup loop (issue #243). It complements the 5-minute HoldExpiryWorker with
// request-level reconciliation and self-consistency checks. Every run, in order:
//
//  1. ORPHAN HOLDS: find transaction_type='hold' rows stuck in status='active'
//     past their expires_at (a worker died without capture/release and the
//     HoldExpiryWorker has not yet caught up). Record an alert-first
//     reconciliation_events row, then SAFELY release the hold (status->'expired',
//     held_balance restored) under the SAME row-locking discipline as the
//     HoldExpiryWorker.
//  2. HELD_BALANCE DRIFT: per (tenant, owner, credit_type), compare the
//     denormalized user_credit_balances.held_balance against SUM(authorized_amount)
//     over still-active holds. Record drift, then correct held_balance to the
//     ledger-derived value.
//  3. BALANCE DRIFT: per (tenant, owner, credit_type), compare the denormalized
//     user_credit_balances.balance against the credit_transactions ledger
//     (SUM(amount) of posted rows). ALERT-ONLY — reported, never auto-corrected
//     (the available-credit ledger has more inputs — FIFO blocks, expiry — than
//     this job models, so an operator decides).
//
// Cross-system drift (Tensorhub usage events vs OpenRails ledger) is NOT run on
// the timer because Tensorhub data is not in this repo. It is exposed as the
// ReconcileSettlements method (the diff INTERFACE / report surface) the host
// drives with a batch of credits.ExpectedSettlement.
//
// ALERT-FIRST: every divergence is persisted to billing.reconciliation_events
// and emitted to the Sink BEFORE remediation, so operators always see what was
// detected even if remediation is later disabled. Owner/tenant-scoped (#221/#223).
type BillingReconciliationWorker struct {
	river.WorkerDefaults[BillingReconciliationArgs]
	DB        *db.DB
	Config    *config.Config
	Clock     clockwork.Clock
	BatchSize int
	// Sink receives every ReconciliationEvent (alert-first). When nil the worker
	// only persists to billing.reconciliation_events.
	Sink credits.ReconciliationSink
	// AutoRemediate gates the safe auto-repair of orphan holds + held_balance
	// drift. When false the job is pure detect+report (alert-only). Defaults to
	// true; set OPENRAILS_RECONCILIATION_AUTOREMEDIATE=false to disable.
	AutoRemediate bool
}

func (BillingReconciliationWorker) Kind() string { return KindBillingReconciliation }

func (w BillingReconciliationWorker) Work(ctx context.Context, job *river.Job[BillingReconciliationArgs]) error {
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	now := clock.Now().UTC()
	logger := log.WithContext(ctx).WithField("worker", KindBillingReconciliation)

	orphans, err := w.reconcileOrphanHolds(ctx, now)
	if err != nil {
		return fmt.Errorf("orphan-hold reconciliation: %w", err)
	}
	heldDrift, err := w.reconcileHeldBalanceDrift(ctx, now)
	if err != nil {
		return fmt.Errorf("held_balance drift reconciliation: %w", err)
	}
	balDrift, err := w.reconcileBalanceDrift(ctx, now)
	if err != nil {
		return fmt.Errorf("balance drift reconciliation: %w", err)
	}

	logger.WithFields(log.Fields{
		"orphan_holds":       orphans,
		"held_balance_drift": heldDrift,
		"balance_drift":      balDrift,
		"auto_remediate":     w.autoRemediate(),
	}).Info("billing reconciliation pass complete")
	return nil
}

// autoRemediate resolves the gate (default ON).
func (w BillingReconciliationWorker) autoRemediate() bool {
	return w.AutoRemediate
}

func (w BillingReconciliationWorker) bun() *bun.DB { return w.DB.GetDB().(*bun.DB) }

func (w BillingReconciliationWorker) batchSize() int {
	if w.BatchSize > 0 {
		return w.BatchSize
	}
	return 200
}

// balanceKey is the unified-balance identity (issue #221/#223): user_id is ACTOR
// attribution only and is NOT part of balance identity.
type balanceKey struct {
	TenantID     uuid.UUID
	OwnerID      uuid.UUID
	CreditTypeID uuid.UUID
}

// reconcileOrphanHolds finds active holds past their expiry, records an
// alert-first event for each, and (when AutoRemediate) safely releases them —
// marking the hold 'expired' and restoring held_balance with the same row
// locking the HoldExpiryWorker uses. Returns the number of orphans detected.
func (w BillingReconciliationWorker) reconcileOrphanHolds(ctx context.Context, now time.Time) (int, error) {
	detected := 0
	for {
		tx, err := w.bun().BeginTx(ctx, nil)
		if err != nil {
			return detected, err
		}

		var holds []models.CreditTransaction
		if err := tx.NewSelect().
			Model(&holds).
			Where("transaction_type = ? AND status = ? AND expires_at IS NOT NULL AND expires_at <= ?", "hold", "active", now).
			OrderExpr("expires_at ASC").
			Limit(w.batchSize()).
			For("UPDATE SKIP LOCKED").
			Scan(ctx); err != nil {
			_ = tx.Rollback()
			return detected, err
		}
		if len(holds) == 0 {
			if err := tx.Commit(); err != nil {
				return detected, err
			}
			break
		}

		releasedTotals := make(map[balanceKey]int64)
		for i := range holds {
			hold := &holds[i]
			detected++

			// ALERT-FIRST: record the orphan before touching anything.
			remediated := w.autoRemediate()
			if err := w.persistEvent(ctx, tx, models.ReconciliationEvent{
				TenantID:     ptrUUID(hold.TenantID),
				OwnerID:      ptrUUID(hold.OwnerID),
				CreditTypeID: ptrUUID(hold.CreditTypeID),
				Kind:         models.ReconciliationOrphanHold,
				SubjectID:    hold.ID.String(),
				DetectedAt:   now,
			}, remediated, now); err != nil {
				_ = tx.Rollback()
				return detected, err
			}
			w.emit(ctx, credits.ReconciliationEvent{
				Kind:         credits.ReconOrphanHold,
				TenantID:     ptrUUID(hold.TenantID),
				OwnerID:      ptrUUID(hold.OwnerID),
				CreditTypeID: ptrUUID(hold.CreditTypeID),
				UserID:       hold.UserID,
				SubjectID:    hold.ID.String(),
				Remediated:   remediated,
				DetectedAt:   now,
			})

			if !w.autoRemediate() {
				continue
			}
			if hold.Authorized != nil && *hold.Authorized > 0 {
				releasedTotals[balanceKey{hold.TenantID, hold.OwnerID, hold.CreditTypeID}] += *hold.Authorized
			}
			hold.Status = "expired"
			hold.UpdatedAt = now
			if _, err := tx.NewUpdate().Model(hold).Column("status", "updated_at").WherePK().Exec(ctx); err != nil {
				_ = tx.Rollback()
				return detected, err
			}
		}

		// Restore held_balance for the released holds (same discipline as the
		// HoldExpiryWorker: lock the owner balance row, never go below zero).
		for k, amount := range releasedTotals {
			if amount <= 0 {
				continue
			}
			bal := new(models.UserCreditBalance)
			if err := tx.NewSelect().Model(bal).
				Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", k.TenantID, k.OwnerID, k.CreditTypeID).
				For("UPDATE").Scan(ctx); err != nil {
				_ = tx.Rollback()
				return detected, err
			}
			newHeld := bal.HeldBalance - amount
			if newHeld < 0 {
				newHeld = 0
			}
			if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
				Set("held_balance = ?", newHeld).
				Set("updated_at = ?", now).
				Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", k.TenantID, k.OwnerID, k.CreditTypeID).
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return detected, err
			}
		}

		if err := tx.Commit(); err != nil {
			return detected, err
		}
		if len(holds) < w.batchSize() {
			break
		}
	}
	return detected, nil
}

// heldDriftRow projects the per-owner comparison of denormalized held_balance
// vs. the sum of still-active hold reservations.
type heldDriftRow struct {
	BalanceID    uuid.UUID `bun:"balance_id"`
	TenantID     uuid.UUID `bun:"tenant_id"`
	OwnerID      uuid.UUID `bun:"owner_id"`
	UserID       string    `bun:"user_id"`
	CreditTypeID uuid.UUID `bun:"credit_type_id"`
	HeldBalance  int64     `bun:"held_balance"`
	ActiveHolds  int64     `bun:"active_holds"`
}

// reconcileHeldBalanceDrift compares each owner balance row's denormalized
// held_balance against SUM(authorized_amount) over its still-active holds.
// Records drift alert-first, then (when AutoRemediate) corrects held_balance to
// the ledger-derived value under a row lock. Returns the number of drifted rows.
func (w BillingReconciliationWorker) reconcileHeldBalanceDrift(ctx context.Context, now time.Time) (int, error) {
	// LEFT JOIN active holds so balances with zero active holds (active_holds=0)
	// are still compared — that's how a stale non-zero held_balance is caught.
	var rows []heldDriftRow
	if err := w.bun().NewSelect().
		ColumnExpr("ucb.id AS balance_id").
		ColumnExpr("ucb.tenant_id AS tenant_id").
		ColumnExpr("ucb.owner_id AS owner_id").
		ColumnExpr("ucb.user_id AS user_id").
		ColumnExpr("ucb.credit_type_id AS credit_type_id").
		ColumnExpr("ucb.held_balance AS held_balance").
		ColumnExpr("COALESCE(h.active_holds, 0) AS active_holds").
		TableExpr("billing.user_credit_balances AS ucb").
		Join(`LEFT JOIN (
			SELECT tenant_id, owner_id, credit_type_id,
			       SUM(COALESCE(authorized_amount, 0)) AS active_holds
			FROM billing.credit_transactions
			WHERE transaction_type = 'hold' AND status = 'active'
			GROUP BY tenant_id, owner_id, credit_type_id
		) AS h ON h.tenant_id = ucb.tenant_id AND h.owner_id = ucb.owner_id AND h.credit_type_id = ucb.credit_type_id`).
		Where("ucb.held_balance <> COALESCE(h.active_holds, 0)").
		Scan(ctx, &rows); err != nil {
		return 0, fmt.Errorf("scan held_balance drift: %w", err)
	}

	for i := range rows {
		r := rows[i]
		tx, err := w.bun().BeginTx(ctx, nil)
		if err != nil {
			return i, err
		}

		// Re-read the balance row under lock and re-derive active holds inside the
		// tx so a concurrent capture/release between the scan and the fix doesn't
		// clobber a correct value.
		bal := new(models.UserCreditBalance)
		if err := tx.NewSelect().Model(bal).Where("id = ?", r.BalanceID).For("UPDATE").Scan(ctx); err != nil {
			_ = tx.Rollback()
			return i, err
		}
		var active int64
		if err := tx.NewSelect().
			Model((*models.CreditTransaction)(nil)).
			ColumnExpr("COALESCE(SUM(COALESCE(authorized_amount, 0)), 0)").
			Where("transaction_type = 'hold' AND status = 'active'").
			Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", bal.TenantID, bal.OwnerID, bal.CreditTypeID).
			Scan(ctx, &active); err != nil {
			_ = tx.Rollback()
			return i, err
		}
		if bal.HeldBalance == active {
			// Drift resolved itself between scan and lock; nothing to do.
			_ = tx.Commit()
			continue
		}

		remediated := w.autoRemediate()
		if err := w.persistEvent(ctx, tx, models.ReconciliationEvent{
			TenantID:      ptrUUID(bal.TenantID),
			OwnerID:       ptrUUID(bal.OwnerID),
			CreditTypeID:  ptrUUID(bal.CreditTypeID),
			Kind:          models.ReconciliationHeldBalanceDrift,
			SubjectID:     bal.ID.String(),
			ExpectedValue: strconv.FormatInt(active, 10),
			ObservedValue: strconv.FormatInt(bal.HeldBalance, 10),
			DetectedAt:    now,
		}, remediated, now); err != nil {
			_ = tx.Rollback()
			return i, err
		}
		w.emit(ctx, credits.ReconciliationEvent{
			Kind:         credits.ReconHeldBalanceDrift,
			TenantID:     ptrUUID(bal.TenantID),
			OwnerID:      ptrUUID(bal.OwnerID),
			CreditTypeID: ptrUUID(bal.CreditTypeID),
			UserID:       bal.UserID,
			SubjectID:    bal.ID.String(),
			Expected:     active,
			Observed:     bal.HeldBalance,
			Remediated:   remediated,
			DetectedAt:   now,
		})

		if w.autoRemediate() {
			if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
				Set("held_balance = ?", active).
				Set("updated_at = ?", now).
				Where("id = ?", bal.ID).
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return i, err
			}
		}
		if err := tx.Commit(); err != nil {
			return i, err
		}
	}
	return len(rows), nil
}

// balanceDriftRow projects denormalized available balance vs. the posted ledger
// sum per owner balance row.
type balanceDriftRow struct {
	BalanceID    uuid.UUID `bun:"balance_id"`
	TenantID     uuid.UUID `bun:"tenant_id"`
	OwnerID      uuid.UUID `bun:"owner_id"`
	UserID       string    `bun:"user_id"`
	CreditTypeID uuid.UUID `bun:"credit_type_id"`
	Balance      int64     `bun:"balance"`
	LedgerSum    int64     `bun:"ledger_sum"`
}

// reconcileBalanceDrift verifies the denormalized user_credit_balances.balance
// against the credit_transactions ledger (SUM(amount) of posted deposit/
// withdrawal/captured rows) per (tenant, owner, credit_type). ALERT-ONLY: it
// records + emits a balance_drift event but never auto-corrects, because the
// available-credit ledger is also shaped by FIFO credit_blocks and expiry the
// reconciliation job does not model — an operator decides the fix. Returns the
// number of drifted rows reported.
func (w BillingReconciliationWorker) reconcileBalanceDrift(ctx context.Context, now time.Time) (int, error) {
	// Posted ledger amount = SUM(amount) over rows that posted a delta:
	// deposits (+), withdrawals (-), captured holds (- captured_amount, stored as
	// negative amount). Active/released/expired holds post amount=0, so they are
	// naturally neutral here.
	var rows []balanceDriftRow
	if err := w.bun().NewSelect().
		ColumnExpr("ucb.id AS balance_id").
		ColumnExpr("ucb.tenant_id AS tenant_id").
		ColumnExpr("ucb.owner_id AS owner_id").
		ColumnExpr("ucb.user_id AS user_id").
		ColumnExpr("ucb.credit_type_id AS credit_type_id").
		ColumnExpr("ucb.balance AS balance").
		ColumnExpr("COALESCE(l.ledger_sum, 0) AS ledger_sum").
		TableExpr("billing.user_credit_balances AS ucb").
		Join(`LEFT JOIN (
			SELECT tenant_id, owner_id, credit_type_id, SUM(amount) AS ledger_sum
			FROM billing.credit_transactions
			GROUP BY tenant_id, owner_id, credit_type_id
		) AS l ON l.tenant_id = ucb.tenant_id AND l.owner_id = ucb.owner_id AND l.credit_type_id = ucb.credit_type_id`).
		Where("ucb.balance <> COALESCE(l.ledger_sum, 0)").
		Scan(ctx, &rows); err != nil {
		return 0, fmt.Errorf("scan balance drift: %w", err)
	}

	for i := range rows {
		r := rows[i]
		tx, err := w.bun().BeginTx(ctx, nil)
		if err != nil {
			return i, err
		}
		if err := w.persistEvent(ctx, tx, models.ReconciliationEvent{
			TenantID:      ptrUUID(r.TenantID),
			OwnerID:       ptrUUID(r.OwnerID),
			CreditTypeID:  ptrUUID(r.CreditTypeID),
			Kind:          models.ReconciliationBalanceDrift,
			SubjectID:     r.BalanceID.String(),
			ExpectedValue: strconv.FormatInt(r.LedgerSum, 10),
			ObservedValue: strconv.FormatInt(r.Balance, 10),
			DetectedAt:    now,
		}, false, now); err != nil {
			_ = tx.Rollback()
			return i, err
		}
		if err := tx.Commit(); err != nil {
			return i, err
		}
		w.emit(ctx, credits.ReconciliationEvent{
			Kind:         credits.ReconBalanceDrift,
			TenantID:     ptrUUID(r.TenantID),
			OwnerID:      ptrUUID(r.OwnerID),
			CreditTypeID: ptrUUID(r.CreditTypeID),
			UserID:       r.UserID,
			SubjectID:    r.BalanceID.String(),
			Expected:     r.LedgerSum,
			Observed:     r.Balance,
			Remediated:   false,
			DetectedAt:   now,
		})
	}
	return len(rows), nil
}

// ReconcileSettlements is the CROSS-SYSTEM drift INTERFACE (issue #243). The
// host supplies the day's expected settlements (derived from Tensorhub
// endpoint_billing_events — data NOT in this repo) and this diffs them against
// the OpenRails credit_transactions ledger, recording + emitting:
//
//   - missing_settlement: an expected settlement whose SourceID has NO matching
//     captured hold or withdrawal in the ledger (held-but-never-settled / lost
//     settle call); and
//   - unexpected_capture: a ledger capture/withdrawal SourceID with NO expected
//     settlement feeding it (double-charge candidate).
//
// It is ALERT-ONLY — it never mutates the ledger. The host (gen-orchestrator /
// processor-sync #107) owns the Tensorhub feed and decides remediation. Scope
// the diff with tenantID/ownerID/creditTypeID so an owner's reconciliation never
// leaks another owner's ledger. Idempotent via reconciliation_events dedupe.
func (w BillingReconciliationWorker) ReconcileSettlements(
	ctx context.Context,
	tenantID, ownerID, creditTypeID uuid.UUID,
	expected []credits.ExpectedSettlement,
	now time.Time,
) (missing, unexpected int, err error) {
	if now.IsZero() {
		if w.Clock != nil {
			now = w.Clock.Now().UTC()
		} else {
			now = time.Now().UTC()
		}
	}

	// Pull the ledger's settling rows for this scope keyed by source_id: captured
	// holds and withdrawals are the terminal spends a settlement must match.
	type ledgerRow struct {
		SourceID string `bun:"source_id"`
		Amount   int64  `bun:"amount"`
	}
	var ledger []ledgerRow
	if err := w.bun().NewSelect().
		Model((*models.CreditTransaction)(nil)).
		ColumnExpr("source_id, amount").
		Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, creditTypeID).
		Where("source_id IS NOT NULL").
		Where("(transaction_type = 'withdrawal') OR (transaction_type = 'hold' AND status = 'captured')").
		Scan(ctx, &ledger); err != nil {
		return 0, 0, fmt.Errorf("load ledger settlements: %w", err)
	}
	ledgerBySource := make(map[string]int64, len(ledger))
	for _, l := range ledger {
		if l.SourceID == "" {
			continue
		}
		ledgerBySource[l.SourceID] = l.Amount
	}

	expectedBySource := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		if e.SourceID == "" {
			continue
		}
		expectedBySource[e.SourceID] = struct{}{}
		if _, ok := ledgerBySource[e.SourceID]; ok {
			continue
		}
		// Expected but not settled.
		ev := models.ReconciliationEvent{
			TenantID:      ptrUUID(tenantID),
			OwnerID:       ptrUUID(ownerID),
			CreditTypeID:  ptrUUID(creditTypeID),
			Kind:          models.ReconciliationMissingSettlement,
			SubjectID:     e.SourceID,
			ExpectedValue: strconv.FormatInt(e.Amount, 10),
			ObservedValue: "",
			DetectedAt:    now,
		}
		tx, txErr := w.bun().BeginTx(ctx, nil)
		if txErr != nil {
			return missing, unexpected, txErr
		}
		if pErr := w.persistEvent(ctx, tx, ev, false, now); pErr != nil {
			_ = tx.Rollback()
			return missing, unexpected, pErr
		}
		if cErr := tx.Commit(); cErr != nil {
			return missing, unexpected, cErr
		}
		w.emit(ctx, credits.ReconciliationEvent{
			Kind:         credits.ReconMissingSettlement,
			TenantID:     ptrUUID(tenantID),
			OwnerID:      ptrUUID(ownerID),
			CreditTypeID: ptrUUID(creditTypeID),
			UserID:       e.UserID,
			SubjectID:    e.SourceID,
			Expected:     e.Amount,
			DetectedAt:   now,
		})
		missing++
	}

	for source, amount := range ledgerBySource {
		if _, ok := expectedBySource[source]; ok {
			continue
		}
		// Settled but not expected — double-charge candidate.
		tx, txErr := w.bun().BeginTx(ctx, nil)
		if txErr != nil {
			return missing, unexpected, txErr
		}
		if pErr := w.persistEvent(ctx, tx, models.ReconciliationEvent{
			TenantID:      ptrUUID(tenantID),
			OwnerID:       ptrUUID(ownerID),
			CreditTypeID:  ptrUUID(creditTypeID),
			Kind:          models.ReconciliationUnexpectedCapture,
			SubjectID:     source,
			ObservedValue: strconv.FormatInt(amount, 10),
			DetectedAt:    now,
		}, false, now); pErr != nil {
			_ = tx.Rollback()
			return missing, unexpected, pErr
		}
		if cErr := tx.Commit(); cErr != nil {
			return missing, unexpected, cErr
		}
		w.emit(ctx, credits.ReconciliationEvent{
			Kind:         credits.ReconUnexpectedCapture,
			TenantID:     ptrUUID(tenantID),
			OwnerID:      ptrUUID(ownerID),
			CreditTypeID: ptrUUID(creditTypeID),
			SubjectID:    source,
			Observed:     amount,
			DetectedAt:   now,
		})
		unexpected++
	}
	return missing, unexpected, nil
}

// persistEvent inserts an alert-first reconciliation_events row, deduping on the
// open (tenant, owner, credit_type, kind, subject_id) uniqueness so reruns are
// idempotent. An existing open row is updated in place (refreshing values /
// remediation state) rather than duplicated. db is the active tx.
func (w BillingReconciliationWorker) persistEvent(ctx context.Context, tx bun.Tx, ev models.ReconciliationEvent, remediated bool, now time.Time) error {
	if remediated {
		ev.RemediatedAt = &now
	}
	if ev.ID == uuid.Nil {
		ev.ID = uuidutil.NewV7()
	}
	if ev.DetectedAt.IsZero() {
		ev.DetectedAt = now
	}

	// Look for an already-open row with the same scope+kind+subject.
	exists, err := tx.NewSelect().
		Model((*models.ReconciliationEvent)(nil)).
		Where("kind = ?", ev.Kind).
		Where("COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE(?::uuid, '00000000-0000-0000-0000-000000000000'::uuid)", ev.TenantID).
		Where("COALESCE(owner_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE(?::uuid, '00000000-0000-0000-0000-000000000000'::uuid)", ev.OwnerID).
		Where("COALESCE(credit_type_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE(?::uuid, '00000000-0000-0000-0000-000000000000'::uuid)", ev.CreditTypeID).
		Where("COALESCE(subject_id, '') = COALESCE(?, '')", ev.SubjectID).
		Where("resolved_at IS NULL").
		Exists(ctx)
	if err != nil {
		return fmt.Errorf("dedupe reconciliation event: %w", err)
	}
	if exists {
		_, err := tx.NewUpdate().
			Model((*models.ReconciliationEvent)(nil)).
			Set("expected_value = ?", ev.ExpectedValue).
			Set("observed_value = ?", ev.ObservedValue).
			Set("remediated_at = ?", ev.RemediatedAt).
			Set("detected_at = ?", ev.DetectedAt).
			Where("kind = ?", ev.Kind).
			Where("COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE(?::uuid, '00000000-0000-0000-0000-000000000000'::uuid)", ev.TenantID).
			Where("COALESCE(owner_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE(?::uuid, '00000000-0000-0000-0000-000000000000'::uuid)", ev.OwnerID).
			Where("COALESCE(credit_type_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE(?::uuid, '00000000-0000-0000-0000-000000000000'::uuid)", ev.CreditTypeID).
			Where("COALESCE(subject_id, '') = COALESCE(?, '')", ev.SubjectID).
			Where("resolved_at IS NULL").
			Exec(ctx)
		return err
	}
	if _, err := tx.NewInsert().Model(&ev).Exec(ctx); err != nil {
		return fmt.Errorf("insert reconciliation event: %w", err)
	}
	return nil
}

// emit best-effort delivers a reconciliation signal to the sink. A sink error is
// logged but never rolls back a persisted event or a completed remediation.
func (w BillingReconciliationWorker) emit(ctx context.Context, ev credits.ReconciliationEvent) {
	if w.Sink == nil {
		return
	}
	if err := w.Sink.Handle(ctx, ev); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"worker": KindBillingReconciliation,
			"kind":   string(ev.Kind),
		}).Error("reconciliation sink failed (event already persisted)")
	}
}

func ptrUUID(u uuid.UUID) *uuid.UUID {
	if u == uuid.Nil {
		return nil
	}
	v := u
	return &v
}
