package openrails

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Deposit keys are opaque query values, not resource path segments. A key
// accepted when funding the payer must remain usable for receipt lookup (#484).
func TestGetDepositPreservesOpaqueSourceKeys(t *testing.T) {
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query().Get("source_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"amount":"1"}`))
	}))
	t.Cleanup(server.Close)
	client, err := NewRemote(server.URL, WithAPIKey("test-key"))
	require.NoError(t, err)
	for _, key := range []string{".", "..", "source/receipt?part=1&currency=JPY"} {
		t.Run(key, func(t *testing.T) {
			receipt, err := client.GetDeposit(context.Background(), CustomerID(uuid.New()), key)
			require.NoError(t, err)
			require.EqualValues(t, 1, receipt.Amount)
			require.Equal(t, key, <-requests)
		})
	}
}

// noRequestTransport fails the test if the Client sends anything.
type noRequestTransport struct{ t *testing.T }

func (n noRequestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("client sent %s %s for an invalid id", r.Method, r.URL.Path)
	return nil, errors.New("no request expected")
}

// Blank, whitespace and dot identifiers, and the zero UUID, are refused before
// any I/O with the same invalid_param StatusError the server returns for an
// invalid identifier, so embedded and remote callers observe one error.
func TestClientRefusesEmptyIdentifiersBeforeIO(t *testing.T) {
	ctx := context.Background()
	c, err := NewRemote("http://openrails.invalid",
		WithHTTPClient(&http.Client{Transport: noRequestTransport{t}}),
		WithTokenProvider(func(context.Context) (string, error) { return "", errors.New("token provider must not run") }),
	)
	require.NoError(t, err)
	now := time.Now()

	customer, price, subscription, method, session := CustomerID(uuid.New()), PriceID(uuid.New()), SubscriptionID(uuid.New()), PaymentMethodID(uuid.New()), CheckoutSessionID(uuid.New())

	// Free-form host strings: keys, entitlement names, request ids and plan
	// migration references. Blank and dot segments are refused.
	calls := map[string]func(id string) error{
		"GetProductByKey":  func(id string) error { _, err := c.GetProductByKey(ctx, id); return err },
		"GetPriceByKey":    func(id string) error { _, err := c.GetPriceByKey(ctx, id); return err },
		"SetPriceKey key":  func(id string) error { _, err := c.SetPriceKey(ctx, price, id); return err },
		"GetUsageMeter":    func(id string) error { _, err := c.GetUsageMeter(ctx, id); return err },
		"EnsureUsageMeter": func(id string) error { return c.EnsureUsageMeter(ctx, UsageMeterSpec{Key: id}) },
		"SetDefaultUsageRateCard": func(id string) error {
			_, err := c.SetDefaultUsageRateCard(ctx, id, DefaultUsageRateCardRequest{})
			return err
		},
		"DeleteDefaultUsageRateCard": func(id string) error { return c.DeleteDefaultUsageRateCard(ctx, id) },
		"GrantEntitlement entitlement": func(id string) error {
			_, err := c.GrantEntitlement(ctx, customer, GrantEntitlementRequest{Entitlement: id})
			return err
		},
		"RevokeEntitlement entitlement": func(id string) error { return c.RevokeEntitlement(ctx, customer, id) },
		"PreviewPlanMigration source": func(id string) error {
			_, err := c.PreviewPlanMigration(ctx, PlanMigrationRequest{SourcePrice: id, TargetPrice: "b"})
			return err
		},
		"PreviewPlanMigration target": func(id string) error {
			_, err := c.PreviewPlanMigration(ctx, PlanMigrationRequest{SourcePrice: "a", TargetPrice: id})
			return err
		},
		"CreatePlanMigration source": func(id string) error {
			_, err := c.CreatePlanMigration(ctx, PlanMigrationRequest{SourcePrice: id, TargetPrice: "b"})
			return err
		},
		"GetDeposit source":               func(id string) error { _, err := c.GetDeposit(ctx, customer, id); return err },
		"Capture":                         func(id string) error { _, err := c.Capture(ctx, id, 1, nil); return err },
		"Release":                         func(id string) error { return c.Release(ctx, id) },
		"ExtendHold":                      func(id string) error { return c.ExtendHold(ctx, id, now) },
		"GetOperationAuthorization":       func(id string) error { _, err := c.GetOperationAuthorization(ctx, id); return err },
		"GetProviderBillingQualification": func(id string) error { _, err := c.GetProviderBillingQualification(ctx, id); return err },
	}
	// Plan-migration prices and admission request ids are free-form host
	// strings; only blankness is refused for them.
	blankOnly := map[string]bool{
		"PreviewPlanMigration source": true, "PreviewPlanMigration target": true, "CreatePlanMigration source": true,
		"Capture": true, "Release": true, "ExtendHold": true, "GetDeposit source": true,
	}
	// Typed identifiers: the zero id names nothing and is refused before I/O.
	typedCalls := map[string]func() error{
		"GetSubscription":    func() error { _, err := c.GetSubscription(ctx, SubscriptionID{}); return err },
		"CancelSubscription": func() error { return c.CancelSubscription(ctx, SubscriptionID{}, CancelSubscriptionRequest{}) },
		"ResumeSubscription": func() error { return c.ResumeSubscription(ctx, SubscriptionID{}) },
		"UpdateSubscriptionPaymentMethod subscription": func() error {
			return c.UpdateSubscriptionPaymentMethod(ctx, SubscriptionID{}, UpdateSubscriptionPaymentMethodRequest{PaymentMethodID: method})
		},
		"UpdateSubscriptionPaymentMethod method": func() error {
			return c.UpdateSubscriptionPaymentMethod(ctx, subscription, UpdateSubscriptionPaymentMethodRequest{})
		},
		"PreviewTierChange subscription": func() error {
			_, err := c.PreviewTierChange(ctx, SubscriptionID{}, ChangeTierRequest{PriceID: price})
			return err
		},
		"PreviewTierChange price": func() error { _, err := c.PreviewTierChange(ctx, subscription, ChangeTierRequest{}); return err },
		"ChangeTier subscription": func() error {
			_, err := c.ChangeTier(ctx, SubscriptionID{}, "key", ChangeTierRequest{PriceID: price})
			return err
		},
		"ChangeTier price":             func() error { _, err := c.ChangeTier(ctx, subscription, "key", ChangeTierRequest{}); return err },
		"ListPaymentMethods":           func() error { _, err := c.ListPaymentMethods(ctx, CustomerID{}, PageOptions{}); return err },
		"DeletePaymentMethod customer": func() error { _, err := c.DeletePaymentMethod(ctx, CustomerID{}, method); return err },
		"DeletePaymentMethod method":   func() error { _, err := c.DeletePaymentMethod(ctx, customer, PaymentMethodID{}); return err },
		"GetCheckoutSession customer":  func() error { _, err := c.GetCheckoutSession(ctx, CustomerID{}, session); return err },
		"GetCheckoutSession session":   func() error { _, err := c.GetCheckoutSession(ctx, customer, CheckoutSessionID{}); return err },
		"ConfirmCheckoutSession": func() error {
			_, err := c.ConfirmCheckoutSession(ctx, CheckoutSessionID{}, ConfirmCheckoutSessionRequest{})
			return err
		},
		"ListCheckoutRailOptions":   func() error { _, err := c.ListCheckoutRailOptions(ctx, PriceID{}); return err },
		"ResolveEffectiveTier":      func() error { _, err := c.ResolveEffectiveTier(ctx, CustomerID{}, "group"); return err },
		"GetCustomerInvoiceProfile": func() error { _, err := c.GetCustomerInvoiceProfile(ctx, CustomerID{}); return err },
		"SetCustomerInvoiceProfile": func() error { return c.SetCustomerInvoiceProfile(ctx, CustomerID{}, InvoiceProfileDTO{}) },
		"EnsureCustomerInvoiceProfile": func() error {
			_, err := c.EnsureCustomerInvoiceProfile(ctx, CustomerID{}, InvoiceProfileDTO{})
			return err
		},
		"SetPriceKey id": func() error { _, err := c.SetPriceKey(ctx, PriceID{}, "key"); return err },
		"GrantEntitlement customer": func() error {
			_, err := c.GrantEntitlement(ctx, CustomerID{}, GrantEntitlementRequest{Entitlement: "pro"})
			return err
		},
		"RevokeEntitlement customer":  func() error { return c.RevokeEntitlement(ctx, CustomerID{}, "ent") },
		"GetDeposit customer":         func() error { _, err := c.GetDeposit(ctx, CustomerID{}, "src"); return err },
		"Balance":                     func() error { _, err := c.Balance(ctx, CustomerID{}); return err },
		"GetCreditAccount":            func() error { _, err := c.GetCreditAccount(ctx, CustomerID{}, "USD"); return err },
		"UsageRollup":                 func() error { _, err := c.UsageRollup(ctx, CustomerID{}, "USD", now, now, "day"); return err },
		"GetTrustLevel":               func() error { _, err := c.GetTrustLevel(ctx, CustomerID{}, "USD"); return err },
		"SetCreditLimit":              func() error { return c.SetCreditLimit(ctx, CustomerID{}, "USD", 1) },
		"GetCreditLimit":              func() error { _, err := c.GetCreditLimit(ctx, CustomerID{}, "USD"); return err },
		"ListEntitlements":            func() error { _, err := c.ListEntitlements(ctx, CustomerID{}, now); return err },
		"ListProductAccess":           func() error { _, err := c.ListProductAccess(ctx, CustomerID{}); return err },
		"HasProductAccess subject":    func() error { _, err := c.HasProductAccess(ctx, CustomerID{}, ProductID(uuid.New())); return err },
		"HasProductAccess product":    func() error { _, err := c.HasProductAccess(ctx, customer, ProductID{}); return err },
		"GetCustomerBillingPolicy":    func() error { _, err := c.GetCustomerBillingPolicy(ctx, CustomerID{}); return err },
		"SetCustomerBillingPolicy":    func() error { _, err := c.SetCustomerBillingPolicy(ctx, CustomerID{}, nil); return err },
		"SetCustomerSpendDelegations": func() error { return c.SetCustomerSpendDelegations(ctx, CustomerID{}, nil) },
		"EnsureCustomer":              func() error { _, err := c.EnsureCustomer(ctx, CustomerID{}); return err },
		"GetProduct":                  func() error { _, err := c.GetProduct(ctx, ProductID{}); return err },
		"UpdateProduct":               func() error { _, err := c.UpdateProduct(ctx, ProductID{}, UpdateProductRequest{}); return err },
		"GetPrice":                    func() error { _, err := c.GetPrice(ctx, PriceID{}); return err },
		"UpdatePrice":                 func() error { _, err := c.UpdatePrice(ctx, PriceID{}, UpdatePriceRequest{}); return err },
		"GetPayment":                  func() error { _, err := c.GetPayment(ctx, PaymentID{}); return err },
		"HasSettledPayment customer":  func() error { _, err := c.HasSettledPayment(ctx, CustomerID{}, price); return err },
		"HasSettledPayment price":     func() error { _, err := c.HasSettledPayment(ctx, customer, PriceID{}); return err },
	}
	uuidCalls := map[string]func(id uuid.UUID) error{
		"GetMerchantInvoice":       func(id uuid.UUID) error { _, err := c.GetMerchantInvoice(ctx, id); return err },
		"VoidInvoice":              func(id uuid.UUID) error { _, err := c.VoidInvoice(ctx, id); return err },
		"MarkInvoiceUncollectible": func(id uuid.UUID) error { _, err := c.MarkInvoiceUncollectible(ctx, id); return err },
		"RecordInvoicePayment": func(id uuid.UUID) error {
			_, err := c.RecordInvoicePayment(ctx, id, RecordInvoicePaymentRequest{})
			return err
		},
		"RetryInvoiceCollection": func(id uuid.UUID) error {
			_, err := c.RetryInvoiceCollection(ctx, InvoiceCollectionRetryRequest{InvoiceID: id})
			return err
		},
		"ListInvoicePaymentAttempts": func(id uuid.UUID) error { _, _, err := c.ListInvoicePaymentAttempts(ctx, id, 10, 0); return err },
		"CancelPlanMigration":        func(id uuid.UUID) error { _, err := c.CancelPlanMigration(ctx, id); return err },
		"AcknowledgeHostEvent":       func(id uuid.UUID) error { return c.AcknowledgeHostEvent(ctx, id) },
	}

	requireInvalidParam := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		require.ErrorIs(t, err, ErrInvalid)
		var status *StatusError
		require.ErrorAs(t, err, &status)
		require.Equal(t, http.StatusBadRequest, status.Status)
		require.Equal(t, "invalid_request_error", status.Type)
		require.Equal(t, "invalid_param", status.Code)
		require.NotEmpty(t, status.Message)
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			ids := []string{"", "   ", "\t\n"}
			if !blankOnly[name] {
				ids = append(ids, ".", "..", " . ")
			}
			for _, id := range ids {
				requireInvalidParam(t, call(id))
			}
		})
	}
	for name, call := range typedCalls {
		t.Run(name, func(t *testing.T) { requireInvalidParam(t, call()) })
	}
	for name, call := range uuidCalls {
		t.Run(name, func(t *testing.T) { requireInvalidParam(t, call(uuid.Nil)) })
	}
}
