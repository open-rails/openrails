package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/uptrace/bun"
)

// Prepaid credit windows (issue #335). A window is a FIRST-CLASS bulk
// reservation: opening one reserves funds exactly like Hold (available check +
// held_balance bump + ledger row), but unlike a hold — which settles exactly
// once via CaptureHold — a window is settled incrementally, per request, until
// it is closed or expires. The unsettled remainder is released then.
var (
	ErrWindowNotFound = errors.New("window_not_found")
	ErrWindowNotOpen  = errors.New("window_not_open")
	// ErrWindowExceeded is returned when a settle would push the window's
	// settled total past its held amount (sum(settles) <= held, server-enforced).
	ErrWindowExceeded = errors.New("window_exceeded")
)

// Ledger coordinates for window rows. Settles are ordinary withdrawals (so
// spend caps + invoices see them) idempotency-keyed on
// (source='window_settle', source_id=request_id) — the same dedup shape as
// capture's request_id key. Open/refill rows are zero-amount audit records of
// the reservation event (live reservation state is on billing.credit_windows).
const (
	windowOpenSource   = "window"
	windowSettleSource = "window_settle"
	txWindowOpen       = "window_open"
	txWindowRefill     = "window_refill"
)

// OpenWindowParams opens a prepaid window for a payer.
type OpenWindowParams struct {
	Payer      identity.TenantSubjectID
	Actor      string // attribution; stamped on the ledger record
	CreditType string
	Amount     int64
	ExpiresAt  time.Time
}

// OpenWindow reserves Amount from the payer's available balance into a new
// open window — the same balance/held mechanics as Hold, in ONE transaction:
// the balance row is locked, available (balance - held) is checked, and
// held_balance is bumped. There is NO optimistic approval: a payer without the
// available funds gets ErrInsufficientCredits, exactly like Hold.
func (s *CreditsService) OpenWindow(ctx context.Context, p OpenWindowParams) (*models.CreditWindow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if p.Payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if p.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	if p.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("expires_at required")
	}
	p.CreditType = strings.TrimSpace(p.CreditType)
	if p.CreditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}

	var window *models.CreditWindow
	err := s.db.RunInTenantTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		txSvc := &CreditsService{db: db.NewWithTx(tx), clock: s.clock}
		ct, err := txSvc.GetCreditTypeByName(ctx, p.CreditType)
		if err != nil {
			return err
		}
		if !ct.IsActive {
			return ErrCreditTypeInactive
		}

		now := s.now()
		tenantID := tenant.FromContextOrDefault(ctx).UUID()
		payerID := p.Payer.UUID()
		if err := ensureTenantSubject(ctx, tx, tenantID, payerID); err != nil {
			return err
		}

		bal, err := txSvc.lockBalance(ctx, tx, p.Payer, p.Actor, ct.ID)
		if err != nil {
			return err
		}
		if bal.Balance-bal.HeldBalance < p.Amount {
			return ErrInsufficientCredits
		}
		if _, err := tx.NewUpdate().Model((*models.CreditBalance)(nil)).
			Set("held_balance = ?", bal.HeldBalance+p.Amount).
			Set("updated_at = ?", now).
			Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, payerID, ct.ID).
			Exec(ctx); err != nil {
			return err
		}

		exp := p.ExpiresAt.UTC()
		w := &models.CreditWindow{
			ID:              uuidutil.NewV7(),
			TenantID:        tenantID,
			TenantSubjectID: payerID,
			CreditTypeID:    ct.ID,
			HeldAmount:      p.Amount,
			SettledAmount:   0,
			Status:          "open",
			ExpiresAt:       exp,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if _, err := tx.NewInsert().Model(w).Exec(ctx); err != nil {
			return err
		}
		if err := insertWindowRecordTx(ctx, tx, w, txWindowOpen, windowActor(p.Actor, payerID), p.Amount, now); err != nil {
			return err
		}
		window = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	return window, nil
}

