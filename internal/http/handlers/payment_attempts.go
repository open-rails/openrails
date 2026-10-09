package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ListPaymentAttempts lists the merchant's payment attempts, newest first
// (#1116).
//
//	GET /merchant/payment-attempts?kind&owner&category&reason&response_code&card_entry&source&observed_via&avs_result&cvv_result&psp_id&customer_id&checkout_id&subscription_id&cycle_id&since&until&limit&offset
//
// A text filter takes one value or a comma-separated list.
func ListPaymentAttempts(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	ids, ok := listIDs(r, billing.ParsePaymentAttemptID)
	if !ok {
		return
	}
	if ids != nil {
		rows, err := r.State.DB.Gen(r.Request.Context()).ListPaymentAttemptsByIDs(r.Request.Context(), gen.ListPaymentAttemptsByIDsParams{MerchantID: mid, Ids: uuidutil.Of(ids)})
		if err != nil {
			r.InternalError("payment attempts could not be listed", err)
			return
		}
		r.SuccessJSON(pagination.Map(billing.ListPage[gen.BillingPaymentAttempt]{Items: rows}, paymentAttemptToAPI))
		return
	}
	q := queryReader{r: r}
	params := gen.ListPaymentAttemptsParams{
		MerchantID: mid, Kinds: q.list("kind"), Owners: q.list("owner"), Categories: q.list("category"), Reasons: q.list("reason"),
		ResponseCodes: q.list("response_code"), CardEntries: q.list("card_entry"), Sources: q.list("source"), ObservedVias: q.list("observed_via"),
		AvsResults: q.list("avs_result"), CvvResults: q.list("cvv_result"), PspID: q.typed("psp_id", parsePSPID), CustomerID: q.uuid("customer_id"),
		CheckoutID: q.uuid("checkout_id"), SubscriptionID: q.typed("subscription_id", func(s string) (uuid.UUID, error) {
			id, err := billing.ParseSubscriptionID(s)
			return id.UUID(), err
		}), CycleID: q.typed("cycle_id", func(s string) (uuid.UUID, error) {
			id, err := billing.ParseRebillCycleID(s)
			return id.UUID(), err
		}), Since: q.time("since"), Until: q.time("until"),
	}
	limit, ok := q.keyset(&params.AfterAt, &params.AfterID)
	if !ok {
		return
	}
	params.RowLimit = pagination.Fetch(limit)
	rows, err := r.State.DB.Gen(r.Request.Context()).ListPaymentAttempts(r.Request.Context(), params)
	if err != nil {
		r.InternalError("payment attempts could not be listed", err)
		return
	}
	page := pagination.Cut(rows, limit, func(a gen.BillingPaymentAttempt) any { return pagination.TimeID{At: a.AttemptedAt, ID: a.ID} })
	r.SuccessJSON(pagination.Map(page, paymentAttemptToAPI))
}

// GetPaymentAttempt reads one payment attempt.
//
//	GET /merchant/payment-attempts/{id}
func GetPaymentAttempt(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	id, err := billing.ParsePaymentAttemptID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid payment attempt id").WithParam("id"))
		return
	}
	row, err := r.State.DB.Gen(r.Request.Context()).GetPaymentAttempt(r.Request.Context(), gen.GetPaymentAttemptParams{MerchantID: mid, ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorCode(billing.CodeResourceNotFound, "payment attempt not found")
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
//
// A text filter takes one value or a comma-separated list.
func ListRebillCycles(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	ids, ok := listIDs(r, billing.ParseRebillCycleID)
	if !ok {
		return
	}
	if ids != nil {
		now := r.Clock.Now()
		rows, err := r.State.DB.Gen(r.Request.Context()).ListRebillCyclesByIDs(r.Request.Context(), gen.ListRebillCyclesByIDsParams{MerchantID: mid, Ids: uuidutil.Of(ids)})
		if err != nil {
			r.InternalError("rebill cycles could not be listed", err)
			return
		}
		r.SuccessJSON(pagination.Map(billing.ListPage[gen.ListRebillCyclesByIDsRow]{Items: rows}, func(c gen.ListRebillCyclesByIDsRow) billing.RebillCycle {
			return rebillCycleToAPI(gen.ListRebillCyclesRow(c), now)
		}))
		return
	}
	q := queryReader{r: r}
	outcomes := q.list("outcome")
	for _, o := range outcomes {
		if o != "collected" && o != "lost" && o != "open" {
			r.APIError(api.Coded(billing.CodeInvalidQuery, `outcome must be "collected", "lost" or "open"`).WithParam("outcome"))
			return
		}
	}
	now := r.Clock.Now()
	params := gen.ListRebillCyclesParams{
		MerchantID: mid, Now: now, Owners: q.list("owner"), FirstOutcomes: q.list("first_outcome"), MissReasons: q.list("miss_reason"), Outcomes: outcomes,
		PspID: q.typed("psp_id", parsePSPID), SubscriptionID: q.typed("subscription_id", func(s string) (uuid.UUID, error) {
			id, err := billing.ParseSubscriptionID(s)
			return id.UUID(), err
		}), DueSince: q.time("due_since"), DueUntil: q.time("due_until"),
	}
	limit, ok := q.keyset(&params.AfterAt, &params.AfterID)
	if !ok {
		return
	}
	params.RowLimit = pagination.Fetch(limit)
	rows, err := r.State.DB.Gen(r.Request.Context()).ListRebillCycles(r.Request.Context(), params)
	if err != nil {
		r.InternalError("rebill cycles could not be listed", err)
		return
	}
	page := pagination.Cut(rows, limit, func(c gen.ListRebillCyclesRow) any { return pagination.TimeID{At: c.DueAt, ID: c.ID} })
	r.SuccessJSON(pagination.Map(page, func(c gen.ListRebillCyclesRow) billing.RebillCycle { return rebillCycleToAPI(c, now) }))
}

// GetRebillCycle reads one rebill cycle with its attempts, oldest first.
//
//	GET /merchant/rebill-cycles/{id}
func GetRebillCycle(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	id, err := billing.ParseRebillCycleID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid rebill cycle id").WithParam("id"))
		return
	}
	ctx := r.Request.Context()
	q := r.State.DB.Gen(ctx)
	now := r.Clock.Now()
	rows, err := q.ListRebillCyclesByIDs(ctx, gen.ListRebillCyclesByIDsParams{MerchantID: mid, Ids: []uuid.UUID{id.UUID()}})
	if err != nil {
		r.InternalError("rebill cycle could not be read", err)
		return
	}
	if len(rows) == 0 {
		r.ErrorCode(billing.CodeResourceNotFound, "rebill cycle not found")
		return
	}
	attempts, err := q.ListCycleAttempts(ctx, gen.ListCycleAttemptsParams{MerchantID: mid, CycleID: id.UUID()})
	if err != nil {
		r.InternalError("rebill cycle attempts could not be read", err)
		return
	}
	out := rebillCycleToAPI(gen.ListRebillCyclesRow(rows[0]), now)
	out.Attempts = make([]billing.PaymentAttempt, 0, len(attempts))
	for _, a := range attempts {
		out.Attempts = append(out.Attempts, paymentAttemptToAPI(a))
	}
	r.JSON(http.StatusOK, out)
}

func readScope(r *httprequest.Request) (uuid.UUID, bool) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorCode(billing.CodeInternalError, "payment attempts unavailable")
		return uuid.Nil, false
	}
	mid, err := merchant.Require(r.Request.Context())
	if err != nil {
		r.InternalError("payment attempts unavailable", err)
		return uuid.Nil, false
	}
	return mid.UUID(), true
}

