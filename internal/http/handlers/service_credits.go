package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin/binding"
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
	// Internal-only escape hatch (trusted callers): allow a bounded negative balance for api_credits.
	AllowNegative *bool  `json:"allow_negative"`
	MaxNegative   *int64 `json:"max_negative"` // absolute units, e.g. 100 => allow down to -100
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
	// Internal-only escape hatch (trusted callers): allow a bounded negative balance for api_credits.
	AllowNegative *bool  `json:"allow_negative"`
	MaxNegative   *int64 `json:"max_negative"` // absolute units
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
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	type itemResult struct {
		Ok     bool                              `json:"ok"`
		Status int                               `json:"status"`
		Result *billingservice.CreditTransaction `json:"result,omitempty"`
		Error  string                            `json:"error,omitempty"`
	}
	raws, isBatch, err := readJSONSingleOrArrayRaw(r)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	if !isBatch {
		var req serviceWithdrawRequest
		if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid request")
			return
		}
		allowNeg := req.AllowNegative != nil && *req.AllowNegative
		trx, err := svc.WithdrawCredits(r.Request.Context(), billingservice.WithdrawCreditsRequest{
			UserID:        req.UserID,
			CreditType:    req.CreditType,
			Amount:        req.Amount,
			Source:        req.Source,
			SourceID:      req.SourceID,
			AllowNegative: allowNeg,
			MaxNegative:   derefInt64(req.MaxNegative),
		})
		if err == billingservice.ErrInsufficientCredits {
			r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
			return
		}
		if err == billingservice.ErrCreditTypeInactive {
			r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
			return
		}
		if err == billingservice.ErrInvalidNegativePolicy {
			r.ErrorJSON(http.StatusBadRequest, "invalid_negative_policy")
			return
		}
		if err == billingservice.ErrNegativeBalanceLimitExceeded {
			r.ErrorJSON(http.StatusPaymentRequired, "negative_balance_limit_exceeded")
			return
		}
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "withdraw failed")
			return
		}
		r.SuccessJSON(trx)
		return
	}

	results := make([]itemResult, 0, len(raws))
	for i := range raws {
		var req serviceWithdrawRequest
		if err := json.Unmarshal(raws[i], &req); err != nil {
			results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_request"})
			continue
		}
		if err := binding.Validator.ValidateStruct(&req); err != nil {
			results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_request"})
			continue
		}
		allowNeg := req.AllowNegative != nil && *req.AllowNegative
		trx, err := svc.WithdrawCredits(r.Request.Context(), billingservice.WithdrawCreditsRequest{
			UserID:        req.UserID,
			CreditType:    req.CreditType,
			Amount:        req.Amount,
			Source:        req.Source,
			SourceID:      req.SourceID,
			AllowNegative: allowNeg,
			MaxNegative:   derefInt64(req.MaxNegative),
		})
		if err == nil {
			results = append(results, itemResult{Ok: true, Status: http.StatusOK, Result: trx})
			continue
		}
		switch err {
		case billingservice.ErrInsufficientCredits:
			results = append(results, itemResult{Ok: false, Status: http.StatusPaymentRequired, Error: "insufficient_credits"})
		case billingservice.ErrCreditTypeInactive:
			results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "credit_type_inactive"})
		case billingservice.ErrInvalidNegativePolicy:
			results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_negative_policy"})
		case billingservice.ErrNegativeBalanceLimitExceeded:
			results = append(results, itemResult{Ok: false, Status: http.StatusPaymentRequired, Error: "negative_balance_limit_exceeded"})
		default:
			results = append(results, itemResult{Ok: false, Status: http.StatusInternalServerError, Error: "withdraw_failed"})
		}
	}
	r.SuccessJSON(map[string]any{"results": results})
}

func ServiceHoldCredits(r *httprequest.Request) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	type itemResult struct {
		Ok     bool                       `json:"ok"`
		Status int                        `json:"status"`
		Result *billingservice.CreditHold `json:"result,omitempty"`
		Error  string                     `json:"error,omitempty"`
	}
	raws, isBatch, err := readJSONSingleOrArrayRaw(r)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	if !isBatch {
		var req serviceHoldRequest
		if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid request")
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
		return
	}

	results := make([]itemResult, 0, len(raws))
	for i := range raws {
		var req serviceHoldRequest
		if err := json.Unmarshal(raws[i], &req); err != nil {
			results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_request"})
			continue
		}
		if err := binding.Validator.ValidateStruct(&req); err != nil {
			results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_request"})
			continue
		}
		hold, err := svc.HoldCredits(r.Request.Context(), billingservice.HoldCreditsRequest{
			UserID:     req.UserID,
			CreditType: req.CreditType,
			Amount:     req.Amount,
			Source:     req.Source,
			SourceID:   req.SourceID,
			ExpiresAt:  time.Unix(req.ExpiresAt, 0).UTC(),
		})
		if err == nil {
			results = append(results, itemResult{Ok: true, Status: http.StatusOK, Result: hold})
			continue
		}
		switch err {
		case billingservice.ErrInsufficientCredits:
			results = append(results, itemResult{Ok: false, Status: http.StatusPaymentRequired, Error: "insufficient_credits"})
		case billingservice.ErrCreditTypeInactive:
			results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "credit_type_inactive"})
		default:
			results = append(results, itemResult{Ok: false, Status: http.StatusInternalServerError, Error: "hold_failed"})
		}
	}
	r.SuccessJSON(map[string]any{"results": results})
}

