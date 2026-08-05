package checkout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// captureCheckoutExecutor records the CheckoutRequest handed to Checkout and
// short-circuits with an error so initializeCheckoutSession returns before
// applyCheckoutResponse — we only assert what was threaded into the request.
type captureCheckoutExecutor struct {
	stubRailTargets
	captured *CheckoutRequest
}

func (c *captureCheckoutExecutor) Checkout(ctx context.Context, req *CheckoutRequest, user *UserIdentity) (*CheckoutResponse, error) {
	c.captured = req
	return nil, errors.New("stop after capture")
}

func (c *captureCheckoutExecutor) RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error) {
	return nil, nil
}

func (c *captureCheckoutExecutor) CheckSubscriptionConflict(ctx context.Context, userID string, price *models.Price, product *models.Product) (*SubscriptionConflict, error) {
	return &SubscriptionConflict{}, nil
}

// TestInitializeCheckoutSession_ThreadsStripeReturnURLs is the regression guard
// for #521: the refactor moved Stripe success_url/cancel_url off rail
// config and onto the request, but the public /v1/checkout SESSION path
// (CreateSession → initializeSession → initializeCheckoutSession) had no field
// to carry them, so processStripeSubscription/Payment always failed with
// "stripe success_url and cancel_url are required". This proves the URLs the
// caller supplies on the session-create request now reach the CheckoutRequest.
func TestInitializeCheckoutSession_ThreadsStripeReturnURLs(t *testing.T) {
	const (
		wantSuccess = "https://app.example/account?subscription=success"
		wantCancel  = "https://app.example/account?subscription=canceled"
	)

	exec := &captureCheckoutExecutor{}
	svc := &CheckoutSessionService{checkoutService: exec}
	startedAt := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)

	session := &models.CheckoutSession{
		ID:        uuid.New(),
		PriceID:   uuid.New(),
		Rail:      models.RailStripe,
		CreatedAt: startedAt,
	}
	payment := &CheckoutSessionPaymentRequest{Rail: string(models.RailStripe)}
	user := &UserIdentity{ID: uuid.New().String()}

	// Returns the stub's "stop after capture" error; we only care about capture.
	_ = svc.initializeCheckoutSession(context.Background(), session, payment, wantSuccess, wantCancel, user)

	if exec.captured == nil {
		t.Fatal("checkoutService.Checkout was never called")
	}
	if exec.captured.SuccessURL != wantSuccess {
		t.Errorf("SuccessURL not threaded: got %q, want %q", exec.captured.SuccessURL, wantSuccess)
	}
	if exec.captured.CancelURL != wantCancel {
		t.Errorf("CancelURL not threaded: got %q, want %q", exec.captured.CancelURL, wantCancel)
	}
	if !exec.captured.CheckoutStartedAt.Equal(startedAt) {
		t.Errorf("CheckoutStartedAt not threaded: got %v, want %v", exec.captured.CheckoutStartedAt, startedAt)
	}
}
