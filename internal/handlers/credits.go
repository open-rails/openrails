package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	authgin "github.com/PaulFidika/authkit/adapters/gin"
	"github.com/doujins-org/doujins-billing/internal/services"
	"github.com/google/uuid"
)

type creditBalanceResponse struct {
	Type           string `json:"type"`
	DisplayName    string `json:"display_name"`
	Unit           string `json:"unit"`
	DecimalPlaces  int    `json:"decimal_places"`
	Balance        int64  `json:"balance"`
	HeldBalance    int64  `json:"held_balance"`
	Permanent      int64  `json:"permanent_balance"`
	Expiring       int64  `json:"expiring_balance"`
	EarliestExpiry *int64 `json:"earliest_expiry,omitempty"`
}

func GetMyCredits(r *Request) {
	cl, ok := authgin.ClaimsFromGin(r.GinCtx)
	if !ok || cl.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	var rows []struct {
		CreditTypeID   uuid.UUID  `bun:"credit_type_id"`
		Name           string     `bun:"name"`
		DisplayName    string     `bun:"display_name"`
		Unit           string     `bun:"unit"`
		DecimalPlaces  int        `bun:"decimal_places"`
		Balance        *int64     `bun:"balance"`
		HeldBalance    *int64     `bun:"held_balance"`
		Permanent      *int64     `bun:"permanent_balance"`
		Expiring       *int64     `bun:"expiring_balance"`
		EarliestExpiry *time.Time `bun:"earliest_expiry"`
	}

	err := r.State.DB.GetDB().NewSelect().
		TableExpr("billing.credit_types ct").
		ColumnExpr("ct.id as credit_type_id").
		ColumnExpr("ct.name").
		ColumnExpr("ct.display_name").
		ColumnExpr("ct.unit").
		ColumnExpr("ct.decimal_places").
		ColumnExpr("ucb.balance").
		ColumnExpr("ucb.held_balance").
		ColumnExpr("ucb.permanent_balance").
		ColumnExpr("ucb.expiring_balance").
		ColumnExpr("ucb.earliest_expiry").
		Join("LEFT JOIN billing.user_credit_balances ucb ON ucb.credit_type_id = ct.id AND ucb.user_id = ?", cl.UserID).
		Where("ct.is_active = true").
		Scan(r.Request.Context(), &rows)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load credits")
		return
	}

	resp := make([]creditBalanceResponse, 0, len(rows))
	for _, row := range rows {
		var earliest *int64
		if row.EarliestExpiry != nil && !row.EarliestExpiry.IsZero() {
			epoch := row.EarliestExpiry.Unix()
			earliest = &epoch
		}
		resp = append(resp, creditBalanceResponse{
			Type:           row.Name,
			DisplayName:    row.DisplayName,
			Unit:           row.Unit,
			DecimalPlaces:  row.DecimalPlaces,
			Balance:        derefInt64(row.Balance),
			HeldBalance:    derefInt64(row.HeldBalance),
			Permanent:      derefInt64(row.Permanent),
			Expiring:       derefInt64(row.Expiring),
			EarliestExpiry: earliest,
		})
	}

	r.SuccessJSON(resp)
}

func GetMyCreditsType(r *Request) {
	cl, ok := authgin.ClaimsFromGin(r.GinCtx)
	if !ok || cl.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	creditType := strings.TrimSpace(r.GinCtx.Param("type"))
	if creditType == "" {
		r.ErrorJSON(http.StatusBadRequest, "credit type required")
		return
	}

	bal, err := r.State.CreditsService.GetBalance(r.Request.Context(), cl.UserID, creditType)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "credit type not found")
		return
	}
	ct, err := r.State.CreditsService.GetCreditTypeByName(r.Request.Context(), creditType)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "credit type not found")
		return
	}

	earliest := (*int64)(nil)
	if bal.EarliestExpiry != nil && !bal.EarliestExpiry.IsZero() {
		ts := bal.EarliestExpiry.Unix()
		earliest = &ts
	}

	r.SuccessJSON(creditBalanceResponse{
		Type:           creditType,
		DisplayName:    ct.DisplayName,
		Unit:           ct.Unit,
		DecimalPlaces:  ct.DecimalPlaces,
		Balance:        bal.Balance,
		HeldBalance:    bal.HeldBalance,
		Permanent:      bal.Permanent,
		Expiring:       bal.Expiring,
		EarliestExpiry: earliest,
	})
}

