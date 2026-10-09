package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

func invalidHostEventRequest(message string) error {
	return apperr.New(http.StatusBadRequest, "invalid_host_event_request", message)
}

func (s *Service) ListHostEvents(ctx context.Context, req billing.HostEventListParams) (billing.ListPage[billing.HostEvent], error) {
	switch req.Type {
	case "", billing.HostEventPaymentSettled, billing.HostEventDelinquencyGrace, billing.HostEventDelinquencyEntered, billing.HostEventDelinquencyCleared, billing.HostEventProductEntitlementsChanged:
	default:
		return billing.ListPage[billing.HostEvent]{}, invalidHostEventRequest("unknown host event type")
	}
	limit, err := pagination.Limit(req.PageRequest)
	if err != nil {
		return billing.ListPage[billing.HostEvent]{}, err
	}
	var after struct {
		ID uuid.UUID `json:"i"`
	}
	present, err := pagination.Decode(req.Cursor, &after)
	if err != nil {
		return billing.ListPage[billing.HostEvent]{}, err
	}
	params := gen.ListHostEventsParams{EventType: string(req.Type), IncludeAcknowledged: req.IncludeAcknowledged, RowLimit: pagination.Fetch(limit)}
	if present {
		params.AfterID = &after.ID
	}
	if !req.PaymentID.IsZero() {
		id := req.PaymentID.UUID()
		params.PaymentID = &id
	}
	events, err := s.hostEvents(ctx, params)
	if err != nil {
		return billing.ListPage[billing.HostEvent]{}, err
	}
	return pagination.Cut(events, limit, func(e billing.HostEvent) any {
		return struct {
			ID uuid.UUID `json:"i"`
		}{e.ID.UUID()}
	}), nil
}

// hostEvents reads host events with their payloads.
func (s *Service) hostEvents(ctx context.Context, params gen.ListHostEventsParams) ([]billing.HostEvent, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	params.MerchantID = mid.UUID()
	rows, err := s.rt.DB.Gen(ctx).ListHostEvents(ctx, params)
	if err != nil {
		return nil, err
	}
	events := make([]billing.HostEvent, 0, len(rows))
	for _, row := range rows {
		event := billing.HostEvent{ID: billing.HostEventID(row.ID), MerchantID: billing.MerchantID(row.MerchantID), Type: billing.HostEventType(row.EventType),
			OccurredAt: row.OccurredAt, AcknowledgedAt: row.DeliveredAt}
		switch event.Type {
		case billing.HostEventPaymentSettled:
			if row.PaymentID == nil || row.Amount == nil || row.PaymentCustomerID == nil || row.PaymentPriceID == nil {
				return nil, fmt.Errorf("host event %s has incomplete payment payload", row.ID)
			}
			event.Payment = &billing.PaymentSettledEvent{PaymentID: billing.PaymentID(*row.PaymentID), CustomerID: billing.CustomerID(*row.PaymentCustomerID),
				PriceID: billing.PriceID(*row.PaymentPriceID), Amount: *row.Amount, Currency: derefString(row.Currency)}
			if row.PaymentSubscriptionID != nil {
				subscriptionID := billing.SubscriptionID(*row.PaymentSubscriptionID)
				event.Payment.SubscriptionID = &subscriptionID
			}
		case billing.HostEventDelinquencyGrace, billing.HostEventDelinquencyEntered, billing.HostEventDelinquencyCleared:
			// Storage predates the public wire DTO and stores money as JSON
			// integers. Decode those fields explicitly before the Client emits
			// lossless decimal strings at the HTTP boundary.
			var payload struct {
				billing.DelinquencyHostEvent
				OverdueAmount int64 `json:"overdue_amount"`
				AmountFloor   int64 `json:"amount_floor"`
			}
			if err := json.Unmarshal(row.Data, &payload); err != nil {
				return nil, fmt.Errorf("decode host event %s: %w", row.ID, err)
			}
			payload.CustomerID = billing.CustomerID(row.SubjectID)
			payload.Currency = derefString(row.Currency)
			payload.DelinquencyHostEvent.OverdueAmount = payload.OverdueAmount
			payload.DelinquencyHostEvent.AmountFloor = payload.AmountFloor
			event.Delinquency = &payload.DelinquencyHostEvent
		case billing.HostEventProductEntitlementsChanged:
			var payload billing.ProductEntitlementsChangedEvent
			if err := json.Unmarshal(row.Data, &payload); err != nil {
				return nil, fmt.Errorf("decode host event %s: %w", row.ID, err)
			}
			payload.ProductID = billing.ProductID(row.SubjectID)
			event.ProductEntitlements = &payload
		default:
			return nil, fmt.Errorf("unknown stored host event type %q", row.EventType)
		}
		events = append(events, event)
	}
	return events, nil
}

// AcknowledgeHostEvents acknowledges the merchant's host events; one that
// does not exist maps to nil. Acknowledging again changes nothing.
func (s *Service) AcknowledgeHostEvents(ctx context.Context, ids []billing.HostEventID) (map[billing.HostEventID]*billing.HostEvent, error) {
	if len(ids) == 0 || len(ids) > billing.MaxBatchItems {
		return nil, invalidHostEventRequest(fmt.Sprintf("acknowledge 1 to %d host events", billing.MaxBatchItems))
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]uuid.UUID, len(ids))
	out := make(map[billing.HostEventID]*billing.HostEvent, len(ids))
	for i, id := range ids {
		keys[i] = id.UUID()
		out[id] = nil
	}
	acknowledged, err := s.rt.DB.Gen(ctx).AcknowledgeHostEvents(ctx, gen.AcknowledgeHostEventsParams{MerchantID: mid.UUID(), Ids: keys, Now: s.now().UTC()})
	if err != nil {
		return nil, err
	}
	if len(acknowledged) == 0 {
		return out, nil
	}
	events, err := s.hostEvents(ctx, gen.ListHostEventsParams{Ids: acknowledged, IncludeAcknowledged: true, RowLimit: billing.MaxBatchItems})
	if err != nil {
		return nil, err
	}
	for i := range events {
		out[events[i].ID] = &events[i]
	}
	return out, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
