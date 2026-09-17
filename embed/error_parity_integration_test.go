//go:build integration

package embed_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/pkg/embedded"
)

// errorObservation is everything a caller can branch on, excluding the human
// message and per-request identifiers.
type errorObservation struct {
	StatusError  bool
	Status       int
	Type         string
	Code         string
	Param        string
	Metadata     map[string]any
	HasRequestID bool

	Invalid, Unauthorized, Denied, NotFound, Conflict, Internal, Unreachable bool
	InsufficientCredits, IdempotencyKeyReused, Canceled, DeadlineExceeded    bool
}

func observeClientError(t *testing.T, label string, err error) errorObservation {
	t.Helper()
	require.Error(t, err, label)
	o := errorObservation{
		Invalid:              errors.Is(err, openrails.ErrInvalid),
		Unauthorized:         errors.Is(err, openrails.ErrUnauthorized),
		Denied:               errors.Is(err, openrails.ErrDenied),
		NotFound:             errors.Is(err, openrails.ErrNotFound),
		Conflict:             errors.Is(err, openrails.ErrConflict),
		Internal:             errors.Is(err, openrails.ErrInternal),
		Unreachable:          errors.Is(err, openrails.ErrUnreachable),
		InsufficientCredits:  errors.Is(err, openrails.ErrInsufficientCredits),
		IdempotencyKeyReused: errors.Is(err, openrails.ErrIdempotencyKeyReused),
		Canceled:             errors.Is(err, context.Canceled),
		DeadlineExceeded:     errors.Is(err, context.DeadlineExceeded),
	}
	var status *openrails.StatusError
	if errors.As(err, &status) {
		o.StatusError, o.Status, o.Type, o.Code = true, status.Status, status.Type, status.Code
		o.Metadata, o.HasRequestID = status.Metadata, status.RequestID != ""
		if status.Param != nil {
			o.Param = *status.Param
		}
	}
	return o
}

// TestClientErrorsAreIdenticalAcrossDeployments drives the same failures
// through the in-process embedded Client, an embedded host's HTTP mount, a
// standalone server and a multi-merchant (SaaS-style) embedded runtime.
func TestClientErrorsAreIdenticalAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD")
	host := h.StartEmbeddedHost("USD")
	multi, err := embed.New(ctx, embed.Options{Options: embedded.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embedded.RiverManagedByOpenRails(),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, multi.Close(context.Background())) })
	multiClient, err := multi.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	local, err := host.Runtime().Client()
	require.NoError(t, err)

	clients := map[string]*openrails.Client{
		"embedded": local, "hosted_http": host.Client(), "standalone": standalone.Client(), "multi_merchant": multiClient,
	}
	observed := map[string]map[string]errorObservation{}
	for name, client := range clients {
		observed[name] = runErrorScript(t, ctx, h, client)
	}

	want := observed["standalone"]
	require.Equal(t, errorObservation{
		StatusError: true, Status: 400, Type: "invalid_request_error", Code: "payment_method_delete_unsupported",
		Metadata: map[string]any{"rail": "stripe"}, HasRequestID: true, Invalid: true,
	}, want["delete_unsupported_rail"], "metadata-bearing refusal")
	require.Equal(t, "customer_id", want["admit_invalid_customer"].Param)
	require.True(t, want["usage_key_reused"].IdempotencyKeyReused)
	require.Equal(t, errorObservation{Unreachable: true, Canceled: true}, want["canceled"])
	require.Equal(t, errorObservation{Unreachable: true, DeadlineExceeded: true}, want["deadline"])
	for name, got := range observed {
		require.Equal(t, want, got, "%s error contract diverged from standalone", name)
	}
}

func runErrorScript(t *testing.T, ctx context.Context, h *integrationharness.Harness, client *openrails.Client) map[string]errorObservation {
	t.Helper()
	mid := dbtest.TestMerchantID.UUID()
	customer, product, price, nmi, stripe, card, portalCard, sub := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	exec := func(sql string, args ...any) {
		_, err := h.Pool().Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	// Other tests in this package assert the shared merchant's checkout config;
	// leave no PSP behind.
	t.Cleanup(func() {
		for _, sql := range []string{
			`DELETE FROM openrails.subscriptions WHERE merchant_id=$1 AND psp_id IN ($2,$3)`,
			`DELETE FROM openrails.payment_methods WHERE merchant_id=$1 AND psp_id IN ($2,$3)`,
			`DELETE FROM openrails.psps WHERE merchant_id=$1 AND id IN ($2,$3)`,
		} {
			_, err := h.Pool().Exec(context.Background(), sql, mid, nmi, stripe)
			require.NoError(t, err)
		}
	})
	exec(`INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
	exec(`INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Parity plan')`, product, mid, product.String())
	exec(`INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,$4,1000000,'USD',true,720)`, price, mid, product, price.String())
	exec(`INSERT INTO openrails.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'nmi','test',$3,$3),($4,$2,'stripe','test',$5,$5)`, nmi, mid, nmi.String(), stripe, stripe.String())
	exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,psp_id,rail,initial_transaction_id,last_four,card_type) VALUES($1,$2,$3,$4,'nmi','parity-anchor','4242','visa'),($5,$2,$3,$6,'stripe','parity-portal','4444','mastercard')`, card, mid, customer, nmi, portalCard, stripe)
	exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,payment_method_id,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,'nmi','active',$7,$8,$9,$10)`, sub, mid, customer, product, price, nmi, sub.String(), card, now, now.Add(48*time.Hour))

	out := map[string]errorObservation{}
	_, err := client.DeletePaymentMethod(ctx, customer.String(), card.String())
	out["delete_in_use"] = observeClientError(t, "delete in use", err)
	_, err = client.DeletePaymentMethod(ctx, uuid.NewString(), card.String())
	out["delete_foreign_customer"] = observeClientError(t, "delete foreign customer", err)
	_, err = client.DeletePaymentMethod(ctx, customer.String(), portalCard.String())
	out["delete_unsupported_rail"] = observeClientError(t, "delete unsupported rail", err)
	_, err = client.GetSubscription(ctx, uuid.NewString())
	out["subscription_not_found"] = observeClientError(t, "subscription not found", err)
	_, err = client.ListPaymentMethods(ctx, customer.String(), openrails.PageOptions{Limit: 101})
	out["page_limit"] = observeClientError(t, "page limit", err)
	_, err = client.Admit(ctx, openrails.AdmitRequest{CustomerID: "invalid", Currency: "USD"})
	out["admit_invalid_customer"] = observeClientError(t, "admit invalid customer", err)

	payer := openrails.CustomerID(customer)
	_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "parity", Currency: "USD", Amount: 1_000_000, Source: "parity", SourceID: uuid.NewString()})
	require.NoError(t, err)
	usage := openrails.UsageReport{CustomerID: customer.String(), Invoker: "parity", Currency: "USD", EventType: "parity", Amount: 1, Source: "parity", SourceID: uuid.NewString()}
	require.NoError(t, client.RecordUsage(ctx, usage))
	usage.Amount = 2
	out["usage_key_reused"] = observeClientError(t, "usage key reused", client.RecordUsage(ctx, usage))

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = client.GetMerchantSettings(canceled)
	out["canceled"] = observeClientError(t, "canceled", err)
	expired, cancelExpired := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancelExpired()
	_, err = client.GetMerchantSettings(expired)
	out["deadline"] = observeClientError(t, "deadline", err)
	return out
}