func paymentAttemptToAPI(a gen.BillingPaymentAttempt) billing.PaymentAttempt {
	out := billing.PaymentAttempt{
		ID: billing.PaymentAttemptID(a.ID), Kind: a.Kind, Owner: a.Owner, CardEntry: a.CardEntry,
		Source: a.Source, ObservedVia: a.ObservedVia, Category: a.Category, Reason: billing.DeclineReason(normalize.FromPtr(a.Reason)),
		Action: normalize.FromPtr(a.Action), ResponseCode: normalize.FromPtr(a.ResponseCode), ResponseText: normalize.FromPtr(a.ResponseText),
		IssuerCode: normalize.FromPtr(a.IssuerCode), IssuerText: normalize.FromPtr(a.IssuerText), AVSResult: normalize.FromPtr(a.AvsResult),
		CVVResult: normalize.FromPtr(a.CvvResult), Card: models.CardFromColumns(a.CardBrand, a.CardLast4, nil, nil).Details(),
		CardBIN: normalize.FromPtr(a.CardBin), TokenType: normalize.FromPtr(a.TokenType), TransactionID: normalize.FromPtr(a.TransactionID),
		Rail: a.Rail, PSPID: billing.PSPID(a.PspID), CustomerID: billing.CustomerID(a.CustomerID), Amount: a.Amount, Currency: normalize.FromPtr(a.Currency),
		AttemptedAt: a.AttemptedAt, CheckoutTarget: normalize.FromPtr(a.CheckoutTarget), EnrichedAt: a.EnrichedAt,
	}
	if a.CheckoutID != nil {
		out.CheckoutID = a.CheckoutID.String()
	}
	if a.CycleID != nil {
		id := billing.RebillCycleID(*a.CycleID)
		out.CycleID = &id
	}
	if a.SubscriptionID != nil {
		id := billing.SubscriptionID(*a.SubscriptionID)
		out.SubscriptionID = &id
	}
	if a.PaymentMethodID != nil {
		id := billing.PaymentMethodID(*a.PaymentMethodID)
		out.PaymentMethodID = &id
	}
	if a.PaymentID != nil {
		id := billing.PaymentID(*a.PaymentID)
		out.PaymentID = &id
	}
	return out
}

func rebillCycleToAPI(c gen.ListRebillCyclesRow, now time.Time) billing.RebillCycle {
	out := billing.RebillCycle{
		ID: billing.RebillCycleID(c.ID), SubscriptionID: billing.SubscriptionID(c.SubscriptionID),
		CustomerID: billing.CustomerID(c.CustomerID), PSPID: billing.PSPID(c.PspID), Rail: c.Rail, Owner: c.Owner, DueAt: c.DueAt, Amount: c.Amount,
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

// list is a filter's values, comma-separated; nil when absent.
func (q *queryReader) list(key string) []string {
	var out []string
	for _, v := range strings.Split(q.r.Query(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
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

// keyset reads the page after the filters, writing the first refusal: the
// limit, and the (time, id) cursor into the query's nullable arguments.
func (q *queryReader) keyset(afterAt **time.Time, afterID **uuid.UUID) (int, bool) {
	if q.err != nil {
		q.r.APIError(api.Coded(billing.CodeInvalidQuery, q.err.Error()))
		return 0, false
	}
	page, ok := q.r.Page()
	if !ok {
		return 0, false
	}
	var err error
	if *afterAt, *afterID, err = pagination.After(page.Cursor); err != nil {
		writeRefusal(q.r, err, "invalid cursor")
		return 0, false
	}
	return page.Limit, true
}

func (q *queryReader) fail(err error) {
	if q.err == nil {
		q.err = err
	}
}

func parsePSPID(s string) (uuid.UUID, error) {
	id, err := billing.ParsePSPID(s)
	return id.UUID(), err
}
