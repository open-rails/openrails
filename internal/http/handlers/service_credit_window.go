package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Prepaid credit windows (issue #335): POST /v1/service/credits/windows opens
// one bulk reservation the host admits against locally; /settle flushes
// CROSS-PAYER batches of actuals (per-item isolation + idempotency);
// /windows/:id/refill extends funds+TTL; /windows/:id/close releases the
// remainder. All service token-authed like the sibling credit spend routes.

type serviceOpenWindowRequest struct {
	CustomerID string `json:"customer_id"`
	Invoker    string `json:"invoker"`
	// Currency is optional; empty defaults to "USD" server-side (#476).
	Currency   string `json:"currency"`
	Amount     int64  `json:"amount" binding:"required"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

// ServiceOpenCreditWindow opens a prepaid window: a REAL hold — funds leave the
// payer's available balance now (no optimistic approval), so local admission
// against the window spends already-reserved money. 402 on insufficient funds.
func ServiceOpenCreditWindow(r *httprequest.Request) {
	var req serviceOpenWindowRequest
	if !r.BindJSON(&req) {
		return
	}
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	w, err := svc.OpenWindow(r.Request.Context(), billingservice.OpenWindowRequest{
		CustomerID: *payer,
		Invoker:    req.Invoker,
		Currency:   req.Currency,
		Amount:     req.Amount,
		TTL:        time.Duration(req.TTLSeconds) * time.Second,
	})
	if errors.Is(err, billingservice.ErrInsufficientCredits) {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "open window failed")
		return
	}
	r.SuccessJSON(w)
}

type serviceWindowSettleUsage struct {
	EventType string         `json:"event_type"`
	Resource  string         `json:"resource"`
	Metadata  map[string]any `json:"metadata"`
}

type serviceWindowSettleItem struct {
	WindowID  string                    `json:"window_id"`
	RequestID string                    `json:"request_id"`
	Amount    int64                     `json:"amount"`
	Invoker   string                    `json:"invoker,omitempty"`
	Usage     *serviceWindowSettleUsage `json:"usage,omitempty"`
}

type serviceWindowSettleRequest struct {
	Items []serviceWindowSettleItem `json:"items"`
}

// maxWindowBatchItems bounds one settle/admit batch request.
const maxWindowBatchItems = 1000

// ServiceSettleCreditWindows settles a CROSS-PAYER batch of actuals against
// their windows. Per-item results: one item's failure (unknown window, closed,
// over-settle) never fails the batch, and idempotent replays (same request_id)
// return success without double-charging — so a re-sent flush is always safe.
func ServiceSettleCreditWindows(r *httprequest.Request) {
	var req serviceWindowSettleRequest
	if !r.BindJSON(&req) {
		return
	}
	if len(req.Items) == 0 {
		r.ErrorJSON(http.StatusBadRequest, "items required")
		return
	}
	if len(req.Items) > maxWindowBatchItems {
		r.ErrorJSON(http.StatusBadRequest, "too many items")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	in := make([]billingservice.WindowSettleItemInput, 0, len(req.Items))
	// Items with an unparseable window_id still need a positional result; track
	// them and merge after the settle call.
	type skip struct {
		idx       int
		requestID string
	}
	var skips []skip
	idxByInput := make([]int, 0, len(req.Items))
	for i, it := range req.Items {
		wid, perr := uuid.Parse(it.WindowID)
		if perr != nil || wid == uuid.Nil {
			skips = append(skips, skip{idx: i, requestID: it.RequestID})
			continue
		}
		item := billingservice.WindowSettleItemInput{
			WindowID:  wid,
			RequestID: it.RequestID,
			Amount:    it.Amount,
			Invoker:   it.Invoker,
		}
		if it.Usage != nil {
			item.EventType = it.Usage.EventType
			item.Resource = it.Usage.Resource
			item.Metadata = it.Usage.Metadata
		}
		in = append(in, item)
		idxByInput = append(idxByInput, i)
	}

	settled, err := svc.SettleWindowItems(r.Request.Context(), in)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "settle failed")
		return
	}

	out := make([]billingservice.WindowSettleItemResult, len(req.Items))
	for j, res := range settled {
		out[idxByInput[j]] = res
	}
	for _, sk := range skips {
		out[sk.idx] = billingservice.WindowSettleItemResult{RequestID: sk.requestID, ErrorCode: "invalid_item"}
	}
	r.SuccessJSON(map[string]any{"items": out})
}

type serviceWindowRefillRequest struct {
	Amount     int64 `json:"amount"`
	TTLSeconds int64 `json:"ttl_seconds"`
}

// ServiceRefillCreditWindow extends an open window's held funds and/or TTL.
// Insufficient available funds fail exactly like open (402) — never optimistic.
func ServiceRefillCreditWindow(r *httprequest.Request) {
	windowID, err := uuid.Parse(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid window id")
		return
	}
	var req serviceWindowRefillRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.Amount <= 0 && req.TTLSeconds <= 0 {
		r.ErrorJSON(http.StatusBadRequest, "amount or ttl_seconds required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	w, err := svc.RefillWindow(r.Request.Context(), windowID, req.Amount, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		writeWindowError(r, err, "refill failed")
		return
	}
	r.SuccessJSON(w)
}

// ServiceCloseCreditWindow releases the window's unsettled remainder and closes
// it. Idempotent: re-closing returns the current (closed/expired) window.
func ServiceCloseCreditWindow(r *httprequest.Request) {
	windowID, err := uuid.Parse(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid window id")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	w, err := svc.CloseWindow(r.Request.Context(), windowID)
	if err != nil {
		writeWindowError(r, err, "close failed")
		return
	}
	r.SuccessJSON(w)
}

func writeWindowError(r *httprequest.Request, err error, fallback string) {
	switch {
	case errors.Is(err, billingservice.ErrWindowNotFound):
		r.ErrorJSON(http.StatusNotFound, "window_not_found")
	case errors.Is(err, billingservice.ErrWindowNotOpen):
		r.ErrorJSON(http.StatusConflict, "window_not_open")
	case errors.Is(err, billingservice.ErrInsufficientCredits):
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
	default:
		r.ErrorJSON(http.StatusInternalServerError, fallback)
	}
}