// insertWindowRecordTx writes the zero-amount audit ledger row for a window
// open/refill (amount=0 — no balance movement; authorized records the reserved
// delta; source_id is the window id).
func insertWindowRecordTx(ctx context.Context, tx bun.Tx, w *models.CreditWindow, txType, actor string, amount int64, now time.Time) error {
	auth := amount
	exp := w.ExpiresAt
	srcID := w.ID.String()
	rec := &models.CreditTransaction{
		ID:              uuidutil.NewV7(),
		TenantID:        w.TenantID,
		TenantSubjectID: w.TenantSubjectID,
		Actor:           actor,
		CreditTypeID:    w.CreditTypeID,
		Amount:          0,
		TransactionType: txType,
		Status:          "posted",
		Authorized:      &auth,
		Source:          windowOpenSource,
		SourceID:        &srcID,
		ExpiresAt:       &exp,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	_, err := tx.NewInsert().Model(rec).Exec(ctx)
	return err
}

// windowActor returns the ledger/usage attribution actor: the caller-supplied
// actor when present, else the payer id string (the AccrueOwed convention).
func windowActor(actor string, payerID uuid.UUID) string {
	if a := strings.TrimSpace(actor); a != "" {
		return a
	}
	return payerID.String()
}

// WindowSettleItem is one settled actual: RequestID is the idempotency key
// (unique ledger row on source='window_settle'); the usage fields, when
// EventType is set, also append a usage_event for analytics (no second debit).
// Items in one batch may span windows AND payers (cross-payer settle, #335).
type WindowSettleItem struct {
	WindowID  uuid.UUID
	RequestID string
	Amount    int64
	// Actor is optional attribution for the ledger/usage rows; defaults to the
	// window's payer id.
	Actor     string
	EventType string
	Resource  string
	Metadata  map[string]any
}

// WindowSettleResult is the per-item outcome. Idempotent replays return OK with
// Replayed=true. ErrorCode is one of window_not_found | window_not_open |
// window_exceeded | invalid_item | internal_error.
type WindowSettleResult struct {
	WindowID      uuid.UUID  `json:"window_id"`
	RequestID     string     `json:"request_id"`
	OK            bool       `json:"ok"`
	Replayed      bool       `json:"replayed,omitempty"`
	ErrorCode     string     `json:"error,omitempty"`
	TransactionID *uuid.UUID `json:"transaction_id,omitempty"`
}

// SettleWindowItems settles a batch of actuals against their windows. Each item
// runs in its OWN transaction so one item's failure never poisons the rest
// (cross-payer batches mix independent payers). Per item, atomically:
//   - the window row is locked (serializing concurrent settles per window),
//   - the replay check runs (existing ledger row for source='window_settle',
//     source_id=request_id → success, no second charge — capture-style dedup),
//   - sum(settled) <= held is enforced (ErrWindowExceeded),
//   - held_balance and balance both decrement by Amount (capture mechanics),
//   - the withdrawal ledger row + optional usage_event are written.
func (s *CreditsService) SettleWindowItems(ctx context.Context, items []WindowSettleItem) ([]WindowSettleResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	out := make([]WindowSettleResult, 0, len(items))
	for i := range items {
		out = append(out, s.settleOneWindowItem(ctx, items[i]))
	}
	return out, nil
}

func (s *CreditsService) settleOneWindowItem(ctx context.Context, item WindowSettleItem) WindowSettleResult {
	res := WindowSettleResult{WindowID: item.WindowID, RequestID: strings.TrimSpace(item.RequestID)}
	if item.WindowID == uuid.Nil || res.RequestID == "" || item.Amount <= 0 {
		res.ErrorCode = "invalid_item"
		return res
	}

	err := s.db.RunInTenantTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		txSvc := &CreditsService{db: db.NewWithTx(tx), clock: s.clock}
		now := s.now()

		w := new(models.CreditWindow)
		if err := tx.NewSelect().Model(w).Where("id = ?", item.WindowID).For("UPDATE").Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrWindowNotFound
			}
			return err
		}
		payer := identity.TenantSubjectID(w.TenantSubjectID)
		actor := windowActor(item.Actor, w.TenantSubjectID)

		// Replay check FIRST (under the window lock) so a re-sent batch returns
		// success even after the window has since closed/expired.
		existing := new(models.CreditTransaction)
		derr := tx.NewSelect().Model(existing).
			Where("tenant_id = ? AND tenant_subject_id = ?", w.TenantID, w.TenantSubjectID).
			Where("credit_type_id = ?", w.CreditTypeID).
			Where("transaction_type = 'withdrawal'").
			Where("source = ? AND source_id = ?", windowSettleSource, res.RequestID).
			Limit(1).Scan(ctx)
		if derr == nil {
			res.OK, res.Replayed, res.TransactionID = true, true, &existing.ID
			return nil
		}
		if !errors.Is(derr, sql.ErrNoRows) {
			return derr
		}

		if w.Status != "open" {
			return ErrWindowNotOpen
		}
		if w.SettledAmount+item.Amount > w.HeldAmount {
			return ErrWindowExceeded
		}

		// Capture mechanics: release this slice of the reservation, then debit the
		// balance/FIFO blocks for it. Both target the payer's balance row.
		bal, err := txSvc.lockBalance(ctx, tx, payer, actor, w.CreditTypeID)
		if err != nil {
			return err
		}
		newHeld := bal.HeldBalance - item.Amount
		if newHeld < 0 {
			newHeld = 0
		}
		if _, err := tx.NewUpdate().Model((*models.CreditBalance)(nil)).
			Set("held_balance = ?", newHeld).
			Set("updated_at = ?", now).
			Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", w.TenantID, w.TenantSubjectID, w.CreditTypeID).
			Exec(ctx); err != nil {
			return err
		}
		newBal, err := txSvc.withdrawBalanceAndBlocks(ctx, tx, payer, actor, w.CreditTypeID, item.Amount)
		if err != nil {
			return err
		}

		if _, err := tx.NewUpdate().Model((*models.CreditWindow)(nil)).
			Set("settled_amount = settled_amount + ?", item.Amount).
			Set("updated_at = ?", now).
			Where("id = ?", w.ID).
			Exec(ctx); err != nil {
			return err
		}

		srcID := res.RequestID
		trx := &models.CreditTransaction{
			ID:              uuidutil.NewV7(),
			TenantID:        w.TenantID,
			TenantSubjectID: w.TenantSubjectID,
			Actor:           actor,
			Resource:        nilIfEmpty(strings.TrimSpace(item.Resource)),
			CreditTypeID:    w.CreditTypeID,
			Amount:          -item.Amount,
			BalanceAfter:    &newBal,
			TransactionType: "withdrawal",
			Status:          "posted",
			Source:          windowSettleSource,
			SourceID:        &srcID,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if _, err := tx.NewInsert().Model(trx).Exec(ctx); err != nil {
			return err
		}

		// Usage analytics (#311-style): append a usage_event linked to the settle
		// debit, in the SAME tx (replays never reach here, so no duplicates).
		if et := strings.TrimSpace(item.EventType); et != "" {
			ev := &models.UsageEvent{
				ID:                  uuidutil.NewV7(),
				TenantID:            w.TenantID,
				TenantSubjectID:     w.TenantSubjectID,
				Actor:               actor,
				Resource:            nilIfEmpty(strings.TrimSpace(item.Resource)),
				CreditTypeID:        w.CreditTypeID,
				EventType:           et,
				Amount:              item.Amount,
				Source:              windowSettleSource,
				SourceID:            res.RequestID,
				CreditTransactionID: &trx.ID,
				Metadata:            item.Metadata,
				OccurredAt:          now,
				CreatedAt:           now,
			}
			if _, err := tx.NewInsert().Model(ev).Exec(ctx); err != nil {
				return err
			}
		}

		res.OK, res.TransactionID = true, &trx.ID
		return nil
	})
	if err != nil {
		res.OK, res.Replayed, res.TransactionID = false, false, nil
		res.ErrorCode = windowErrCode(err)
	}
	return res
}

