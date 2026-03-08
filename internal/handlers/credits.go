package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

type creditBalanceResponse struct {
	Type          string `json:"type"`
	DisplayName   string `json:"display_name"`
	Unit          string `json:"unit"`
	DecimalPlaces int    `json:"decimal_places"`
	Balance       int64  `json:"balance"`
	HeldBalance   int64  `json:"held_balance"`
}

func GetMyCredits(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	var rows []struct {
		CreditTypeID  uuid.UUID `bun:"credit_type_id"`
		Name          string    `bun:"name"`
		DisplayName   string    `bun:"display_name"`
		Unit          string    `bun:"unit"`
		DecimalPlaces int       `bun:"decimal_places"`
		Balance       *int64    `bun:"balance"`
		HeldBalance   *int64    `bun:"held_balance"`
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
		Join("LEFT JOIN billing.user_credit_balances ucb ON ucb.credit_type_id = ct.id AND ucb.user_id = ?", user.ID).
		Where("ct.is_active = true").
		Scan(r.Request.Context(), &rows)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load credits")
		return
	}

	resp := make([]creditBalanceResponse, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, creditBalanceResponse{
			Type:          row.Name,
			DisplayName:   row.DisplayName,
			Unit:          row.Unit,
			DecimalPlaces: row.DecimalPlaces,
			Balance:       derefInt64(row.Balance),
			HeldBalance:   derefInt64(row.HeldBalance),
		})
	}

	r.SuccessJSON(resp)
}

func GetMyCreditsType(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	creditType := strings.TrimSpace(r.GinCtx.Param("type"))
	if creditType == "" {
		r.ErrorJSON(http.StatusBadRequest, "credit type required")
		return
	}

	bal, err := r.State.CreditsService.GetBalance(r.Request.Context(), user.ID, creditType)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "credit type not found")
		return
	}
	ct, err := r.State.CreditsService.GetCreditTypeByName(r.Request.Context(), creditType)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "credit type not found")
		return
	}

	r.SuccessJSON(creditBalanceResponse{
		Type:          creditType,
		DisplayName:   ct.DisplayName,
		Unit:          ct.Unit,
		DecimalPlaces: ct.DecimalPlaces,
		Balance:       bal.Balance,
		HeldBalance:   bal.HeldBalance,
	})
}

func GetMyCreditTransactions(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
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

	items, total, err := r.State.CreditsService.GetTransactions(r.Request.Context(), user.ID, creditType, limit, offset)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load transactions")
		return
	}
	r.SuccessJSONPaginated(items, int64(total), limit, offset)
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
