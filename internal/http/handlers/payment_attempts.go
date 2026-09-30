package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ListPaymentAttempts lists the merchant's payment attempts, newest first
// (#1116).
//
//	GET /merchant/payment-attempts?kind&owner&category&reason&response_code&card_entry&psp_id&customer_id&checkout_id&subscription_id&cycle_id&since&until&limit&offset
func ListPaymentAttempts(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	q := queryReader{r: r}
	params := gen.ListPaymentAttemptsParams{
		MerchantID: mid, Kind: q.text("kind"), Owner: q.text("owner"), Category: q.text("category"), Reason: q.text("reason"),
		ResponseCode: q.text("response_code"), CardEntry: q.text("card_entry"), PspID: q.uuid("psp_id"), CustomerID: q.uuid("customer_id"),
		CheckoutID: q.uuid("checkout_id"), SubscriptionID: q.typed("subscription_id", func(s string) (uuid.UUID, error) {
			id, err := openrails.ParseSubscriptionID(s)
			return id.UUID(), err
		}), CycleID: q.typed("cycle_id", func(s string) (uuid.UUID, error) {
			id, err := openrails.ParseRebillCycleID(s)
			return id.UUID(), err
		}), Since: q.time("since"), Until: q.time("until"),
	}
	params.PageLimit, params.PageOffset = q.page()
	if q.err != nil {
		r.ErrorJSON(http.StatusBadRequest, q.err.Error())
		return
	}
	rows, err := r.State.DB.Gen(r.Request.Context()).ListPaymentAttempts(r.Request.Context(), params)
	if err != nil {
		r.InternalError("payment attempts could not be listed", err)
		return
	}
	out := openrails.Page[openrails.PaymentAttempt]{Object: "list", Data: make([]openrails.PaymentAttempt, 0, len(rows)), Limit: int(params.PageLimit), Offset: int(params.PageOffset)}
	for _, row := range rows {
		out.Data = append(out.Data, paymentAttemptToAPI(row.OpenrailsPaymentAttempt))
		out.Total = row.Total
	}
	out.HasMore = int64(out.Offset+len(out.Data)) < out.Total
	r.JSON(http.StatusOK, out)
}

// GetPaymentAttempt reads one payment attempt.
//
//	GET /merchant/payment-attempts/{id}
func GetPaymentAttempt(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	id, err := openrails.ParsePaymentAttemptID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment attempt id")
		return
	}
	row, err := r.State.DB.Gen(r.Request.Context()).GetPaymentAttempt(r.Request.Context(), gen.GetPaymentAttemptParams{MerchantID: mid, ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorJSON(http.StatusNotFound, "payment attempt not found")
		return
	}
	if err != nil {
		r.InternalError("payment attempt could not be read", err)
		return
	}
	r.JSON(http.StatusOK, paymentAttemptToAPI(row))
}

// ListRebillCycles lists the merchant's rebill cycles, latest due first
// (#1116).
//
//	GET /merchant/rebill-cycles?owner&first_outcome&miss_reason&outcome&psp_id&subscription_id&due_since&due_until&limit&offset
func ListRebillCycles(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	q := queryReader{r: r}
	outcome := q.text("outcome")
	if outcome != nil && *outcome != "collected" && *outcome != "lost" && *outcome != "open" {
		r.ErrorJSON(http.StatusBadRequest, `outcome must be "collected", "lost" or "open"`)
		return
	}
	now := r.Clock.Now()
	params := gen.ListRebillCyclesParams{
		MerchantID: mid, Now: now, Owner: q.text("owner"), FirstOutcome: q.text("first_outcome"), MissReason: q.text("miss_reason"), Outcome: outcome,
		PspID: q.uuid("psp_id"), SubscriptionID: q.typed("subscription_id", func(s string) (uuid.UUID, error) {
			id, err := openrails.ParseSubscriptionID(s)
			return id.UUID(), err
		}), DueSince: q.time("due_since"), DueUntil: q.time("due_until"),
	}
	params.PageLimit, params.PageOffset = q.page()
	if q.err != nil {
		r.ErrorJSON(http.StatusBadRequest, q.err.Error())
		return
	}
	rows, err := r.State.DB.Gen(r.Request.Context()).ListRebillCycles(r.Request.Context(), params)
	if err != nil {
		r.InternalError("rebill cycles could not be listed", err)
		return
	}
	out := openrails.Page[openrails.RebillCycle]{Object: "list", Data: make([]openrails.RebillCycle, 0, len(rows)), Limit: int(params.PageLimit), Offset: int(params.PageOffset)}
	for _, row := range rows {
		out.Data = append(out.Data, rebillCycleToAPI(row.OpenrailsRebillCycleFact, now))
		out.Total = row.Total
	}
	out.HasMore = int64(out.Offset+len(out.Data)) < out.Total
	r.JSON(http.StatusOK, out)
}

// GetRebillCycle reads one rebill cycle with its attempts, oldest first.
//
//	GET /merchant/rebill-cycles/{id}
func GetRebillCycle(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	id, err := openrails.ParseRebillCycleID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid rebill cycle id")
		return
	}
	ctx := r.Request.Context()
	q := r.State.DB.Gen(ctx)
	row, err := q.GetRebillCycle(ctx, gen.GetRebillCycleParams{MerchantID: mid, ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorJSON(http.StatusNotFound, "rebill cycle not found")
		return
	}
	if err != nil {
		r.InternalError("rebill cycle could not be read", err)
		return
	}
	attempts, err := q.ListCycleAttempts(ctx, gen.ListCycleAttemptsParams{MerchantID: mid, CycleID: id.UUID()})
	if err != nil {
		r.InternalError("rebill cycle attempts could not be read", err)
		return
	}
	out := rebillCycleToAPI(row, r.Clock.Now())
	for _, a := range attempts {
		out.Attempts = append(out.Attempts, paymentAttemptToAPI(a))
	}
	r.JSON(http.StatusOK, out)
}