// windowErrCode maps window errors to stable per-item codes.
func windowErrCode(err error) string {
	switch {
	case errors.Is(err, ErrWindowNotFound):
		return "window_not_found"
	case errors.Is(err, ErrWindowNotOpen):
		return "window_not_open"
	case errors.Is(err, ErrWindowExceeded):
		return "window_exceeded"
	case errors.Is(err, ErrInsufficientCredits):
		return "insufficient_credits"
	default:
		return "internal_error"
	}
}

// RefillWindow extends an open window: amount > 0 reserves more funds (same
// available-balance gate as OpenWindow — no optimistic approval), and a
// non-zero extendTo pushes expires_at out. A closed/expired window cannot be
// refilled (ErrWindowNotOpen) — the caller opens a new window instead.
func (s *CreditsService) RefillWindow(ctx context.Context, windowID uuid.UUID, amount int64, extendTo time.Time) (*models.CreditWindow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if windowID == uuid.Nil {
		return nil, fmt.Errorf("window_id required")
	}
	if amount < 0 {
		return nil, fmt.Errorf("amount must be >= 0")
	}
	if amount == 0 && extendTo.IsZero() {
		return nil, fmt.Errorf("amount or ttl required")
	}

	var window *models.CreditWindow
	err := s.db.RunInTenantTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		txSvc := &CreditsService{db: db.NewWithTx(tx), clock: s.clock}
		now := s.now()

		w := new(models.CreditWindow)
		if err := tx.NewSelect().Model(w).Where("id = ?", windowID).For("UPDATE").Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrWindowNotFound
			}
			return err
		}
		if w.Status != "open" {
			return ErrWindowNotOpen
		}
		payer := identity.TenantSubjectID(w.TenantSubjectID)
		actor := w.TenantSubjectID.String()

		if amount > 0 {
			bal, err := txSvc.lockBalance(ctx, tx, payer, actor, w.CreditTypeID)
			if err != nil {
				return err
			}
			if bal.Balance-bal.HeldBalance < amount {
				return ErrInsufficientCredits
			}
			if _, err := tx.NewUpdate().Model((*models.CreditBalance)(nil)).
				Set("held_balance = ?", bal.HeldBalance+amount).
				Set("updated_at = ?", now).
				Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", w.TenantID, w.TenantSubjectID, w.CreditTypeID).
				Exec(ctx); err != nil {
				return err
			}
			w.HeldAmount += amount
		}
		if !extendTo.IsZero() {
			w.ExpiresAt = extendTo.UTC()
		}
		w.UpdatedAt = now
		if _, err := tx.NewUpdate().Model(w).
			Column("held_amount", "expires_at", "updated_at").
			WherePK().
			Exec(ctx); err != nil {
			return err
		}
		if amount > 0 {
			if err := insertWindowRecordTx(ctx, tx, w, txWindowRefill, actor, amount, now); err != nil {
				return err
			}
		}
		window = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	return window, nil
}

