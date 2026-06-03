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

// serviceAuthorizeRequest is the body of POST /v1/service/credits/authorize
// (issue #235/#247). payer = the owner org billed; invoker = the canonical actor
// for per-(payer,invoker) sub-budgets; estimate_cents = the upper-bound charge;
// request_id = the idempotency key for the placed hold.
type serviceAuthorizeRequest struct {
	Payer         string `json:"payer"`
	OwnerID       string `json:"owner_id"`
	OwnerOrgID    string `json:"owner_org_id"`
	Invoker       string `json:"invoker"`
	CreditType    string `json:"credit_type" binding:"required"`
	EstimateCents int64  `json:"estimate_cents"`
	RequestID     string `json:"request_id" binding:"required"`
	ExpiresAt     *int64 `json:"expires_at"`
}

// serviceAuthorizeResponse mirrors the unified authorize contract: the policy
// decision + the payer's real available/outstanding + the placed reservation.
type serviceAuthorizeResponse struct {
	Allowed             bool       `json:"allowed"`
	DenyCode            string     `json:"deny_code,omitempty"`
	BillingMode         string     `json:"billing_mode"`
	AvailableCents      int64      `json:"available_cents"`
	OutstandingCents    int64      `json:"outstanding_cents"`
	RemainingTodayCents *int64     `json:"remaining_today_cents,omitempty"`
	RetryAfterSeconds   int64      `json:"retry_after_seconds,omitempty"`
	ReservationID       *uuid.UUID `json:"reservation_id,omitempty"`
}

// ServiceAuthorizeCredits is the OAT-authed policy-decision + ATOMIC hold
// placement (issue #235/#247). The decision (CheckSpendAllowed + prepaid
// available-balance gate) and the hold are run in ONE transaction so two
// concurrent authorizes cannot both pass on the same balance.
//
// PAYER AUTHORIZATION (issue #246): the acting TENANT is bound by the OAT
// (middleware.OATRequired pinned it onto the context; RegisterServiceRoutes then
// pinned the RLS connection), NEVER by the body. The payer (owner org) is read
// from the body but every credit query runs RLS-scoped to the OAT's tenant, so a
// payer outside the OAT's tenant is structurally unreachable (its rows carry a
// different tenant_id and are invisible — fail closed). The route additionally
// requires PermCreditsSpend (billing:spend), the explicit "may you draw down this
// payer's balance" gate.
func ServiceAuthorizeCredits(r *httprequest.Request) {
	var req serviceAuthorizeRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.EstimateCents < 0 {
		r.ErrorJSON(http.StatusBadRequest, "estimate_cents must be >= 0")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	// Payer is derived from the body but is ONLY ever billed within the OAT's
	// tenant (RLS). A missing/invalid payer fails closed.
	payerRaw := req.Payer
	if strings.TrimSpace(payerRaw) == "" {
		payerRaw = req.OwnerID
	}
	if strings.TrimSpace(payerRaw) == "" {
		payerRaw = req.OwnerOrgID
	}
	payer, err := parseServiceOwnerOrgID(payerRaw, "")
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid payer")
		return
	}
	if payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "payer required")
		return
	}

	var expiresAt time.Time
	if req.ExpiresAt != nil {
		expiresAt = time.Unix(*req.ExpiresAt, 0).UTC()
	}

	out, err := svc.AuthorizeAndHold(r.Request.Context(), billingservice.AuthorizeAndHoldRequest{
		Payer:         *payer,
		Invoker:       req.Invoker,
		CreditType:    req.CreditType,
		EstimateCents: req.EstimateCents,
		RequestID:     req.RequestID,
		ExpiresAt:     expiresAt,
	})
	if err != nil {
		if err == billingservice.ErrCreditTypeInactive {
			r.ErrorJSON(http.StatusBadRequest, "credit_type_inactive")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "authorize failed")
		return
	}

	r.SuccessJSON(serviceAuthorizeResponse{
		Allowed:             out.Allowed,
		DenyCode:            out.DenyCode,
		BillingMode:         out.BillingMode,
		AvailableCents:      out.AvailableCents,
		OutstandingCents:    out.OutstandingOwedCents,
		RemainingTodayCents: out.RemainingTodayCents,
		RetryAfterSeconds:   out.RetryAfterSeconds,
		ReservationID:       out.ReservationID,
	})
}

// serviceBalanceResponse is the payer balance snapshot served by
// GET /v1/service/credits/balance (issue #235/#247).
type serviceBalanceResponse struct {
	OwnerID              uuid.UUID `json:"owner_id"`
	CreditType           string    `json:"credit_type"`
	BillingMode          string    `json:"billing_mode"`
	BalanceCents         int64     `json:"balance_cents"`
	HeldCents            int64     `json:"held_cents"`
	AvailableCents       int64     `json:"available_cents"`
	OutstandingOwedCents int64     `json:"outstanding_owed_cents"`
}

// ServiceGetCreditsBalance returns the payer's REAL balance snapshot (issue
// #235/#247): available = balance - held, plus outstanding owed + billing mode.
// Tenant-bound by the OAT (RLS); payer supplied via ?payer= (or ?owner_id=).
func ServiceGetCreditsBalance(r *httprequest.Request) {
	creditType := strings.TrimSpace(r.Request.URL.Query().Get("credit_type"))
	if creditType == "" {
		creditType = strings.TrimSpace(r.Request.URL.Query().Get("type"))
	}
	if creditType == "" {
		creditType = "api_credits"
	}
	payerRaw := r.Request.URL.Query().Get("payer")
	if strings.TrimSpace(payerRaw) == "" {
		payerRaw = r.Request.URL.Query().Get("owner_id")
	}
	if strings.TrimSpace(payerRaw) == "" {
		payerRaw = r.Request.URL.Query().Get("owner_org_id")
	}
	payer, err := parseServiceOwnerOrgID(payerRaw, "")
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid payer")
		return
	}
	if payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "payer required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	snap, err := svc.GetCreditAccount(r.Request.Context(), *payer, creditType)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(serviceBalanceResponse{
		OwnerID:              snap.OwnerID,
		CreditType:           snap.CreditType,
		BillingMode:          snap.BillingMode,
		BalanceCents:         snap.BalanceCents,
		HeldCents:            snap.HeldCents,
		AvailableCents:       snap.AvailableCents,
		OutstandingOwedCents: snap.OutstandingOwedCents,
	})
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
