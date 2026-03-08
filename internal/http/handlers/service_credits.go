package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type serviceWithdrawRequest struct {
	UserID     string     `json:"user_id" binding:"required"`
	CreditType string     `json:"credit_type" binding:"required"`
	Amount     int64      `json:"amount" binding:"required"`
	Source     string     `json:"source" binding:"required"`
	SourceID   *uuid.UUID `json:"source_id"`
}

type serviceDepositRequest struct {
	UserID      string     `json:"user_id" binding:"required"`
	CreditType  string     `json:"credit_type" binding:"required"`
	Amount      int64      `json:"amount" binding:"required"`
	Source      string     `json:"source" binding:"required"`
	SourceID    *uuid.UUID `json:"source_id"`
	ExpiresAt   *int64     `json:"expires_at"`
	Description *string    `json:"description"`
}

type serviceHoldRequest struct {
	UserID     string `json:"user_id" binding:"required"`
	CreditType string `json:"credit_type" binding:"required"`
	Amount     int64  `json:"amount" binding:"required"`
	Source     string `json:"source" binding:"required"`
	SourceID   string `json:"source_id" binding:"required"`
	ExpiresAt  int64  `json:"expires_at" binding:"required"`
}

type serviceCaptureRequest struct {
	Amount int64 `json:"amount" binding:"required"`
}

func ServiceDepositCredits(r *httprequest.Request) {
	var req serviceDepositRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		v := time.Unix(*req.ExpiresAt, 0).UTC()
		expiresAt = &v
	}

	trx, err := svc.DepositCredits(r.Request.Context(), billingservice.DepositCreditsRequest{
		UserID:      req.UserID,
		CreditType:  req.CreditType,
		Amount:      req.Amount,
		Source:      req.Source,
		SourceID:    req.SourceID,
		ExpiresAt:   expiresAt,
		Description: req.Description,
	})
	if err != nil {
		if err == billingservice.ErrCreditTypeInactive {
			r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "deposit failed")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceWithdrawCredits(r *httprequest.Request) {
	var req serviceWithdrawRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	trx, err := svc.WithdrawCredits(r.Request.Context(), billingservice.WithdrawCreditsRequest{
		UserID:     req.UserID,
		CreditType: req.CreditType,
		Amount:     req.Amount,
		Source:     req.Source,
		SourceID:   req.SourceID,
	})
	if err == billingservice.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err == billingservice.ErrCreditTypeInactive {
		r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "withdraw failed")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceHoldCredits(r *httprequest.Request) {
	var req serviceHoldRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	hold, err := svc.HoldCredits(r.Request.Context(), billingservice.HoldCreditsRequest{
		UserID:     req.UserID,
		CreditType: req.CreditType,
		Amount:     req.Amount,
		Source:     req.Source,
		SourceID:   req.SourceID,
		ExpiresAt:  time.Unix(req.ExpiresAt, 0).UTC(),
	})
	if err == billingservice.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err == billingservice.ErrCreditTypeInactive {
		r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "hold failed")
		return
	}
	r.SuccessJSON(hold)
}

func ServiceCaptureHold(r *httprequest.Request) {
	holdID, err := uuid.Parse(r.GinCtx.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid hold id")
		return
	}
	var req serviceCaptureRequest
	if err = r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	trx, err := svc.CaptureHold(r.Request.Context(), billingservice.CaptureHoldRequest{HoldID: holdID, Amount: req.Amount})
	if err == billingservice.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "capture failed")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceReleaseHold(r *httprequest.Request) {
	holdID, err := uuid.Parse(r.GinCtx.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid hold id")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.ReleaseHold(r.Request.Context(), holdID); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "release failed")
		return
	}
	r.SuccessJSON(map[string]any{"ok": true})
}

func ServiceGetUserCredits(r *httprequest.Request) {
	userID := strings.TrimSpace(r.GinCtx.Param("user_id"))
	if userID == "" {
		r.ErrorJSON(http.StatusBadRequest, "user_id required")
		return
	}
	creditType := strings.TrimSpace(r.Request.URL.Query().Get("type"))
	if creditType == "" {
		creditType = "api_credits"
	}
	bal, err := r.State.CreditsService.GetBalance(r.Request.Context(), userID, creditType)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "credit type not found")
		return
	}
	r.SuccessJSON(map[string]any{
		"type":         creditType,
		"balance":      bal.Balance,
		"held_balance": bal.HeldBalance,
	})
}

func ServiceLookupCreditTransaction(r *httprequest.Request) {
	userID := strings.TrimSpace(r.Request.URL.Query().Get("user_id"))
	if userID == "" {
		r.ErrorJSON(http.StatusBadRequest, "user_id required")
		return
	}
	creditType := strings.TrimSpace(r.Request.URL.Query().Get("credit_type"))
	if creditType == "" {
		r.ErrorJSON(http.StatusBadRequest, "credit_type required")
		return
	}
	source := strings.TrimSpace(r.Request.URL.Query().Get("source"))
	if source == "" {
		r.ErrorJSON(http.StatusBadRequest, "source required")
		return
	}
	sourceID := strings.TrimSpace(r.Request.URL.Query().Get("source_id"))
	if sourceID == "" {
		r.ErrorJSON(http.StatusBadRequest, "source_id required")
		return
	}
	transactionType := strings.TrimSpace(r.Request.URL.Query().Get("transaction_type"))
	if transactionType == "" {
		transactionType = "hold"
	}

	trx, err := r.State.CreditsService.GetTransactionBySource(r.Request.Context(), userID, creditType, transactionType, source, sourceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "not found")
			return
		}
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(trx)
}
