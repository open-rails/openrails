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
// accepted when funding the payer must remain usable for receipt lookup.
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
			receipt, err := client.GetDeposit(context.Background(), uuid.NewString(), key)
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
	valid := uuid.NewString()
	now := time.Now()

	calls := map[string]func(id string) error{
		"GetSubscription":    func(id string) error { _, err := c.GetSubscription(ctx, id); return err },
		"CancelSubscription": func(id string) error { return c.CancelSubscription(ctx, id, CancelSubscriptionRequest{}) },
		"ResumeSubscription": func(id string) error { return c.ResumeSubscription(ctx, id) },
		"UpdateSubscriptionPaymentMethod": func(id string) error {
			return c.UpdateSubscriptionPaymentMethod(ctx, id, UpdateSubscriptionPaymentMethodRequest{})
		},
		"PreviewTierChange":            func(id string) error { _, err := c.PreviewTierChange(ctx, id, ChangeTierRequest{}); return err },
		"ChangeTier":                   func(id string) error { _, err := c.ChangeTier(ctx, id, "key", ChangeTierRequest{}); return err },
		"ListPaymentMethods":           func(id string) error { _, err := c.ListPaymentMethods(ctx, id, PageOptions{}); return err },
		"DeletePaymentMethod customer": func(id string) error { _, err := c.DeletePaymentMethod(ctx, id, valid); return err },
		"DeletePaymentMethod method":   func(id string) error { _, err := c.DeletePaymentMethod(ctx, valid, id); return err },
		"GetCheckoutSession customer":  func(id string) error { _, err := c.GetCheckoutSession(ctx, id, valid); return err },
		"GetCheckoutSession session":   func(id string) error { _, err := c.GetCheckoutSession(ctx, valid, id); return err },
		"ConfirmCheckoutSession": func(id string) error {
			_, err := c.ConfirmCheckoutSession(ctx, id, ConfirmCheckoutSessionRequest{})
			return err
		},
		"ListCheckoutRailOptions":   func(id string) error { _, err := c.ListCheckoutRailOptions(ctx, id); return err },
		"ResolveEffectiveTier":      func(id string) error { _, err := c.ResolveEffectiveTier(ctx, id, "group"); return err },
		"GetCustomerInvoiceProfile": func(id string) error { _, err := c.GetCustomerInvoiceProfile(ctx, id); return err },
		"SetCustomerInvoiceProfile": func(id string) error { return c.SetCustomerInvoiceProfile(ctx, id, InvoiceProfileDTO{}) },
		"EnsureCustomerInvoiceProfile": func(id string) error {
			_, err := c.EnsureCustomerInvoiceProfile(ctx, id, InvoiceProfileDTO{})
			return err
		},
		"GetProductByKey":  func(id string) error { _, err := c.GetProductByKey(ctx, id); return err },
		"GetPriceByKey":    func(id string) error { _, err := c.GetPriceByKey(ctx, id); return err },
		"SetPriceKey key":  func(id string) error { _, err := c.SetPriceKey(ctx, uuid.New(), id); return err },
		"GetUsageMeter":    func(id string) error { _, err := c.GetUsageMeter(ctx, id); return err },
		"EnsureUsageMeter": func(id string) error { return c.EnsureUsageMeter(ctx, UsageMeterSpec{Key: id}) },
		"SetDefaultUsageRateCard": func(id string) error {
			_, err := c.SetDefaultUsageRateCard(ctx, id, DefaultUsageRateCardRequest{})
			return err
		},
		"DeleteDefaultUsageRateCard": func(id string) error { return c.DeleteDefaultUsageRateCard(ctx, id) },
		"GrantEntitlement customer": func(id string) error {
			_, err := c.GrantEntitlement(ctx, id, GrantEntitlementRequest{Entitlement: "pro"})
			return err
		},
		"GrantEntitlement entitlement": func(id string) error {
			_, err := c.GrantEntitlement(ctx, valid, GrantEntitlementRequest{Entitlement: id})
			return err
		},
		"RevokeEntitlement customer":    func(id string) error { return c.RevokeEntitlement(ctx, id, valid) },
		"RevokeEntitlement entitlement": func(id string) error { return c.RevokeEntitlement(ctx, valid, id) },
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
		"GetDeposit customer":             func(id string) error { _, err := c.GetDeposit(ctx, id, "src"); return err },
		"GetDeposit source":               func(id string) error { _, err := c.GetDeposit(ctx, valid, id); return err },
		"Balance":                         func(id string) error { _, err := c.Balance(ctx, id); return err },
		"GetCreditAccount":                func(id string) error { _, err := c.GetCreditAccount(ctx, id, "USD"); return err },
		"UsageRollup":                     func(id string) error { _, err := c.UsageRollup(ctx, id, "USD", now, now, "day"); return err },
		"GetTrustLevel":                   func(id string) error { _, err := c.GetTrustLevel(ctx, id, "USD"); return err },
		"SetCreditLimit":                  func(id string) error { return c.SetCreditLimit(ctx, id, "USD", 1) },
		"GetCreditLimit":                  func(id string) error { _, err := c.GetCreditLimit(ctx, id, "USD"); return err },
		"Capture":                         func(id string) error { _, err := c.Capture(ctx, id, 1, nil); return err },
		"Release":                         func(id string) error { return c.Release(ctx, id) },
		"ExtendHold":                      func(id string) error { return c.ExtendHold(ctx, id, now) },
		"GetOperationAuthorization":       func(id string) error { _, err := c.GetOperationAuthorization(ctx, id); return err },
		"GetProviderBillingQualification": func(id string) error { _, err := c.GetProviderBillingQualification(ctx, id); return err },
		"ListEntitlements":                func(id string) error { _, err := c.ListEntitlements(ctx, id, now); return err },
		"ListProductAccess":               func(id string) error { _, err := c.ListProductAccess(ctx, id); return err },
		"HasProductAccess subject":        func(id string) error { _, err := c.HasProductAccess(ctx, id, valid); return err },
		"SetCustomerSpendDelegations":     func(id string) error { return c.SetCustomerSpendDelegations(ctx, id, nil) },
	}
	// Plan-migration prices and admission request ids are free-form host
	// strings; only blankness is refused for them.
	blankOnly := map[string]bool{
		"PreviewPlanMigration source": true, "PreviewPlanMigration target": true, "CreatePlanMigration source": true,
		"Capture": true, "Release": true, "ExtendHold": true,
		"GetDeposit source": true, "GetTrustLevel": true, "SetCreditLimit": true, "GetCreditLimit": true,
		"Balance": true, "GetCreditAccount": true, "UsageRollup": true, "GetDeposit customer": true,
		"ListEntitlements": true, "ListProductAccess": true, "HasProductAccess subject": true, "SetCustomerSpendDelegations": true,
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
		"GetProduct":                 func(id uuid.UUID) error { _, err := c.GetProduct(ctx, id); return err },
		"UpdateProduct":              func(id uuid.UUID) error { _, err := c.UpdateProduct(ctx, id, UpdateProductRequest{}); return err },
		"GetPrice":                   func(id uuid.UUID) error { _, err := c.GetPrice(ctx, id); return err },
		"UpdatePrice":                func(id uuid.UUID) error { _, err := c.UpdatePrice(ctx, id, UpdatePriceRequest{}); return err },
		"SetPriceKey id":             func(id uuid.UUID) error { _, err := c.SetPriceKey(ctx, id, "key"); return err },
		"CancelPlanMigration":        func(id uuid.UUID) error { _, err := c.CancelPlanMigration(ctx, id); return err },
		"AcknowledgeHostEvent":       func(id uuid.UUID) error { return c.AcknowledgeHostEvent(ctx, id) },
		"HasSettledPayment customer": func(id uuid.UUID) error { _, err := c.HasSettledPayment(ctx, CustomerID(id), uuid.New()); return err },
		"HasSettledPayment price":    func(id uuid.UUID) error { _, err := c.HasSettledPayment(ctx, CustomerID(uuid.New()), id); return err },
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
	for name, call := range uuidCalls {
		t.Run(name, func(t *testing.T) { requireInvalidParam(t, call(uuid.Nil)) })
	}
}
