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
)

func invalidHostEventRequest(message string) error {
	return &billing.StatusError{Status: http.StatusBadRequest, ErrorDetails: billing.ErrorDetails{
		Type: "invalid_request_error", Code: "invalid_host_event_request", Message: message}}
}

func (s *Service) ListHostEvents(ctx context.Context, options billing.HostEventListOptions) ([]billing.HostEvent, error) {
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
	case "", billing.HostEventPaymentSettled, billing.HostEventDelinquencyGrace, billing.HostEventDelinquencyEntered, billing.HostEventDelinquencyCleared:
	default:
		return nil, invalidHostEventRequest("unknown host event type")
	}
	if options.Limit < 0 || options.Limit > billing.MaxHostEventPageSize {
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
	events := make([]billing.HostEvent, 0, len(rows))
	for _, row := range rows {
		event := billing.HostEvent{ID: row.ID, MerchantID: billing.MerchantID(row.MerchantID), Type: billing.HostEventType(row.EventType),
			OccurredAt: row.OccurredAt, AcknowledgedAt: row.DeliveredAt}
		switch event.Type {
		case billing.HostEventPaymentSettled:
			if row.PaymentID == nil || row.Amount == nil || row.PaymentCustomerID == nil || row.PaymentPriceID == nil {
				return nil, fmt.Errorf("host event %s has incomplete payment payload", row.ID)
			}
			event.Payment = &billing.PaymentSettledEvent{PaymentID: billing.PaymentID(*row.PaymentID), CustomerID: billing.CustomerID(*row.PaymentCustomerID).String(),
				PriceID: billing.PriceID(*row.PaymentPriceID).String(), Amount: *row.Amount, Currency: row.Currency}
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
			payload.CustomerID = (billing.CustomerID(row.SubjectID)).String()
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
		return &billing.StatusError{Status: http.StatusNotFound, ErrorDetails: billing.ErrorDetails{
			Type: "invalid_request_error", Code: "host_event_not_found", Message: "host event not found"}}
	}
	return nil
}
