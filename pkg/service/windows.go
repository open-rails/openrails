package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// Prepaid money windows (#335): one bulk reservation a host admits requests
// against locally, settled in cross-payer batches, refilled async, with the
// remainder released at close/expiry. Window errors are re-exported so HTTP
// handlers and embedded hosts match on the facade, not the internal package.
var (
	ErrWindowNotFound = money.ErrWindowNotFound
	ErrWindowNotOpen  = money.ErrWindowNotOpen
	ErrWindowExceeded = money.ErrWindowExceeded
)

// OpenWindowRequest opens a prepaid window for one payer.
type OpenWindowRequest struct {
	MerchantSubjectID identity.MerchantSubjectID
	Actor           string
	Currency        string // "" => DefaultCurrency (#476)
	Amount          int64
	TTL             time.Duration
}

// CreditWindowDTO is the public view of a window.
type CreditWindowDTO struct {
	WindowID        uuid.UUID `json:"window_id"`
	MerchantSubjectID uuid.UUID `json:"tenant_subject_id"`
	HeldAmount      int64     `json:"held_amount"`
	SettledAmount   int64     `json:"settled_amount"`
	Status          string    `json:"status"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// defaultWindowTTL bounds a window when the caller does not supply a TTL.
// Windows are refilled/extended by live callers; the expiry sweep is the
// backstop for abandoned ones.
const defaultWindowTTL = 15 * time.Minute

// OpenWindow reserves funds into a new prepaid window (hold mechanics: the
// payer's available balance is checked and held atomically — no optimistic
// approval; insufficient funds returns ErrInsufficientCredits).
func (s *Service) OpenWindow(ctx context.Context, req OpenWindowRequest) (*CreditWindowDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if req.MerchantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = defaultWindowTTL
	}
	w, err := s.moneyService().OpenWindow(ctx, money.OpenWindowParams{
		Payer:     req.MerchantSubjectID,
		Actor:     strings.TrimSpace(req.Actor),
		Currency:  req.Currency,
		Amount:    req.Amount,
		ExpiresAt: s.now().Add(ttl).UTC(),
	})
	if err != nil {
		return nil, err
	}
	return windowToDTO(w), nil
}

// WindowSettleItemInput is one settled actual. Items in a batch may span
// windows AND payers; RequestID is the per-item idempotency key.
type WindowSettleItemInput struct {
	WindowID  uuid.UUID
	RequestID string
	Amount    int64
	Actor     string
	EventType string
	Resource  string
	Metadata  map[string]any
}

// WindowSettleItemResult mirrors money.WindowSettleResult on the facade.
type WindowSettleItemResult = money.WindowSettleResult

// SettleWindowItems settles a cross-payer batch of actuals. Per-item isolation:
// each item commits or fails alone; idempotent replays return OK. The returned
// slice is positionally aligned with the input.
func (s *Service) SettleWindowItems(ctx context.Context, items []WindowSettleItemInput) ([]WindowSettleItemResult, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	in := make([]money.WindowSettleItem, 0, len(items))
	for _, it := range items {
		in = append(in, money.WindowSettleItem{
			WindowID:  it.WindowID,
			RequestID: strings.TrimSpace(it.RequestID),
			Amount:    it.Amount,
			Actor:     strings.TrimSpace(it.Actor),
			EventType: strings.TrimSpace(it.EventType),
			Resource:  strings.TrimSpace(it.Resource),
			Metadata:  it.Metadata,
		})
	}
	return s.moneyService().SettleWindowItems(ctx, in)
}

// RefillWindow extends an open window: amount > 0 reserves more funds (same
// insufficient-funds gate as open), ttl > 0 pushes expires_at to now+ttl.
func (s *Service) RefillWindow(ctx context.Context, windowID uuid.UUID, amount int64, ttl time.Duration) (*CreditWindowDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	var extendTo time.Time
	if ttl > 0 {
		extendTo = s.now().Add(ttl).UTC()
	}
	w, err := s.moneyService().RefillWindow(ctx, windowID, amount, extendTo)
	if err != nil {
		return nil, err
	}
	return windowToDTO(w), nil
}

// CloseWindow releases the window's unsettled remainder and marks it closed.
// Idempotent on already closed/expired windows.
func (s *Service) CloseWindow(ctx context.Context, windowID uuid.UUID) (*CreditWindowDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	w, err := s.moneyService().CloseWindow(ctx, windowID)
	if err != nil {
		return nil, err
	}
	return windowToDTO(w), nil
}

func windowToDTO(w *models.MoneyWindow) *CreditWindowDTO {
	return &CreditWindowDTO{
		WindowID:        w.ID,
		MerchantSubjectID: w.MerchantSubjectID,
		HeldAmount:      w.HeldAmount,
		SettledAmount:   w.SettledAmount,
		Status:          w.Status,
		ExpiresAt:       w.ExpiresAt,
	}
}
