//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/internal/service"
)

// enginesOf maps a world's remote client to its embedded one: both reach the
// one engine.
var enginesOf sync.Map

// The engine's checkout, as the embedding host's own backend drives it in
// process. No merchant route serves it: a browser buys through a checkout
// session.
func createCheckoutAttempt(ctx context.Context, client *openrails.Client, params billing.CreateCheckoutAttemptParams) (*billing.CheckoutAttempt, error) {
	if strings.TrimSpace(params.IdempotencyKey) == "" {
		return nil, errors.New("IdempotencyKey is required")
	}
	svc, ctx, err := checkoutService(ctx, client)
	if err != nil {
		return nil, err
	}
	out, err := svc.CreateCheckoutAttempt(ctx, params)
	return out, asStatusError(err)
}

func getCheckoutAttempt(ctx context.Context, client *openrails.Client, id billing.CheckoutAttemptID) (*billing.CheckoutAttempt, error) {
	svc, ctx, err := checkoutService(ctx, client)
	if err != nil {
		return nil, err
	}
	out, err := svc.GetCheckoutAttempt(ctx, id)
	return out, asStatusError(err)
}

func confirmCheckoutAttempt(ctx context.Context, client *openrails.Client, id billing.CheckoutAttemptID, params billing.ConfirmCheckoutAttemptParams) (*billing.CheckoutAttempt, error) {
	svc, ctx, err := checkoutService(ctx, client)
	if err != nil {
		return nil, err
	}
	out, err := svc.ConfirmCheckoutAttempt(ctx, id, params)
	return out, asStatusError(err)
}

// checkoutService is client's engine's billing service, with ctx pinned to
// its merchant as the embedding host's own in-process request.
func checkoutService(ctx context.Context, client *openrails.Client) (*service.Service, context.Context, error) {
	if local, ok := enginesOf.Load(client); ok {
		client = local.(*openrails.Client)
	}
	graph := engine.Graph(client)
	if graph == nil || graph.Runtime == nil {
		return nil, ctx, errors.New("checkout attempts need an embedded engine")
	}
	mid := graph.Runtime.ConfiguredMerchant()
	ctx = requestauth.WithHostPrincipal(merchant.WithID(ctx, mid), &requestauth.HostPrincipal{MerchantID: mid})
	svc, err := service.New(graph.Runtime)
	return svc, ctx, err
}

// statusError is an engine error as the API answers it, the engine's error
// still beneath it.
type statusError struct {
	*billing.StatusError
	cause error
}

func (e *statusError) Unwrap() []error { return []error{e.StatusError, e.cause} }

// asStatusError answers err through the checkout attempt route's own error
// writer and reads it back as the remote Client does.
func asStatusError(err error) error {
	var already *statusError
	if err == nil || errors.As(err, &already) {
		return err
	}
	rec := httptest.NewRecorder()
	handlers.WriteCheckoutAttemptError(rec, httptest.NewRequest(http.MethodPost, "/", nil), err)
	var envelope struct {
		Error *billing.ErrorDetails `json:"error"`
	}
	out := &billing.StatusError{Status: rec.Code, RetryAfter: rec.Header().Get("Retry-After")}
	if json.Unmarshal(rec.Body.Bytes(), &envelope) == nil && envelope.Error != nil {
		out.ErrorDetails = *envelope.Error
	}
	return &statusError{StatusError: out, cause: err}
}
