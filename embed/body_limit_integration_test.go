//go:build integration

package embed_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/integrationharness"
)

type bodyLimitObservation struct {
	Status                         int
	Type, Code                     string
	HasRequestID                   bool
	TooLarge, Invalid, Unreachable bool
	RecoveredAfterwards            bool
}

// TestOversizedRequestsAreRefusedIdenticallyAcrossDeployments proves the
// transport body cap answers with the standard error envelope and a stable
// code, and that the in-process Client applies the same cap as the HTTP
// mounts, so a consumer sees one 413 request_body_too_large everywhere.
func TestOversizedRequestsAreRefusedIdenticallyAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD")
	host := h.StartEmbeddedHost("USD")
	local, err := host.Runtime().Client()
	require.NoError(t, err)
	mid := dbtest.TestMerchantID.UUID()

	oversized := strings.Repeat("x", int(middleware.DefaultMaxBodyBytes)+1)
	observed := map[string]bodyLimitObservation{}
	for name, client := range map[string]*openrails.Client{"embedded": local, "hosted_http": host.Client(), "standalone": standalone.Client()} {
		customer := uuid.New()
		_, err := h.Pool().Exec(ctx, `INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
		require.NoError(t, err)
		payer := openrails.CustomerID(customer)
		deposit := openrails.DepositCreditsRequest{CustomerID: new(payer.String()), Invoker: "body-limit", Currency: "USD", Amount: 1, Source: "body-limit", SourceID: uuid.NewString()}
		deposit.Description = oversized
		_, err = client.DepositCredits(ctx, deposit)
		require.Error(t, err, name)
		o := bodyLimitObservation{
			TooLarge: errors.Is(err, openrails.ErrRequestBodyTooLarge), Invalid: errors.Is(err, openrails.ErrInvalid), Unreachable: errors.Is(err, openrails.ErrUnreachable),
		}
		var se *openrails.StatusError
		if errors.As(err, &se) {
			o.Status, o.Type, o.Code, o.HasRequestID = se.Status, se.Type, se.Code, se.RequestID != ""
		}
		deposit.Description = "fits"
		_, err = client.DepositCredits(ctx, deposit)
		o.RecoveredAfterwards = err == nil
		observed[name] = o
	}
	want := bodyLimitObservation{Status: 413, Type: "invalid_request_error", Code: "request_body_too_large", HasRequestID: true, TooLarge: true, Invalid: true, RecoveredAfterwards: true}
	for name, got := range observed {
		require.Equal(t, want, got, "%s oversized request", name)
	}
}
