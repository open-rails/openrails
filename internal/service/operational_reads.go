package service

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// inspectionCursor keeps the time window fixed while traversing a keyset.
// Filters remain explicit request parameters, as on other merchant lists.
type inspectionCursor struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	Key  string    `json:"key"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type inspectionPage struct {
	limit    int
	from, to time.Time
	after    *inspectionCursor
}

func invalidInspectionQuery(param, message string) error {
	return apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, message).WithParam(param)
}

func readInspectionPage(kind string, page billing.PageRequest, from, to, defaultFrom, now time.Time) (inspectionPage, error) {
	out := inspectionPage{from: from.UTC(), to: to.UTC()}
	var err error
	if out.limit, err = pagination.Limit(page); err != nil {
		return out, err
	}
	var after inspectionCursor
	present, err := pagination.Decode(page.Cursor, &after)
	if err != nil {
		return out, err
	}
	if present {
		if after.Kind != kind || after.At.IsZero() || after.Key == "" || after.From.IsZero() || after.To.IsZero() ||
			(!from.IsZero() && !from.Equal(after.From)) || (!to.IsZero() && !to.Equal(after.To)) {
			return out, pagination.ErrInvalidCursor
		}
		out.from, out.to, out.after = after.From.UTC(), after.To.UTC(), &after
	}
	if out.to.IsZero() {
		out.to = now.UTC().Add(time.Microsecond)
	}
	if out.from.IsZero() {
		out.from = defaultFrom.UTC()
	}
	if !out.from.Before(out.to) {
		return out, invalidInspectionQuery("from", "from must be before to")
	}
	if out.after != nil && (out.after.At.Before(out.from) || !out.after.At.Before(out.to)) {
		return out, pagination.ErrInvalidCursor
	}
	return out, nil
}

func (p inspectionPage) position(kind string, at time.Time, key string) inspectionCursor {
	return inspectionCursor{Kind: kind, At: at, Key: key, From: p.from, To: p.to}
}

func optionalInspectionText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func inspectionCurrency(currency string) (*string, error) {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return nil, nil
	}
	normalized, err := requireCurrency(currency)
	return &normalized, err
}

func retainedFrom(from, oldest time.Time) time.Time {
	if from.Before(oldest) {
		return oldest
	}
	return from
}

// ListAdmissions inspects admitted requests' holds. It does not reevaluate
// policy, extend a hold, or materialize expiry as a write.
func (s *Service) ListAdmissions(ctx context.Context, params billing.AdmissionListParams) (*billing.ListPage[billing.Admission], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	now := s.now().UTC()
	oldest := retention.AdmissionsDropBefore(now)
	page, err := readInspectionPage("admissions", params.PageRequest, params.From, params.To, oldest, now)
	if err != nil {
		return nil, err
	}
	switch params.State {
	case "", billing.AdmissionOpen, billing.AdmissionExpired, billing.AdmissionReleased, billing.AdmissionCaptured:
	default:
		return nil, invalidInspectionQuery("state", "state must be open, expired, released or captured")
	}
	currency, err := inspectionCurrency(params.Currency)
	if err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	args := gen.ListRequestHoldsParams{MerchantID: mid.UUID(), FromAt: retainedFrom(page.from, oldest), ToAt: page.to,
		AsOf: now, Currency: currency, State: optionalInspectionText(string(params.State)), RowLimit: pagination.Fetch(page.limit)}
	if !params.CustomerID.IsZero() {
		customer := params.CustomerID.UUID()
		args.CustomerID = &customer
	}
	if page.after != nil {
		args.AfterAt, args.AfterRequestID = &page.after.At, &page.after.Key
	}
	rows, err := s.rt.DB.Gen(ctx).ListRequestHolds(ctx, args)
	if err != nil {
		return nil, err
	}
	cut := pagination.Cut(rows, page.limit, func(row gen.BillingLimitHold) any {
		return page.position("admissions", row.CreatedAt, *row.RequestID)
	})
	out := pagination.Map(cut, func(row gen.BillingLimitHold) billing.Admission { return *admissionFromHold(row, now) })
	return &out, nil
}