func ServiceCaptureHold(r *httprequest.Request) {
	idParam := strings.TrimSpace(r.GinCtx.Param("id"))
	if strings.EqualFold(idParam, "batch") {
		type captureBatchItem struct {
			HoldID        uuid.UUID `json:"hold_id" binding:"required"`
			Amount        int64     `json:"amount" binding:"required"`
			AllowNegative *bool     `json:"allow_negative"`
			MaxNegative   *int64    `json:"max_negative"`
		}
		raws, isBatch, err := readJSONSingleOrArrayRaw(r)
		if err != nil || !isBatch {
			r.ErrorJSON(http.StatusBadRequest, "invalid request")
			return
		}
		svc, err := billingservice.New(r.State)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
			return
		}
		type itemResult struct {
			Ok     bool                              `json:"ok"`
			Status int                               `json:"status"`
			Result *billingservice.CreditTransaction `json:"result,omitempty"`
			Error  string                            `json:"error,omitempty"`
		}
		results := make([]itemResult, 0, len(raws))
		for i := range raws {
			var it captureBatchItem
			if err := json.Unmarshal(raws[i], &it); err != nil {
				results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_request"})
				continue
			}
			if err := binding.Validator.ValidateStruct(&it); err != nil {
				results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_request"})
				continue
			}
			allowNeg := it.AllowNegative != nil && *it.AllowNegative
			trx, err := svc.CaptureHold(r.Request.Context(), billingservice.CaptureHoldRequest{
				HoldID:        it.HoldID,
				Amount:        it.Amount,
				AllowNegative: allowNeg,
				MaxNegative:   derefInt64(it.MaxNegative),
			})
			if err == nil {
				results = append(results, itemResult{Ok: true, Status: http.StatusOK, Result: trx})
				continue
			}
			switch err {
			case billingservice.ErrInsufficientCredits:
				results = append(results, itemResult{Ok: false, Status: http.StatusPaymentRequired, Error: "insufficient_credits"})
			case billingservice.ErrInvalidNegativePolicy:
				results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_negative_policy"})
			case billingservice.ErrNegativeBalanceLimitExceeded:
				results = append(results, itemResult{Ok: false, Status: http.StatusPaymentRequired, Error: "negative_balance_limit_exceeded"})
			default:
				results = append(results, itemResult{Ok: false, Status: http.StatusInternalServerError, Error: "capture_failed"})
			}
		}
		r.SuccessJSON(map[string]any{"results": results})
		return
	}

	holdID, err := uuid.Parse(idParam)
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
	allowNeg := req.AllowNegative != nil && *req.AllowNegative
	trx, err := svc.CaptureHold(r.Request.Context(), billingservice.CaptureHoldRequest{
		HoldID:        holdID,
		Amount:        req.Amount,
		AllowNegative: allowNeg,
		MaxNegative:   derefInt64(req.MaxNegative),
	})
	if err == billingservice.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err == billingservice.ErrInvalidNegativePolicy {
		r.ErrorJSON(http.StatusBadRequest, "invalid_negative_policy")
		return
	}
	if err == billingservice.ErrNegativeBalanceLimitExceeded {
		r.ErrorJSON(http.StatusPaymentRequired, "negative_balance_limit_exceeded")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "capture failed")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceReleaseHold(r *httprequest.Request) {
	idParam := strings.TrimSpace(r.GinCtx.Param("id"))
	if strings.EqualFold(idParam, "batch") {
		type releaseBatchItem struct {
			HoldID uuid.UUID `json:"hold_id" binding:"required"`
		}
		raws, isBatch, err := readJSONSingleOrArrayRaw(r)
		if err != nil || !isBatch {
			r.ErrorJSON(http.StatusBadRequest, "invalid request")
			return
		}
		svc, err := billingservice.New(r.State)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
			return
		}
		type itemResult struct {
			Ok     bool   `json:"ok"`
			Status int    `json:"status"`
			Error  string `json:"error,omitempty"`
		}
		results := make([]itemResult, 0, len(raws))
		for i := range raws {
			var it releaseBatchItem
			if err := json.Unmarshal(raws[i], &it); err != nil {
				results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_request"})
				continue
			}
			if err := binding.Validator.ValidateStruct(&it); err != nil {
				results = append(results, itemResult{Ok: false, Status: http.StatusBadRequest, Error: "invalid_request"})
				continue
			}
			if err := svc.ReleaseHold(r.Request.Context(), it.HoldID); err != nil {
				results = append(results, itemResult{Ok: false, Status: http.StatusInternalServerError, Error: "release_failed"})
				continue
			}
			results = append(results, itemResult{Ok: true, Status: http.StatusOK})
		}
		r.SuccessJSON(map[string]any{"results": results})
		return
	}

	holdID, err := uuid.Parse(idParam)
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
