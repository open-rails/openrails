//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/shared/apperr"
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

func asStatusError(err error) error {
	if err == nil {
		return nil
	}
	status, code := checkoutErrorStatus(err)
	details := billing.ErrorDetails{Code: code, Message: err.Error()}
	var refusal *apperr.Error
	switch {
	case errors.Is(err, checkout.ErrPaymentMethodRequired):
		details.Param = new("payment_method_id")
	case errors.As(err, &refusal) && refusal.Param != "":
		details.Param = new(refusal.Param)
	}
	return &statusError{StatusError: &billing.StatusError{Status: status, ErrorDetails: details}, cause: err}
}

// checkoutErrorStatus is the status and code the API answers a checkout
// attempt error with.
func checkoutErrorStatus(err error) (int, string) {
	var insufficient *recurring.InsufficientUSDCError
	var refusal *apperr.Error
	var blocked *checkout.CardAttemptsBlockedError
	var declined *paymentmethods.PaymentMethodError
	var already *statusError
	switch {
	case errors.As(err, &already):
		return already.Status, already.Code
	case errors.As(err, &blocked):
		return http.StatusTooManyRequests, "card_attempts_blocked"
	case errors.As(err, &declined):
		return http.StatusPaymentRequired, "card_declined"
	case errors.Is(err, paymentmethods.ErrCardNotSaved):
		return http.StatusConflict, "card_not_saved"
	case errors.Is(err, paymentmethods.ErrCardEntryNotEnabled), errors.Is(err, paymentmethods.ErrCardWithToken):
		return http.StatusBadRequest, billing.CodeInvalidParam
	case errors.Is(err, checkout.ErrPaymentMethodStale):
		return http.StatusPaymentRequired, billing.CodePaymentMethodStale
	case errors.Is(err, checkout.ErrPaymentMethodRequired):
		return http.StatusBadRequest, billing.CodePaymentMethodRequired
	case errors.Is(err, billing.ErrIdempotencyKeyReused):
		return http.StatusConflict, "idempotency_key_reused"
	case errors.As(err, &insufficient):
		return http.StatusPaymentRequired, "insufficient_funds"
	case errors.Is(err, vault.ErrUnavailable):
		return http.StatusServiceUnavailable, "service_unavailable"
	case errors.Is(err, checkout.ErrCheckoutAttemptNotFound):
		return http.StatusNotFound, billing.CodeResourceNotFound
	case errors.Is(err, checkout.ErrCheckoutAttemptForbidden):
		return http.StatusForbidden, billing.CodeResourceAccessDenied
	case errors.Is(err, checkout.ErrCheckoutAttemptExpired):
		return http.StatusGone, "checkout_attempt_expired"
	case errors.Is(err, checkout.ErrCheckoutAttemptPending), errors.Is(err, checkout.ErrCheckoutProcessing), errors.Is(err, checkout.ErrCheckoutAttemptConflict):
		return http.StatusConflict, billing.CodeResourceConflict
	case errors.Is(err, checkout.ErrCheckoutAttemptValidation):
		return http.StatusBadRequest, billing.CodeInvalidParam
	case errors.As(err, &refusal):
		return refusal.Status, refusal.Code
	}
	return http.StatusInternalServerError, billing.CodeInternalError
}
