//go:build e2e && integration

// Package testevents reads retained facts for domain assertions. Tests own any
// cursors; no test can acknowledge another reader's source history.
package testevents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
)

type Reader[Option any] interface {
	ListEvents(context.Context, billing.EventQuery, ...Option) (*billing.EventPage, error)
}

func Hosts[Option any](ctx context.Context, c Reader[Option], kind billing.HostEventType) (*billing.ListPage[billing.HostEvent], error) {
	out := &billing.ListPage[billing.HostEvent]{Items: []billing.HostEvent{}}
	query := billing.EventQuery{Limit: 500}
	if kind != "" {
		query.Types = []string{string(kind)}
	}
	for {
		page, err := c.ListEvents(ctx, query)
		if err != nil {
			return nil, err
		}
		for _, fact := range page.Events {
			if fact.Source != "host" {
				continue
			}
			id, err := billing.ParseHostEventID(fact.ID)
			if err != nil {
				return nil, err
			}
			event := billing.HostEvent{ID: id, MerchantID: fact.MerchantID, Type: billing.HostEventType(fact.Type), OccurredAt: fact.OccurredAt}
			var payload any
			switch event.Type {
			case billing.HostEventPaymentSettled:
				event.Payment = new(billing.PaymentSettledEvent)
				payload = event.Payment
			case billing.HostEventDelinquencyGrace, billing.HostEventDelinquencyEntered, billing.HostEventDelinquencyCleared:
				event.Delinquency = new(billing.DelinquencyHostEvent)
				payload = event.Delinquency
			case billing.HostEventProductEntitlementsChanged:
				event.ProductEntitlements = new(billing.ProductEntitlementsChangedEvent)
				payload = event.ProductEntitlements
			case billing.HostEventOrderCompleted, billing.HostEventOrderRequiresAction, billing.HostEventOrderPaymentFailed, billing.HostEventOrderCanceled, billing.HostEventOrderExpired, billing.HostEventOrderVoided:
				event.Order = new(billing.OrderHostEvent)
				payload = event.Order
			case billing.HostEventMandateRevoked:
				event.Mandate = new(billing.MandateHostEvent)
				payload = event.Mandate
			case billing.HostEventBalanceLow, billing.HostEventBalanceDepleted:
				event.Balance = new(billing.BalanceHostEvent)
				payload = event.Balance
			case billing.HostEventDestructiveHoldOpened, billing.HostEventDestructiveHoldCleared:
				event.DestructiveHold = new(billing.DestructiveHoldEvent)
				payload = event.DestructiveHold
			default:
				return nil, fmt.Errorf("unknown test host event %q", event.Type)
			}
			if err := json.Unmarshal(fact.Data, payload); err != nil {
				return nil, err
			}
			out.Items = append(out.Items, event)
		}
		if !page.HasMore {
			return out, nil
		}
		query.After = page.NextCursor
	}
}

type NotificationReader struct {
	read func(context.Context, billing.EventQuery) (*billing.EventPage, error)
}

func Notifications[Option any](c Reader[Option]) *NotificationReader {
	return &NotificationReader{read: func(ctx context.Context, q billing.EventQuery) (*billing.EventPage, error) {
		return c.ListEvents(ctx, q)
	}}
}
func (n *NotificationReader) Read(ctx context.Context) ([]billing.NotificationEvent, error) {
	return ReadNotifications(ctx, n.read)
}
func ReadNotifications(ctx context.Context, read func(context.Context, billing.EventQuery) (*billing.EventPage, error)) ([]billing.NotificationEvent, error) {
	out := []billing.NotificationEvent{}
	q := billing.EventQuery{Limit: 500}
	for {
		page, err := read(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, fact := range page.Events {
			if fact.Source != "notification" {
				continue
			}
			id, err := billing.ParseNotificationID(fact.ID)
			if err != nil {
				return nil, err
			}
			customer, err := uuid.Parse(fact.Subject.ID)
			if err != nil {
				return nil, err
			}
			var data billing.NotificationData
			if err := json.Unmarshal(fact.Data, &data); err != nil {
				return nil, err
			}
			out = append(out, billing.NotificationEvent{ID: id, MerchantID: fact.MerchantID, CustomerID: billing.CustomerID(customer), EventType: fact.Type, Data: fact.Data, CreatedAt: fact.OccurredAt, ActionRequested: billing.CustomerNotificationActionRequested(fact.Type), Optional: billing.CustomerNotificationOptional(fact.Type, data)})
		}
		if !page.HasMore {
			return out, nil
		}
		q.After = page.NextCursor
	}
}