// CloseWindow releases an open window's unsettled remainder back to available
// balance and marks it closed. Idempotent: closing an already closed/expired
// window is a no-op returning the current row.
func (s *CreditsService) CloseWindow(ctx context.Context, windowID uuid.UUID) (*models.CreditWindow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if windowID == uuid.Nil {
		return nil, fmt.Errorf("window_id required")
	}

	var window *models.CreditWindow
	err := s.db.RunInTenantTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		txSvc := &CreditsService{db: db.NewWithTx(tx), clock: s.clock}
		now := s.now()

		w := new(models.CreditWindow)
		if err := tx.NewSelect().Model(w).Where("id = ?", windowID).For("UPDATE").Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrWindowNotFound
			}
			return err
		}
		if w.Status != "open" {
			window = w // already closed/expired: remainder already released
			return nil
		}
		payer := identity.TenantSubjectID(w.TenantSubjectID)

		if remainder := w.HeldAmount - w.SettledAmount; remainder > 0 {
			bal, err := txSvc.lockBalance(ctx, tx, payer, w.TenantSubjectID.String(), w.CreditTypeID)
			if err != nil {
				return err
			}
			newHeld := bal.HeldBalance - remainder
			if newHeld < 0 {
				newHeld = 0
			}
			if _, err := tx.NewUpdate().Model((*models.CreditBalance)(nil)).
				Set("held_balance = ?", newHeld).
				Set("updated_at = ?", now).
				Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", w.TenantID, w.TenantSubjectID, w.CreditTypeID).
				Exec(ctx); err != nil {
				return err
			}
		}
		w.Status = "closed"
		w.UpdatedAt = now
		if _, err := tx.NewUpdate().Model(w).
			Column("status", "updated_at").
			WherePK().
			Exec(ctx); err != nil {
			return err
		}
		window = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	return window, nil
}

