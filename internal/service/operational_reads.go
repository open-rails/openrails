package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
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

func readInspectionPage(kind string, page billing.PageRequest, from, to, defaultFrom, now time.Time, maxSpan time.Duration) (inspectionPage, error) {
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
		if maxSpan > 0 {
			out.from = out.to.Add(-maxSpan)
		}
	}
	if !out.from.Before(out.to) {
		return out, invalidInspectionQuery("from", "from must be before to")
	}
	if maxSpan > 0 && out.to.Sub(out.from) > maxSpan+time.Microsecond {
		return out, invalidInspectionQuery("from", "a usage event list covers at most 31 days; request successive windows")
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

// ListAdmissions inspects durable admitted requests. It does not reevaluate
// policy, extend a reservation, or materialize expiry as a write.
func (s *Service) ListAdmissions(ctx context.Context, params billing.AdmissionListParams) (*billing.ListPage[billing.Admission], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	now := s.now().UTC()
	oldest := retention.AdmissionsDropBefore(now)
	page, err := readInspectionPage("admissions", params.PageRequest, params.From, params.To, oldest, now, 0)
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
	args := gen.ListAdmissionOperationsParams{MerchantID: mid.UUID(), FromAt: retainedFrom(page.from, oldest), ToAt: page.to,
		AsOf: now, Currency: currency, State: optionalInspectionText(string(params.State)), RowLimit: pagination.Fetch(page.limit)}
	if !params.CustomerID.IsZero() {
		customer := params.CustomerID.UUID()
		args.CustomerID = &customer
	}
	if page.after != nil {
		args.AfterAt, args.AfterRequestID = &page.after.At, &page.after.Key
	}
	rows, err := s.rt.DB.Gen(ctx).ListAdmissionOperations(ctx, args)
	if err != nil {
		return nil, err
	}
	cut := pagination.Cut(rows, page.limit, func(row gen.BillingAdmissionOperation) any {
		return page.position("admissions", row.AdmittedAt, row.RequestID)
	})
	out := pagination.Map(cut, func(row gen.BillingAdmissionOperation) billing.Admission { return *admissionFromOperation(row, now) })
	return &out, nil
}

// GetUsageEvent reads an existing event within retained partitions. The read
// never rates usage or posts to the ledger.
func (s *Service) GetUsageEvent(ctx context.Context, id billing.UsageEventID) (*billing.UsageEvent, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	row, err := s.rt.DB.Gen(ctx).GetUsageEventByID(ctx, gen.GetUsageEventByIDParams{MerchantID: mid.UUID(), ID: id.UUID(), FromAt: retention.UsageDropBefore(now), ToAt: now.Add(retention.UsageClockSkew)})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "usage event not found")
	}
	if err != nil {
		return nil, err
	}
	event, err := money.UsageEventFromRow(row)
	if err != nil {
		return nil, err
	}
	return usageEventReceipt(event), nil
}

// ListUsageEvents is a bounded read of individual usage facts, not aggregates.
func (s *Service) ListUsageEvents(ctx context.Context, params billing.UsageEventListParams) (*billing.ListPage[billing.UsageEvent], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	now := s.now().UTC()
	const maxSpan = 31 * retention.Day
	page, err := readInspectionPage("usage-events", params.PageRequest, params.From, params.To, now.Add(time.Microsecond).Add(-maxSpan), now, maxSpan)
	if err != nil {
		return nil, err
	}
	switch params.Outcome {
	case "", billing.UsageSucceeded, billing.UsageFailed:
	default:
		return nil, invalidInspectionQuery("outcome", "outcome must be succeeded or failed")
	}
	params.Source, params.SourceID, params.EventType = strings.TrimSpace(params.Source), strings.TrimSpace(params.SourceID), strings.TrimSpace(params.EventType)
	if params.SourceID != "" && params.Source == "" {
		return nil, invalidInspectionQuery("source_id", "source_id requires source")
	}
	if len(params.SourceID) > 255 {
		return nil, invalidInspectionQuery("source_id", "source_id must be at most 255 bytes")
	}
	currency, err := inspectionCurrency(params.Currency)
	if err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	args := gen.ListUsageEventsParams{MerchantID: mid.UUID(), FromAt: retainedFrom(page.from, retention.UsageDropBefore(now)), ToAt: page.to,
		Currency: currency, EventType: optionalInspectionText(params.EventType), Source: optionalInspectionText(params.Source), SourceID: optionalInspectionText(params.SourceID), Outcome: optionalInspectionText(string(params.Outcome)), RowLimit: pagination.Fetch(page.limit)}
	if !params.CustomerID.IsZero() {
		customer := params.CustomerID.UUID()
		args.CustomerID = &customer
	}
	if page.after != nil {
		id, err := uuid.Parse(page.after.Key)
		if err != nil || id == uuid.Nil {
			return nil, pagination.ErrInvalidCursor
		}
		args.AfterAt, args.AfterID = &page.after.At, &id
	}
	rows, err := s.rt.DB.Gen(ctx).ListUsageEvents(ctx, args)
	if err != nil {
		return nil, err
	}
	cut := pagination.Cut(rows, page.limit, func(row gen.BillingUsageEvent) any {
		return page.position("usage-events", row.OccurredAt, row.ID.String())
	})
	out := billing.ListPage[billing.UsageEvent]{Items: make([]billing.UsageEvent, 0, len(cut.Items)), Next: cut.Next}
	for _, row := range cut.Items {
		event, err := money.UsageEventFromRow(row)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, *usageEventReceipt(event))
	}
	return &out, nil
}
