package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ListPaymentAttempts lists the merchant's payment attempts, newest first
// (#1116).
//
//	GET /admin/payment-attempts?kind&owner&category&reason&response_code&card_entry&source&observed_via&avs_result&cvv_result&psp_id&customer_id&payment_id&invoice_id&order_id&subscription_id&cycle_id&since&until&limit&offset
//
// A text filter takes one value or a comma-separated list.
func ListPaymentAttempts(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	var query billing.PaymentAttemptListParams
	if !bindList(r, &query, &query.PageRequest) {
		return
	}
	ids, ok := listIDs(r, query.IDs)
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
	params := gen.ListPaymentAttemptsParams{
		MerchantID: mid, Kinds: filled(query.Kind), Owners: filled(query.Owner), Categories: filled(query.Category), Reasons: filled(query.Reason),
		ResponseCodes: filled(query.ResponseCode), CardEntries: filled(query.CardEntry), Sources: filled(query.Source), ObservedVias: filled(query.ObservedVia),
		AvsResults: filled(query.AVSResult), CvvResults: filled(query.CVVResult), PspID: idOf(query.PSPID), CustomerID: idOf(query.CustomerID),
		PaymentID: idOf(query.PaymentID), OrderID: idOf(query.OrderID), SubscriptionID: idOf(query.SubscriptionID), CycleID: idOf(query.CycleID),
		Since: instant(query.Since), Until: instant(query.Until),
	}
	limit, ok := keyset(r, query.PageRequest, &params.AfterAt, &params.AfterID)
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
//	GET /admin/payment-attempts/{id}
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

// An engine renewal nothing attempted by its deadline is listed as missed.
var (
	engineRenewalDeadline = metrics.EngineRenewalDeadline.Seconds()
	solanaRenewalDeadline = metrics.SolanaRenewalDeadline.Seconds()
)

// ListRenewals lists the merchant's renewals, latest due first.
//
//	GET /admin/renewals?owner&first_outcome&miss_reason&outcome&psp_id&subscription_id&due_since&due_until&limit&offset
//
// A text filter takes one value or a comma-separated list.
func ListRenewals(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	var query billing.RenewalListParams
	if !bindList(r, &query, &query.PageRequest) {
		return
	}
	ids, ok := listIDs(r, query.IDs)
	if !ok {
		return
	}
	if ids != nil {
		now := r.Clock.Now()
		rows, err := r.State.DB.Gen(r.Request.Context()).ListRebillCyclesByIDs(r.Request.Context(), gen.ListRebillCyclesByIDsParams{
			MerchantID: mid, Ids: uuidutil.Of(ids), Now: now, EngineDeadlineSeconds: engineRenewalDeadline, SolanaDeadlineSeconds: solanaRenewalDeadline,
		})
		if err != nil {
			r.InternalError("renewals could not be listed", err)
			return
		}
		r.SuccessJSON(pagination.Map(billing.ListPage[gen.ListRebillCyclesByIDsRow]{Items: rows}, func(c gen.ListRebillCyclesByIDsRow) billing.Renewal {
			return rebillCycleToAPI(gen.ListRebillCyclesRow(c), now)
		}))
		return
	}
	outcomes := filled(query.Outcome)
	for _, o := range outcomes {
		if o != "collected" && o != "lost" && o != "open" {
			r.APIError(api.Coded(billing.CodeInvalidQuery, `outcome must be "collected", "lost" or "open"`).WithParam("outcome"))
			return
		}
	}
	now := r.Clock.Now()
	params := gen.ListRebillCyclesParams{
		MerchantID: mid, Now: now, EngineDeadlineSeconds: engineRenewalDeadline, SolanaDeadlineSeconds: solanaRenewalDeadline, Owners: filled(query.Owner), FirstOutcomes: filled(query.FirstOutcome), MissReasons: filled(query.MissReason), Outcomes: outcomes,
		PspID: idOf(query.PSPID), SubscriptionID: idOf(query.SubscriptionID), DueSince: instant(query.DueSince), DueUntil: instant(query.DueUntil),
	}
	limit, ok := keyset(r, query.PageRequest, &params.AfterAt, &params.AfterID)
	if !ok {
		return
	}
	params.RowLimit = pagination.Fetch(limit)
	rows, err := r.State.DB.Gen(r.Request.Context()).ListRebillCycles(r.Request.Context(), params)
	if err != nil {
		r.InternalError("renewals could not be listed", err)
		return
	}
	page := pagination.Cut(rows, limit, func(c gen.ListRebillCyclesRow) any { return pagination.TimeID{At: c.DueAt, ID: c.ID} })
	r.SuccessJSON(pagination.Map(page, func(c gen.ListRebillCyclesRow) billing.Renewal { return rebillCycleToAPI(c, now) }))
}

// GetRenewal reads one renewal with its attempts, oldest first.
//
//	GET /admin/renewals/{id}
func GetRenewal(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	id, err := billing.ParseRenewalID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid renewal id").WithParam("id"))
		return
	}
	ctx := r.Request.Context()
	q := r.State.DB.Gen(ctx)
	now := r.Clock.Now()
	rows, err := q.ListRebillCyclesByIDs(ctx, gen.ListRebillCyclesByIDsParams{
		MerchantID: mid, Ids: []uuid.UUID{id.UUID()}, Now: now, EngineDeadlineSeconds: engineRenewalDeadline, SolanaDeadlineSeconds: solanaRenewalDeadline,
	})
	if err != nil {
		r.InternalError("renewal could not be read", err)
		return
	}
	if len(rows) == 0 {
		r.ErrorCode(billing.CodeResourceNotFound, "renewal not found")
		return
	}
	attempts, err := q.ListCycleAttempts(ctx, gen.ListCycleAttemptsParams{MerchantID: mid, CycleID: id.UUID()})
	if err != nil {
		r.InternalError("renewal attempts could not be read", err)
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
		Rail: billing.Rail(a.Rail), PSPID: billing.PSPID(a.PspID), CustomerID: billing.CustomerID(a.CustomerID), Amount: a.Amount, Currency: normalize.FromPtr(a.Currency),
		AttemptedAt: a.AttemptedAt,
	}
	if a.OrderID != nil {
		id := billing.OrderID(*a.OrderID)
		out.OrderID = &id
	}
	if a.CycleID != nil {
		id := billing.RenewalID(*a.CycleID)
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
	if a.MandateID != nil {
		id := billing.MandateID(*a.MandateID)
		out.MandateID = &id
	}
	out.SentInitialTransactionID = normalize.FromPtr(a.SentInitialTransactionID)
	return out
}

func rebillCycleToAPI(c gen.ListRebillCyclesRow, now time.Time) billing.Renewal {
	out := billing.Renewal{
		ID: billing.RenewalID(c.ID), SubscriptionID: billing.SubscriptionID(c.SubscriptionID),
		CustomerID: billing.CustomerID(c.CustomerID), PSPID: billing.PSPID(c.PspID), Rail: billing.Rail(c.Rail), Owner: c.Owner, DueAt: c.DueAt, Amount: c.Amount,
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

// filled is a comma-separated filter's values without empty ones; nil
// when none.
func filled(values []string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// idOf is a typed id's UUID, nil when zero.
func idOf(id interface{ UUID() uuid.UUID }) *uuid.UUID {
	u := id.UUID()
	if u == uuid.Nil {
		return nil
	}
	return &u
}

// keyset reads the (time, id) cursor of page into the query's nullable
// arguments, and answers its limit.
func keyset(r *httprequest.Request, page billing.PageRequest, afterAt **time.Time, afterID **uuid.UUID) (int, bool) {
	var err error
	if *afterAt, *afterID, err = pagination.After(page.Cursor); err != nil {
		writeRefusal(r, err, "invalid cursor")
		return 0, false
	}
	return page.Limit, true
}