func readScope(r *httprequest.Request) (uuid.UUID, bool) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment attempts unavailable")
		return uuid.Nil, false
	}
	mid, err := merchant.Require(r.Request.Context())
	if err != nil {
		r.InternalError("payment attempts unavailable", err)
		return uuid.Nil, false
	}
	return mid.UUID(), true
}

func paymentAttemptToAPI(a gen.OpenrailsPaymentAttempt) openrails.PaymentAttempt {
	out := openrails.PaymentAttempt{
		ID: openrails.PaymentAttemptID(a.ID), Object: "payment_attempt", Kind: a.Kind, Owner: a.Owner, CardEntry: a.CardEntry,
		Source: a.Source, ObservedVia: a.ObservedVia, Category: a.Category, Reason: openrails.DeclineReason(normalize.FromPtr(a.Reason)),
		Action: normalize.FromPtr(a.Action), ResponseCode: normalize.FromPtr(a.ResponseCode), ResponseText: normalize.FromPtr(a.ResponseText),
		IssuerCode: normalize.FromPtr(a.IssuerCode), IssuerText: normalize.FromPtr(a.IssuerText), AVSResult: normalize.FromPtr(a.AvsResult),
		CVVResult: normalize.FromPtr(a.CvvResult), CardBrand: normalize.FromPtr(a.CardBrand), CardLast4: normalize.FromPtr(a.CardLast4),
		CardBIN: normalize.FromPtr(a.CardBin), TokenType: normalize.FromPtr(a.TokenType), TransactionID: normalize.FromPtr(a.TransactionID),
		Rail: a.Rail, PSPID: a.PspID.String(), CustomerID: a.CustomerID.String(), Amount: a.Amount, Currency: normalize.FromPtr(a.Currency),
		AttemptedAt: a.AttemptedAt, CheckoutTarget: normalize.FromPtr(a.CheckoutTarget), EnrichedAt: a.EnrichedAt,
	}
	if a.CheckoutID != nil {
		out.CheckoutID = a.CheckoutID.String()
	}
	if a.CycleID != nil {
		id := openrails.RebillCycleID(*a.CycleID)
		out.CycleID = &id
	}
	if a.SubscriptionID != nil {
		id := openrails.SubscriptionID(*a.SubscriptionID)
		out.SubscriptionID = &id
	}
	if a.PaymentMethodID != nil {
		id := openrails.PaymentMethodID(*a.PaymentMethodID)
		out.PaymentMethodID = &id
	}
	if a.PaymentID != nil {
		id := openrails.PaymentID(*a.PaymentID)
		out.PaymentID = &id
	}
	return out
}

func rebillCycleToAPI(c gen.OpenrailsRebillCycleFact, now time.Time) openrails.RebillCycle {
	out := openrails.RebillCycle{
		ID: openrails.RebillCycleID(c.ID), Object: "rebill_cycle", SubscriptionID: openrails.SubscriptionID(c.SubscriptionID),
		CustomerID: c.CustomerID.String(), PSPID: c.PspID.String(), Rail: c.Rail, Owner: c.Owner, DueAt: c.DueAt, Amount: c.Amount,
		Currency: c.Currency, FirstOutcome: c.FirstOutcome, MissedAt: c.MissedAt, MissReason: normalize.FromPtr(c.MissReason),
		CollectedAt: c.WonAt, RecoveredBy: c.RecoveredBy, ClosesAt: c.ClosedAt,
	}
	switch {
	case c.WonAt != nil:
		out.Outcome = "collected"
	case !c.ClosedAt.After(now):
		out.Outcome = "lost"
	default:
		out.Outcome = "open"
	}
	return out
}

// queryReader reads optional list filters, keeping the first error.
type queryReader struct {
	r   *httprequest.Request
	err error
}

func (q *queryReader) text(key string) *string {
	if v := strings.TrimSpace(q.r.Query(key)); v != "" {
		return &v
	}
	return nil
}

func (q *queryReader) uuid(key string) *uuid.UUID {
	return q.typed(key, uuid.Parse)
}

func (q *queryReader) typed(key string, parse func(string) (uuid.UUID, error)) *uuid.UUID {
	v := strings.TrimSpace(q.r.Query(key))
	if v == "" {
		return nil
	}
	id, err := parse(v)
	if err != nil || id == uuid.Nil {
		q.fail(errors.New("invalid " + key))
		return nil
	}
	return &id
}

func (q *queryReader) time(key string) *time.Time {
	v := strings.TrimSpace(q.r.Query(key))
	if v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		q.fail(errors.New(key + " must be an RFC3339 time"))
		return nil
	}
	return &t
}

func (q *queryReader) page() (int64, int64) {
	return int64(min(max(parseIntDefault(q.r.Query("limit"), 50), 1), 200)), int64(max(parseIntDefault(q.r.Query("offset"), 0), 0))
}

func (q *queryReader) fail(err error) {
	if q.err == nil {
		q.err = err
	}
}
