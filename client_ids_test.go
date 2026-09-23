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
	client, err := NewRemote(server.URL, WithAPIKey("test-key"), WithDefaultMerchant("fixture"))
	require.NoError(t, err)
	for _, key := range []string{".", "..", "source/receipt?part=1&currency=JPY"} {
		t.Run(key, func(t *testing.T) {
			receipt, err := client.GetDeposit(context.Background(), (CustomerID(uuid.New())).String(), key)
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
		WithDefaultMerchant("fixture"),
		WithHTTPClient(&http.Client{Transport: noRequestTransport{t}}),
		WithTokenProvider(func(context.Context) (string, error) { return "", errors.New("token provider must not run") }),
	)
	require.NoError(t, err)
	now := time.Now()

	customer, price, subscription, method, session := CustomerID(uuid.New()), PriceID(uuid.New()), SubscriptionID(uuid.New()), PaymentMethodID(uuid.New()), CheckoutSessionID(uuid.New())

	// Free-form host strings: keys, entitlement names, request ids and plan
	// migration references. Blank and dot segments are refused.
	calls := map[string]func(id string) error{
		"GetProductByKey":  func(id string) error { _, err := c.Products.RetrieveByKey(ctx, id); return err },
		"GetPriceByKey":    func(id string) error { _, err := c.Prices.RetrieveByKey(ctx, id); return err },
		"SetPriceKey key":  func(id string) error { _, err := c.Prices.SetKey(ctx, (price).String(), id); return err },
		"GetUsageMeter":    func(id string) error { _, err := c.GetUsageMeter(ctx, id); return err },
		"EnsureUsageMeter": func(id string) error { return c.EnsureUsageMeter(ctx, UsageMeterSpec{Key: id}) },
		"SetDefaultUsageRateCard": func(id string) error {
			_, err := c.SetDefaultUsageRateCard(ctx, id, DefaultUsageRateCardRequest{})
			return err
		},
		"DeleteDefaultUsageRateCard": func(id string) error { return c.DeleteDefaultUsageRateCard(ctx, id) },
		"GrantEntitlement entitlement": func(id string) error {
			_, err := c.GrantEntitlement(ctx, (customer).String(), GrantEntitlementRequest{Entitlement: id})
			return err
		},
		"RevokeEntitlement entitlement": func(id string) error { return c.RevokeEntitlement(ctx, (customer).String(), id) },
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
		"GetDeposit source":               func(id string) error { _, err := c.GetDeposit(ctx, (customer).String(), id); return err },
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
		"PayInvoiceNow":        func() error { _, err := c.PayInvoiceNow(ctx, PayInvoiceNowRequest{}); return err },
		"RetrySubscriptionNow": func() error { _, err := c.RetrySubscriptionNow(ctx, RetrySubscriptionNowRequest{}); return err },
		"GetMyInvoice":         func() error { _, err := c.GetMyInvoice(ctx, uuid.Nil); return err },
		"GetMySubscription":    func() error { _, err := c.GetMySubscription(ctx, SubscriptionID{}); return err },
		"GetSubscription":      func() error { _, err := c.GetSubscription(ctx, SubscriptionID{}); return err },
		"CancelSubscription":   func() error { return c.CancelSubscription(ctx, SubscriptionID{}, CancelSubscriptionRequest{}) },
		"ResumeSubscription":   func() error { return c.ResumeSubscription(ctx, SubscriptionID{}) },
		"UpdateSubscriptionPaymentMethod subscription": func() error {
			return c.UpdateSubscriptionPaymentMethod(ctx, SubscriptionID{}, UpdateSubscriptionPaymentMethodRequest{PaymentMethodID: method})
		},
		"UpdateSubscriptionPaymentMethod method": func() error {
			return c.UpdateSubscriptionPaymentMethod(ctx, subscription, UpdateSubscriptionPaymentMethodRequest{})
		},
		"PreviewTierChange subscription": func() error {
			_, err := c.PreviewTierChange(ctx, SubscriptionID{}, ChangeTierRequest{PriceID: (price).String()})
			return err
		},
		"PreviewTierChange price": func() error { _, err := c.PreviewTierChange(ctx, subscription, ChangeTierRequest{}); return err },
		"ChangeTier subscription": func() error {
			_, err := c.ChangeTier(ctx, SubscriptionID{}, "key", ChangeTierRequest{PriceID: (price).String()})
			return err
		},
		"ChangeTier price":             func() error { _, err := c.ChangeTier(ctx, subscription, "key", ChangeTierRequest{}); return err },
		"ListPaymentMethods":           func() error { _, err := c.ListPaymentMethods(ctx, (CustomerID{}).String(), PageOptions{}); return err },
		"DeletePaymentMethod customer": func() error { _, err := c.DeletePaymentMethod(ctx, (CustomerID{}).String(), method); return err },
		"DeletePaymentMethod method":   func() error { _, err := c.DeletePaymentMethod(ctx, (customer).String(), PaymentMethodID{}); return err },
		"GetCheckoutSession customer":  func() error { _, err := c.GetCheckoutSession(ctx, "", session.String()); return err },
		"GetCheckoutSession session":   func() error { _, err := c.GetCheckoutSession(ctx, customer.String(), ""); return err },
		"ConfirmCheckoutSession": func() error {
			_, err := c.ConfirmCheckoutSession(ctx, "", ConfirmCheckoutSessionRequest{})
			return err
		},
		"ListCheckoutRailOptions":   func() error { _, err := c.ListCheckoutRailOptions(ctx, (PriceID{}).String()); return err },
		"ResolveEffectiveTier":      func() error { _, err := c.ResolveEffectiveTier(ctx, (CustomerID{}).String(), "group"); return err },
		"GetCustomerInvoiceProfile": func() error { _, err := c.GetCustomerInvoiceProfile(ctx, (CustomerID{}).String()); return err },
		"SetCustomerInvoiceProfile": func() error { return c.SetCustomerInvoiceProfile(ctx, (CustomerID{}).String(), InvoiceProfileDTO{}) },
		"EnsureCustomerInvoiceProfile": func() error {
			_, err := c.EnsureCustomerInvoiceProfile(ctx, (CustomerID{}).String(), InvoiceProfileDTO{})
			return err
		},
		"SetPriceKey id": func() error { _, err := c.Prices.SetKey(ctx, "", "key"); return err },
		"GrantEntitlement customer": func() error {
			_, err := c.GrantEntitlement(ctx, (CustomerID{}).String(), GrantEntitlementRequest{Entitlement: "pro"})
			return err
		},
		"RevokeEntitlement customer": func() error { return c.RevokeEntitlement(ctx, (CustomerID{}).String(), "ent") },
		"GetDeposit customer":        func() error { _, err := c.GetDeposit(ctx, (CustomerID{}).String(), "src"); return err },
		"Balance":                    func() error { _, err := c.Balance(ctx, (CustomerID{}).String()); return err },
		"GetCreditAccount":           func() error { _, err := c.GetCreditAccount(ctx, (CustomerID{}).String(), "USD"); return err },
		"UsageRollup": func() error {
			_, err := c.UsageRollup(ctx, (CustomerID{}).String(), "USD", now, now, "day")
			return err
		},
		"GetTrustLevel":     func() error { _, err := c.GetTrustLevel(ctx, (CustomerID{}).String(), "USD"); return err },
		"SetCreditLimit":    func() error { return c.SetCreditLimit(ctx, (CustomerID{}).String(), "USD", 1) },
		"GetCreditLimit":    func() error { _, err := c.GetCreditLimit(ctx, (CustomerID{}).String(), "USD"); return err },
		"ListEntitlements":  func() error { _, err := c.ListEntitlements(ctx, (CustomerID{}).String(), now); return err },
		"ListProductAccess": func() error { _, err := c.ProductAccess.List(ctx, &ProductAccessListParams{}); return err },
		"HasProductAccess subject": func() error {
			_, err := c.ProductAccess.Check(ctx, &ProductAccessCheckParams{ProductID: ProductID(uuid.New()).String()})
			return err
		},
		"HasProductAccess product": func() error {
			_, err := c.ProductAccess.Check(ctx, &ProductAccessCheckParams{CustomerID: customer.String()})
			return err
		},
		"GetCustomerBillingPolicy":    func() error { _, err := c.GetCustomerBillingPolicy(ctx, (CustomerID{}).String()); return err },
		"SetCustomerBillingPolicy":    func() error { _, err := c.SetCustomerBillingPolicy(ctx, (CustomerID{}).String(), nil); return err },
		"SetCustomerSpendDelegations": func() error { return c.SetCustomerSpendDelegations(ctx, (CustomerID{}).String(), nil) },
		"EnsureCustomer":              func() error { _, err := c.EnsureCustomer(ctx, (CustomerID{}).String()); return err },
		"GetProduct":                  func() error { _, err := c.Products.Retrieve(ctx, ""); return err },
		"UpdateProduct":               func() error { _, err := c.Products.Update(ctx, "", &ProductUpdateParams{}); return err },
		"GetPrice":                    func() error { _, err := c.Prices.Retrieve(ctx, ""); return err },
		"UpdatePrice":                 func() error { _, err := c.Prices.Update(ctx, "", &PriceUpdateParams{}); return err },
		"GetPayment":                  func() error { _, err := c.GetPayment(ctx, PaymentID{}); return err },
		"HasSettledPayment customer": func() error {
			_, err := c.HasSettledPayment(ctx, (CustomerID{}).String(), (price).String())
			return err
		},
		"HasSettledPayment price": func() error {
			_, err := c.HasSettledPayment(ctx, (customer).String(), (PriceID{}).String())
			return err
		},
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
