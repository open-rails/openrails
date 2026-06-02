package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingidentity "github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type serviceWithdrawRequest struct {
	OwnerID    string     `json:"owner_id"`
	OwnerOrgID string     `json:"owner_org_id"`
	UserID     string     `json:"user_id" binding:"required"`
	CreditType string     `json:"credit_type" binding:"required"`
	Amount     int64      `json:"amount" binding:"required"`
	Source     string     `json:"source" binding:"required"`
	SourceID   *uuid.UUID `json:"source_id" binding:"required"`
}

type serviceDepositRequest struct {
	OwnerID     string     `json:"owner_id"`
	OwnerOrgID  string     `json:"owner_org_id"`
	UserID      string     `json:"user_id" binding:"required"`
	CreditType  string     `json:"credit_type" binding:"required"`
	Amount      int64      `json:"amount" binding:"required"`
	Source      string     `json:"source" binding:"required"`
	SourceID    *uuid.UUID `json:"source_id" binding:"required"`
	ExpiresAt   *int64     `json:"expires_at"`
	Description *string    `json:"description"`
}

type serviceHoldRequest struct {
	OwnerID    string `json:"owner_id"`
	OwnerOrgID string `json:"owner_org_id"`
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

func parseServiceOwnerOrgID(ownerID, ownerOrgID string) (*billingidentity.OwnerOrgID, error) {
	raw := strings.TrimSpace(ownerID)
	if raw == "" {
		raw = strings.TrimSpace(ownerOrgID)
	}
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return nil, errors.New("invalid owner_id")
	}
	owner := billingidentity.OwnerOrgID(id)
	return &owner, nil
}

func ServiceDepositCredits(r *httprequest.Request) {
	var req serviceDepositRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	ownerID, err := parseServiceOwnerOrgID(req.OwnerID, req.OwnerOrgID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		v := time.Unix(*req.ExpiresAt, 0).UTC()
		expiresAt = &v
	}

	trx, err := svc.DepositCredits(r.Request.Context(), billingservice.DepositCreditsRequest{
		OwnerID:     ownerID,
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
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	ownerID, err := parseServiceOwnerOrgID(req.OwnerID, req.OwnerOrgID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	trx, err := svc.WithdrawCredits(r.Request.Context(), billingservice.WithdrawCreditsRequest{
		OwnerID:    ownerID,
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
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	ownerID, err := parseServiceOwnerOrgID(req.OwnerID, req.OwnerOrgID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	hold, err := svc.HoldCredits(r.Request.Context(), billingservice.HoldCreditsRequest{
		OwnerID:    ownerID,
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
	if !r.BindJSON(&req) {
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
	ownerID, err := parseServiceOwnerOrgID(r.Request.URL.Query().Get("owner_id"), r.Request.URL.Query().Get("owner_org_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var balance int64
	var heldBalance int64
	if ownerID != nil {
		bal, err := r.State.CreditsService.GetBalanceForOwner(r.Request.Context(), *ownerID, creditType)
		if err == nil {
			balance = bal.Balance
			heldBalance = bal.HeldBalance
		}
	} else {
		bal, err := r.State.CreditsService.GetBalance(r.Request.Context(), userID, creditType)
		if err == nil {
			balance = bal.Balance
			heldBalance = bal.HeldBalance
		}
	}
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "credit type not found")
		return
	}
	r.SuccessJSON(map[string]any{
		"type":         creditType,
		"balance":      balance,
		"held_balance": heldBalance,
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
