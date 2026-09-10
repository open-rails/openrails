package webhooks

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

type recordingConvergeEnqueuer struct {
	requests []ConvergeRequest
}

func (e *recordingConvergeEnqueuer) EnqueueSubscriptionConverge(_ context.Context, req ConvergeRequest) error {
	e.requests = append(e.requests, req)
	return nil
}

func TestHandleNMIWebhookDispatchesSubscriptionSignals(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		body      any
		reference string
	}{
		{
			name:      "recurring event",
			eventType: EventTypeNMIUpdateSubscription,
			body:      NMIRecurringEventBody{SubscriptionID: " sub_recurring "},
			reference: "sub_recurring",
		},
		{
			name:      "transaction event",
			eventType: EventTypeNMITransactionSuccess,
			body:      NMITransactionEventBody{OrderID: " sub_transaction "},
			reference: "sub_transaction",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.body)
			require.NoError(t, err)
			enqueuer := &recordingConvergeEnqueuer{}
			eventID := uuid.NewString()
			svc := &NMIWebhookService{
				Rail: "nmi",
				Data: NMIWebhookEvent{
					EventID:   eventID,
					EventType: tc.eventType,
					EventBody: body,
				},
				ConvergeEnqueuer: enqueuer,
			}

			require.NoError(t, svc.HandleNMIWebhook(dbtest.WithTestMerchant(context.Background())))
			require.Equal(t, []ConvergeRequest{{
				MerchantID:            dbtest.TestMerchantID.UUID(),
				Rail:                  "nmi",
				SubscriptionReference: tc.reference,
				EventType:             tc.eventType,
			}}, enqueuer.requests)
		})
	}
}

func TestHandleNMIWebhookRejectsUnsupportedEvent(t *testing.T) {
	svc := &NMIWebhookService{Data: NMIWebhookEvent{EventID: uuid.NewString(), EventType: "unsupported", EventBody: []byte(`{}`)}}
	err := svc.HandleNMIWebhook(context.Background())
	require.ErrorContains(t, err, "unsupported event type")
	require.True(t, IsWebhookErrorNonRetryable(err))
}

type recordingCheckoutSessionStore struct {
	failedID      uuid.UUID
	failedMessage string
	failedCode    string
	expiredID     uuid.UUID
	expiredReason string
}

func (*recordingCheckoutSessionStore) FindOpenCCBillReservation(context.Context, string, string, uuid.UUID) (*models.CheckoutSession, error) {
	return nil, nil
}

func (*recordingCheckoutSessionStore) FindOpenByUserPriceRail(context.Context, string, uuid.UUID, models.Rail) (*models.CheckoutSession, error) {
	return nil, nil
}

func (*recordingCheckoutSessionStore) MarkSucceeded(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

func (*recordingCheckoutSessionStore) MarkSucceededWithSubscription(context.Context, uuid.UUID, uuid.UUID, string, uuid.UUID) error {
	return nil
}

func (s *recordingCheckoutSessionStore) MarkFailed(_ context.Context, id uuid.UUID, message, code string) error {
	s.failedID, s.failedMessage, s.failedCode = id, message, code
	return nil
}

func (s *recordingCheckoutSessionStore) MarkExpired(_ context.Context, id uuid.UUID, reason string) error {
	s.expiredID, s.expiredReason = id, reason
	return nil
}

func TestHandleStripeWebhookDispatchesCheckoutSessionEvents(t *testing.T) {
	for _, eventType := range []string{"checkout.session.completed", "checkout.session.async_payment_succeeded"} {
		t.Run(eventType, func(t *testing.T) {
			svc := &StripeWebhookService{}
			err := svc.HandleStripeWebhook(context.Background(), stripeWebhookPayload(t, eventType, map[string]any{}))
			require.ErrorContains(t, err, "stripe checkout missing user_id metadata")
		})
	}

	sessionID := uuid.New()
	store := &recordingCheckoutSessionStore{}
	svc := &StripeWebhookService{CheckoutSessionService: store}
	obj := map[string]any{"metadata": map[string]string{"checkout_session_id": sessionID.String()}}

	require.NoError(t, svc.HandleStripeWebhook(context.Background(), stripeWebhookPayload(t, "checkout.session.async_payment_failed", obj)))
	require.Equal(t, sessionID, store.failedID)
	require.Equal(t, "stripe async payment failed", store.failedMessage)
	require.Equal(t, "async_payment_failed", store.failedCode)

	require.NoError(t, svc.HandleStripeWebhook(context.Background(), stripeWebhookPayload(t, "checkout.session.expired", obj)))
	require.Equal(t, sessionID, store.expiredID)
	require.Equal(t, "checkout expired", store.expiredReason)
}

func TestHandleStripeWebhookDispatchesDisputeEvents(t *testing.T) {
	for _, tc := range []struct {
		eventType string
		status    string
	}{
		{eventType: "charge.dispute.created", status: "needs_response"},
		{eventType: "charge.dispute.closed", status: "lost"},
	} {
		t.Run(tc.eventType, func(t *testing.T) {
			svc := &StripeWebhookService{}
			err := svc.HandleStripeWebhook(context.Background(), stripeWebhookPayload(t, tc.eventType, map[string]any{
				"id": "dp_123", "amount": 100, "status": tc.status,
			}))
			require.ErrorContains(t, err, "payment service is required for stripe dispute")
		})
	}
}

func stripeWebhookPayload(t *testing.T, eventType string, object any) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"id":   "evt_" + uuid.NewString(),
		"type": eventType,
		"data": map[string]any{"object": object},
	})
	require.NoError(t, err)
	return payload
}