func GetMyCreditTransactions(r *Request) {
	cl, ok := authgin.ClaimsFromGin(r.GinCtx)
	if !ok || cl.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	creditType := strings.TrimSpace(r.GinCtx.Param("type"))
	if creditType == "" {
		r.ErrorJSON(http.StatusBadRequest, "credit type required")
		return
	}

	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	items, total, err := r.State.CreditsService.GetTransactions(r.Request.Context(), cl.UserID, creditType, limit, offset)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load transactions")
		return
	}
	r.SuccessJSONPaginated(items, int64(total), limit, offset)
}

type serviceWithdrawRequest struct {
	UserID     string     `json:"user_id" binding:"required"`
	CreditType string     `json:"credit_type" binding:"required"`
	Amount     int64      `json:"amount" binding:"required"`
	Source     string     `json:"source" binding:"required"`
	SourceID   *uuid.UUID `json:"source_id"`
}

func ServiceWithdrawCredits(r *Request) {
	var req serviceWithdrawRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	trx, err := r.State.CreditsService.Withdraw(r.Request.Context(), services.CreditWithdrawParams{
		UserID:     req.UserID,
		CreditType: req.CreditType,
		Amount:     req.Amount,
		Source:     req.Source,
		SourceID:   req.SourceID,
	})
	if err == services.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "withdraw failed")
		return
	}
	r.SuccessJSON(trx)
}

type serviceHoldRequest struct {
	UserID     string `json:"user_id" binding:"required"`
	CreditType string `json:"credit_type" binding:"required"`
	Amount     int64  `json:"amount" binding:"required"`
	Source     string `json:"source" binding:"required"`
	SourceID   string `json:"source_id" binding:"required"`
	ExpiresAt  int64  `json:"expires_at" binding:"required"` // epoch seconds
}

func ServiceHoldCredits(r *Request) {
	var req serviceHoldRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	hold, err := r.State.CreditsService.Hold(
		r.Request.Context(),
		req.UserID,
		req.CreditType,
		req.Amount,
		req.Source,
		req.SourceID,
		time.Unix(req.ExpiresAt, 0).UTC(),
	)
	if err == services.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "hold failed")
		return
	}
	r.SuccessJSON(hold)
}

type serviceCaptureRequest struct {
	Amount int64 `json:"amount" binding:"required"`
}

func ServiceCaptureHold(r *Request) {
	holdID, err := uuid.Parse(r.GinCtx.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid hold id")
		return
	}
	var req serviceCaptureRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	trx, err := r.State.CreditsService.CaptureHold(r.Request.Context(), holdID, req.Amount)
	if err == services.ErrInsufficientCredits {
		r.ErrorJSON(http.StatusPaymentRequired, "insufficient_credits")
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "capture failed")
		return
	}
	r.SuccessJSON(trx)
}

func ServiceReleaseHold(r *Request) {
	holdID, err := uuid.Parse(r.GinCtx.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid hold id")
		return
	}
	if err := r.State.CreditsService.ReleaseHold(r.Request.Context(), holdID); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "release failed")
		return
	}
	r.SuccessJSON(map[string]any{"ok": true})
}

func ServiceGetUserCredits(r *Request) {
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
		"type":              creditType,
		"balance":           bal.Balance,
		"held_balance":      bal.HeldBalance,
		"permanent_balance": bal.Permanent,
		"expiring_balance":  bal.Expiring,
	})
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
