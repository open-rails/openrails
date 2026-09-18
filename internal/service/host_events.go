package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

func invalidHostEventRequest(message string) error {
	return &openrails.StatusError{Status: http.StatusBadRequest, ErrorDetails: openrails.ErrorDetails{
		Type: "invalid_request_error", Code: "invalid_host_event_request", Message: message}}
}

func (s *Service) ListHostEvents(ctx context.Context, options openrails.HostEventListOptions) ([]openrails.HostEvent, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	switch options.Type {
	case "", openrails.HostEventPaymentSettled, openrails.HostEventDelinquencyGrace, openrails.HostEventDelinquencyEntered, openrails.HostEventDelinquencyCleared:
	default:
		return nil, invalidHostEventRequest("unknown host event type")
	}
	if options.Limit < 0 || options.Limit > openrails.MaxHostEventPageSize {
		return nil, invalidHostEventRequest("limit must be between 1 and 1000")
	}
	if options.Limit == 0 {
		options.Limit = 100
	}
	var paymentID *uuid.UUID
	if !options.PaymentID.IsZero() {
		id := options.PaymentID.UUID()
		paymentID = &id
	}
	rows, err := s.rt.DB.Gen(ctx).ListHostEvents(ctx, gen.ListHostEventsParams{
		MerchantID: mid.UUID(), EventType: string(options.Type), PaymentID: paymentID,
		IncludeAcknowledged: options.IncludeAcknowledged, RowLimit: int32(options.Limit),
	})
	if err != nil {
		return nil, err
	}
	events := make([]openrails.HostEvent, 0, len(rows))
	for _, row := range rows {
		event := openrails.HostEvent{ID: row.ID, MerchantID: merchant.ID(row.MerchantID), Type: openrails.HostEventType(row.EventType),
			OccurredAt: row.OccurredAt, AcknowledgedAt: row.DeliveredAt}
		switch event.Type {
		case openrails.HostEventPaymentSettled:
			if row.PaymentID == nil || row.Amount == nil || row.PaymentCustomerID == nil || row.PaymentPriceID == nil {
				return nil, fmt.Errorf("host event %s has incomplete payment payload", row.ID)
			}
			event.Payment = &openrails.PaymentSettledEvent{PaymentID: openrails.PaymentID(*row.PaymentID), CustomerID: openrails.CustomerID(*row.PaymentCustomerID),
				PriceID: openrails.PriceID(*row.PaymentPriceID), Amount: *row.Amount, Currency: row.Currency}
			if row.PaymentSubscriptionID != nil {
				subscriptionID := openrails.SubscriptionID(*row.PaymentSubscriptionID)
				event.Payment.SubscriptionID = &subscriptionID
			}
		case openrails.HostEventDelinquencyGrace, openrails.HostEventDelinquencyEntered, openrails.HostEventDelinquencyCleared:
			// Storage predates the public wire DTO and stores money as JSON
			// integers. Decode those fields explicitly before the Client emits
			// lossless decimal strings at the HTTP boundary.
			var payload struct {
				openrails.DelinquencyHostEvent
				OverdueAmount int64 `json:"overdue_amount"`
				AmountFloor   int64 `json:"amount_floor"`
			}
			if err := json.Unmarshal(row.Data, &payload); err != nil {
				return nil, fmt.Errorf("decode host event %s: %w", row.ID, err)
			}
			payload.CustomerID = openrails.CustomerID(row.SubjectID)
			payload.Currency = row.Currency
			payload.DelinquencyHostEvent.OverdueAmount = payload.OverdueAmount
			payload.DelinquencyHostEvent.AmountFloor = payload.AmountFloor
			event.Delinquency = &payload.DelinquencyHostEvent
		default:
			return nil, fmt.Errorf("unknown stored host event type %q", row.EventType)
		}
		events = append(events, event)
	}
	return events, nil
}

func (s *Service) AcknowledgeHostEvent(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return invalidHostEventRequest("host event id is required")
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	count, err := s.rt.DB.Gen(ctx).AcknowledgeHostEvent(ctx, gen.AcknowledgeHostEventParams{MerchantID: mid.UUID(), ID: id, Now: s.now().UTC()})
	if err != nil {
		return err
	}
	if count == 0 {
		return &openrails.StatusError{Status: http.StatusNotFound, ErrorDetails: openrails.ErrorDetails{
			Type: "invalid_request_error", Code: "host_event_not_found", Message: "host event not found"}}
	}
	return nil
}