// GetWindow returns one window by id (RLS/tenant-scoped).
func (s *CreditsService) GetWindow(ctx context.Context, windowID uuid.UUID) (*models.CreditWindow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	w := new(models.CreditWindow)
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		return s.db.Q(ctx).NewSelect().Model(w).Where("id = ?", windowID).Limit(1).Scan(ctx)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWindowNotFound
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

// ExpireWindows releases the unsettled remainder of every open window whose
// expires_at has passed and marks it expired — the window analogue of the hold
// expiry sweep, run from the same periodic HoldExpiryWorker job. Cross-tenant
// (sweeps the whole table, like hold expiry). Returns the number expired.
func (s *CreditsService) ExpireWindows(ctx context.Context, batchSize int) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("credits service not initialized")
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	bunDB, ok := s.db.GetDB().(*bun.DB)
	if !ok {
		return 0, fmt.Errorf("expire windows: requires a root *bun.DB (cross-tenant sweep)")
	}
	now := s.now()
	total := 0

	for {
		tx, err := bunDB.BeginTx(ctx, nil)
		if err != nil {
			return total, err
		}

		var windows []models.CreditWindow
		if err := tx.NewSelect().Model(&windows).
			Where("status = 'open' AND expires_at <= ?", now).
			OrderExpr("expires_at ASC").
			Limit(batchSize).
			For("UPDATE SKIP LOCKED").
			Scan(ctx); err != nil {
			_ = tx.Rollback()
			return total, err
		}
		if len(windows) == 0 {
			_ = tx.Rollback()
			break
		}

		// Group released remainders by balance key (the HoldExpiryWorker pattern).
		type key struct {
			TenantID        uuid.UUID
			TenantSubjectID uuid.UUID
			CreditTypeID    uuid.UUID
		}
		released := make(map[key]int64)
		for i := range windows {
			w := &windows[i]
			if rem := w.HeldAmount - w.SettledAmount; rem > 0 {
				released[key{w.TenantID, w.TenantSubjectID, w.CreditTypeID}] += rem
			}
			w.Status = "expired"
			w.UpdatedAt = now
			if _, err := tx.NewUpdate().Model(w).
				Column("status", "updated_at").
				WherePK().
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return total, err
			}
		}
		for k, amount := range released {
			bal := new(models.CreditBalance)
			if err := tx.NewSelect().Model(bal).
				Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", k.TenantID, k.TenantSubjectID, k.CreditTypeID).
				For("UPDATE").
				Scan(ctx); err != nil {
				_ = tx.Rollback()
				return total, err
			}
			newHeld := bal.HeldBalance - amount
			if newHeld < 0 {
				newHeld = 0
			}
			if _, err := tx.NewUpdate().Model((*models.CreditBalance)(nil)).
				Set("held_balance = ?", newHeld).
				Set("updated_at = ?", now).
				Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", k.TenantID, k.TenantSubjectID, k.CreditTypeID).
				Exec(ctx); err != nil {
				_ = tx.Rollback()
				return total, err
			}
		}
		if err := tx.Commit(); err != nil {
			return total, err
		}
		total += len(windows)
		if len(windows) < batchSize {
			break
		}
	}
	return total, nil
}
