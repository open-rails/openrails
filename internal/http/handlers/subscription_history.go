package handlers

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/pagination"
)

type subscriptionHistoryCursor struct {
	Version      int       `json:"v"`
	Merchant     uuid.UUID `json:"m"`
	Subscription uuid.UUID `json:"s"`
	At           time.Time `json:"t"`
	Source       int32     `json:"k"`
	ID           uuid.UUID `json:"i"`
}

// GetSubscriptionHistory reads retained facts without executing billing work.
func GetSubscriptionHistory(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	id, err := billing.ParseSubscriptionID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid subscription ID")
		return
	}
	ctx := r.Request.Context()
	q := r.State.DB.Gen(ctx)
	_, err = q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: mid, ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorCode("subscription_not_found", "subscription not found")
		return
	}
	if err != nil {
		r.InternalError("subscription could not be read", err)
		return
	}
	request, ok := r.Page()
	if !ok {
		return
	}
	limit, err := pagination.Limit(request)
	if err != nil {
		writeRefusal(r, err, "invalid history page")
		return
	}
	params := gen.ListSubscriptionHistoryParams{MerchantID: mid, SubscriptionID: id.UUID(), RowLimit: pagination.Fetch(limit)}
	var cursor subscriptionHistoryCursor
	present, err := pagination.Decode(request.Cursor, &cursor)
	if err != nil || (present && (cursor.Version != 1 || cursor.Merchant != mid || cursor.Subscription != id.UUID() ||
		cursor.At.IsZero() || cursor.ID == uuid.Nil || cursor.Source < 1 || cursor.Source > 3)) {
		writeRefusal(r, pagination.ErrInvalidCursor, "invalid history cursor")
		return
	}
	if present {
		params.AfterAt, params.AfterSource, params.AfterID = &cursor.At, cursor.Source, cursor.ID
	}
	rows, err := q.ListSubscriptionHistory(ctx, params)
	if err != nil {
		r.InternalError("subscription history could not be read", err)
		return
	}
	page := pagination.Cut(rows, limit, func(row gen.ListSubscriptionHistoryRow) any {
		return subscriptionHistoryCursor{Version: 1, Merchant: mid, Subscription: id.UUID(), At: row.At, Source: row.Source, ID: row.ID}
	})
	r.SuccessJSON(billing.SubscriptionHistory{
		Events: pagination.Map(page, subscriptionHistoryEntry),
		Coverage: billing.SubscriptionHistoryCoverage{
			Sources:       []string{"subscription_transition", "payment_attempt", "provider_operation"},
			RetentionDays: 761,
			Gaps:          []string{"actor_attribution", "card_assignment_changes", "retry_schedule_changes", "pre_dispatch_holds"},
		},
	})
}

func subscriptionHistoryEntry(row gen.ListSubscriptionHistoryRow) billing.SubscriptionHistoryEntry {
	out := billing.SubscriptionHistoryEntry{At: row.At, RecordedAt: row.RecordedAt}
	switch row.Source {
	case 1:
		out.Kind = "subscription_transition"
		out.Transition = &billing.SubscriptionHistoryTransition{
			ToStatus: billing.SubscriptionStatus(row.ToStatus), Decision: row.Kind, CancelType: row.CancelType,
			FromPeriodEnd: row.FromPeriodEnd, ToPeriodEnd: row.ToPeriodEnd,
		}
		if row.FromStatus != "" {
			status := billing.SubscriptionStatus(row.FromStatus)
			out.Transition.FromStatus = &status
		}
	case 2:
		out.Kind = "payment_attempt"
		out.Attempt = &billing.SubscriptionHistoryAttempt{
			ID: billing.PaymentAttemptID(row.ID), Kind: row.Kind, Category: row.Category, Reason: row.Reason,
			Action: row.Action, Source: row.Origin, ObservedVia: row.ObservedVia, Amount: row.Amount, Currency: row.Currency,
		}
		if row.PspID != nil {
			out.Attempt.PSPID = billing.PSPID(*row.PspID)
		}
		if row.OrderID != nil {
			id := billing.OrderID(*row.OrderID)
			out.Attempt.OrderID = &id
		}
		if row.PaymentID != nil {
			id := billing.PaymentID(*row.PaymentID)
			out.Attempt.PaymentID = &id
		}
		if row.CycleID != nil {
			id := billing.RenewalID(*row.CycleID)
			out.Attempt.RenewalID = &id
		}
		if row.PaymentMethodID != nil {
			id := billing.PaymentMethodID(*row.PaymentMethodID)
			out.Attempt.PaymentMethodID = &id
		}
		if row.OperationID != nil {
			out.Attempt.OperationID = row.OperationID.String()
		}
	case 3:
		out.Kind = "provider_operation"
		out.Operation = &billing.SubscriptionHistoryOperation{
			ID: row.OperationID.String(), Kind: row.Kind, Phase: row.Category, Attempt: int(row.Attempt),
		}
	}
	out.ID = out.Kind + ":" + row.ID.String()
	return out
}
