package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
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
// the reservation event (live reservation state is on openrails.credit_windows).
const (
	windowOpenSource   = "window"
	windowSettleSource = "window_settle"
	txWindowOpen       = "window_open"
	txWindowRefill     = "window_refill"
)

// OpenWindowParams opens a prepaid window for a payer.
type OpenWindowParams struct {
	Payer     identity.CustomerID
	Invoker   string // attribution; stamped on the ledger record
	Currency  string
	Amount    int64
	ExpiresAt time.Time
}

// creditWindowFromGen maps a generated window row onto the domain model.
func creditWindowFromGen(r gen.OpenrailsMoneyWindow) *models.MoneyWindow {
	return &models.MoneyWindow{
		ID:            r.ID,
		MerchantID:    r.MerchantID,
		CustomerID:    r.CustomerID,
		Currency:      r.Currency,
		HeldAmount:    r.HeldAmount,
		SettledAmount: r.SettledAmount,
		Status:        r.Status,
		ExpiresAt:     r.ExpiresAt,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

// OpenWindow reserves Amount from the payer's available balance into a new
// open window — the same balance/held mechanics as Hold, in ONE transaction:
// the balance row is locked, available (balance - held) is checked, and
// held_balance is bumped. There is NO optimistic approval: a payer without the
// available funds gets ErrInsufficientCredits, exactly like Hold.
func (s *MoneyService) OpenWindow(ctx context.Context, p OpenWindowParams) (*models.MoneyWindow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
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
	cur := normalizeCurrency(p.Currency)
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}

	var window *models.MoneyWindow
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		now := s.now()
		tid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		tenantID := tid.UUID()
		payerID := p.Payer.UUID()

		// Lock + derive under the customers-row lock; the open window row IS the
		// held reservation (#491): SumActiveMoneyHeld counts open windows' unsettled.
		bal, err := s.lockBalance(ctx, q, p.Payer, p.Invoker, cur)
		if err != nil {
			return err
		}
		if bal.Balance-bal.HeldBalance < p.Amount {
			return ErrInsufficientCredits
		}

		exp := p.ExpiresAt.UTC()
		w := &models.MoneyWindow{
			ID:            uuidutil.NewV7(),
			MerchantID:    tenantID,
			CustomerID:    payerID,
			Currency:      cur,
			HeldAmount:    p.Amount,
			SettledAmount: 0,
			Status:        "open",
			ExpiresAt:     exp,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := q.InsertMoneyWindow(ctx, gen.InsertMoneyWindowParams{
			ID: w.ID, MerchantID: w.MerchantID, CustomerID: w.CustomerID, Currency: w.Currency,
			HeldAmount: w.HeldAmount, SettledAmount: w.SettledAmount,
			Status: w.Status, ExpiresAt: w.ExpiresAt, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
		}); err != nil {
			return err
		}
		// The open window row IS the reservation record (#335); the prior
		// zero-amount money_transactions audit row is gone with the hard cut.
		window = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	return window, nil
}

// windowActor returns the ledger/usage attribution invoker: the caller-supplied
// invoker when present, else the payer id string (the AccrueOwed convention).
func windowActor(invoker string, payerID uuid.UUID) string {
	if a := strings.TrimSpace(invoker); a != "" {
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
	// Invoker is optional attribution for the ledger/usage rows; defaults to the
	// window's payer id.
	Invoker   string
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
func (s *MoneyService) SettleWindowItems(ctx context.Context, items []WindowSettleItem) ([]WindowSettleResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	out := make([]WindowSettleResult, 0, len(items))
	for i := range items {
		out = append(out, s.settleOneWindowItem(ctx, items[i]))
	}
	return out, nil
}

func (s *MoneyService) settleOneWindowItem(ctx context.Context, item WindowSettleItem) WindowSettleResult {
	res := WindowSettleResult{WindowID: item.WindowID, RequestID: strings.TrimSpace(item.RequestID)}
	if item.WindowID == uuid.Nil || res.RequestID == "" || item.Amount <= 0 {
		res.ErrorCode = "invalid_item"
		return res
	}

	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		now := s.now()

		wRow, err := q.GetMoneyWindowForUpdate(ctx, item.WindowID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrWindowNotFound
			}
			return err
		}
		w := creditWindowFromGen(wRow)
		payer := identity.CustomerID(w.CustomerID)
		invoker := windowActor(item.Invoker, w.CustomerID)
		cur := normalizeCurrency(w.Currency)

		// Replay check FIRST (under the window lock) so a re-sent batch returns
		// success even after the window has since closed/expired. The settle is a
		// #512 credit_spend transfer keyed on (source=window_settle, request_id).
		existing, derr := q.GetLedgerTransferByCoords(ctx, gen.GetLedgerTransferByCoordsParams{
			MerchantID: w.MerchantID, CustomerID: w.CustomerID, Currency: cur,
			TransferType: "credit_spend", Source: windowSettleSource, SourceID: res.RequestID,
		})
		if derr == nil {
			id := existing.ID
			res.OK, res.Replayed, res.TransactionID = true, true, &id
			return nil
		}
		if !errors.Is(derr, pgx.ErrNoRows) {
			return derr
		}

		if w.Status != "open" {
			return ErrWindowNotOpen
		}
		if w.SettledAmount+item.Amount > w.HeldAmount {
			return ErrWindowExceeded
		}

		// Capture mechanics: bumping settled_amount releases this slice of the
		// window reservation from the derived held (#491); withdrawBalanceAndBlocks
		// spends the FIFO credit lots for the actual amount, tagged with the
		// request_id (idempotency). Lock the customers row first.
		if _, err := s.lockBalance(ctx, q, payer, invoker, cur); err != nil {
			return err
		}
		if err := q.AddMoneyWindowSettled(ctx, gen.AddMoneyWindowSettledParams{
			ID: w.ID, Amount: item.Amount, UpdatedAt: now,
		}); err != nil {
			return err
		}
		if _, err := s.withdrawBalanceAndBlocks(ctx, q, payer, invoker, cur, windowSettleSource, res.RequestID, strings.TrimSpace(item.Resource), item.Amount); err != nil {
			return err
		}
		// Fetch the settle transfer id (the first lot debit at these coordinates)
		// for the result + the usage-event link.
		settleTr, err := q.GetLedgerSpendByCoords(ctx, gen.GetLedgerSpendByCoordsParams{
			MerchantID: w.MerchantID, CustomerID: w.CustomerID, Currency: cur,
			Source: windowSettleSource, SourceID: res.RequestID,
		})
		if err != nil {
			return err
		}

		// Usage analytics (#311-style): append a usage_event linked to the settle
		// debit, in the SAME tx (replays never reach here, so no duplicates).
		if et := strings.TrimSpace(item.EventType); et != "" {
			meta, jerr := toJSONBC(item.Metadata)
			if jerr != nil {
				return jerr
			}
			if err := q.InsertUsageEvent(ctx, gen.InsertUsageEventParams{
				ID:                 uuidutil.NewV7(),
				MerchantID:         w.MerchantID,
				CustomerID:         w.CustomerID,
				InvokerID:          invoker,
				Resource:           nilIfEmpty(strings.TrimSpace(item.Resource)),
				EventType:          et,
				Amount:             item.Amount,
				Source:             windowSettleSource,
				SourceID:           res.RequestID,
				MoneyTransactionID: &settleTr.ID,
				Metadata:           meta,
				OccurredAt:         now,
				CreatedAt:          now,
			}); err != nil {
				return err
			}
		}

		res.OK, res.TransactionID = true, &settleTr.ID
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
func (s *MoneyService) RefillWindow(ctx context.Context, windowID uuid.UUID, amount int64, extendTo time.Time) (*models.MoneyWindow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
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

	var window *models.MoneyWindow
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		now := s.now()

		wRow, err := q.GetMoneyWindowForUpdate(ctx, windowID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrWindowNotFound
			}
			return err
		}
		w := creditWindowFromGen(wRow)
		if w.Status != "open" {
			return ErrWindowNotOpen
		}
		payer := identity.CustomerID(w.CustomerID)
		invoker := w.CustomerID.String()
		cur := normalizeCurrency(w.Currency)

		if amount > 0 {
			// Bumping the window's held_amount (below) raises its derived-held
			// contribution (#491); just gate on available under the lock.
			bal, err := s.lockBalance(ctx, q, payer, invoker, cur)
			if err != nil {
				return err
			}
			if bal.Balance-bal.HeldBalance < amount {
				return ErrInsufficientCredits
			}
			w.HeldAmount += amount
		}
		if !extendTo.IsZero() {
			w.ExpiresAt = extendTo.UTC()
		}
		w.UpdatedAt = now
		if err := q.UpdateMoneyWindowReservation(ctx, gen.UpdateMoneyWindowReservationParams{
			ID: w.ID, HeldAmount: w.HeldAmount, ExpiresAt: w.ExpiresAt, UpdatedAt: w.UpdatedAt,
		}); err != nil {
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

// CloseWindow releases an open window's unsettled remainder back to available
// balance and marks it closed. Idempotent: closing an already closed/expired
// window is a no-op returning the current row.
func (s *MoneyService) CloseWindow(ctx context.Context, windowID uuid.UUID) (*models.MoneyWindow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if windowID == uuid.Nil {
		return nil, fmt.Errorf("window_id required")
	}

	var window *models.MoneyWindow
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		now := s.now()

		wRow, err := q.GetMoneyWindowForUpdate(ctx, windowID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrWindowNotFound
			}
			return err
		}
		w := creditWindowFromGen(wRow)
		if w.Status != "open" {
			window = w // already closed/expired: remainder already released
			return nil
		}
		payer := identity.CustomerID(w.CustomerID)
		cur := normalizeCurrency(w.Currency)

		// Marking the window closed drops its whole unsettled remainder from the
		// derived held (#491) — no cache write; lock to serialize concurrent spend.
		if _, err := s.lockBalance(ctx, q, payer, w.CustomerID.String(), cur); err != nil {
			return err
		}
		w.Status = "closed"
		w.UpdatedAt = now
		if err := q.SetMoneyWindowStatus(ctx, gen.SetMoneyWindowStatusParams{
			ID: w.ID, Status: w.Status, UpdatedAt: w.UpdatedAt,
		}); err != nil {
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

// GetWindow returns one window by id (RLS/merchant-scoped).
func (s *MoneyService) GetWindow(ctx context.Context, windowID uuid.UUID) (*models.MoneyWindow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	var w *models.MoneyWindow
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, e := s.db.Gen(ctx).GetMoneyWindow(ctx, windowID)
		if e != nil {
			return e
		}
		w = creditWindowFromGen(row)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWindowNotFound
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

// ExpireWindows releases the unsettled remainder of every open window whose
// expires_at has passed and marks it expired — the window analogue of the hold
// expiry sweep, run from the same periodic HoldExpiryWorker job. Cross-merchant
// (sweeps the whole table, like hold expiry). Returns the number expired.
func (s *MoneyService) ExpireWindows(ctx context.Context, batchSize int) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	now := s.now()
	total := 0

	for {
		batch := 0
		// Privileged (no-GUC) transaction per batch: cross-merchant sweep with
		// explicit merchant_id predicates, like the hold expiry worker.
		err := s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			batchSize32, _ := safecast.Convert[int32](batchSize)
			windows, err := q.ListExpiredOpenMoneyWindowsForUpdate(ctx, gen.ListExpiredOpenMoneyWindowsForUpdateParams{
				Now: now, BatchSize: batchSize32,
			})
			if err != nil {
				return err
			}
			batch = len(windows)
			if batch == 0 {
				return nil
			}

			// Held is derived (#491): marking a window expired drops its unsettled
			// remainder from SumActiveMoneyHeld automatically — no cache to update.
			for i := range windows {
				if err := q.SetMoneyWindowStatus(ctx, gen.SetMoneyWindowStatusParams{
					ID: windows[i].ID, Status: "expired", UpdatedAt: now,
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		if batch == 0 {
			break
		}
		total += batch
		if batch < batchSize {
			break
		}
	}
	return total, nil
}
