package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
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

	err := r.State.DB.Q(r.Request.Context()).NewSelect().
		TableExpr("billing.credit_types ct").
		ColumnExpr("ct.id as credit_type_id").
		ColumnExpr("ct.name").
		ColumnExpr("ct.display_name").
		ColumnExpr("ct.unit").
		ColumnExpr("ct.decimal_places").
		ColumnExpr("ucb.balance").
		ColumnExpr("ucb.held_balance").
		Join("LEFT JOIN billing.credit_balances ucb ON ucb.credit_type_id = ct.id AND ucb.user_id = ?", user.ID).
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
	creditType := strings.TrimSpace(r.Param("type"))
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
	creditType := strings.TrimSpace(r.Param("type"))
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

// parseUsageWindow parses the from/to query params for GetMyUsage. It accepts
// RFC3339 timestamps as well as plain YYYY-MM-DD dates (interpreted as UTC
// midnight). When a bound is omitted the window defaults to the current calendar
// month: [firstOfThisMonthUTC, now).
func parseUsageWindow(fromRaw, toRaw string, now time.Time) (from, to time.Time, err error) {
	now = now.UTC()
	fromRaw = strings.TrimSpace(fromRaw)
	toRaw = strings.TrimSpace(toRaw)

	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to = now
	if fromRaw != "" {
		if from, err = parseTimeParam(fromRaw); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if toRaw != "" {
		if to, err = parseTimeParam(toRaw); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	return from, to, nil
}

func parseTimeParam(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// GetMyUsage returns the authenticated payer's metered usage rolled up by
// event_type (endpoint/model) over a [from, to) window, with summed per-dimension
// counts (issue #289). The acting user is the delegated token's subject
// (r.GetUser()); their tenant subject is that subject's personal org
// (identity.TenantSubjectIDFromString), matching how the self-service surface resolves
// the payer. from/to accept RFC3339 timestamps or plain YYYY-MM-DD dates; when
// omitted the window defaults to the current calendar month [firstOfMonthUTC, now).
func GetMyUsage(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	payer := identity.TenantSubjectIDFromString(user.ID)
	if payer.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "payer could not be resolved from subject")
		return
	}

	from, to, err := parseUsageWindow(
		r.Request.URL.Query().Get("from"),
		r.Request.URL.Query().Get("to"),
		time.Now(),
	)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid from/to: use RFC3339 or YYYY-MM-DD")
		return
	}
	if !to.After(from) {
		r.ErrorJSON(http.StatusBadRequest, "to must be after from")
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	rows, err := svc.GetUsage(r.Request.Context(), payer, from, to)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load usage")
		return
	}
	r.SuccessJSON(rows)
}

// GetMyInvoices lists the authenticated payer's finalized monthly invoices,
// newest period first, paginated via limit/offset query params (issue #303). The
// acting user is the delegated token's subject (r.GetUser()); their tenant subject is
// that subject's personal org (identity.TenantSubjectIDFromString), matching how the
// rest of the self-service surface resolves the payer (mirrors GetMyUsage).
func GetMyInvoices(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	payer := identity.TenantSubjectIDFromString(user.ID)
	if payer.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "payer could not be resolved from subject")
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

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	items, total, err := svc.ListInvoices(r.Request.Context(), payer, limit, offset)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load invoices")
		return
	}
	r.SuccessJSONPaginated(items, int64(total), limit, offset)
}

// GetMyInvoice returns one of the authenticated payer's invoices with its line
// items, addressed by :id (issue #303). RLS + the payer filter scope it to the
// acting user's own invoices: an id belonging to another payer/tenant returns
// 404 (fail closed). The acting payer is resolved exactly as in GetMyInvoices.
func GetMyInvoice(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	payer := identity.TenantSubjectIDFromString(user.ID)
	if payer.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "payer could not be resolved from subject")
		return
	}

	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid invoice id")
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	inv, err := svc.GetInvoice(r.Request.Context(), payer, id)
	if err != nil {
		if repo.IsNotFound(err) {
			r.ErrorJSON(http.StatusNotFound, "invoice not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to load invoice")
		return
	}
	r.SuccessJSON(inv)
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
