package checkout

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// The initial-membership quote is the server-side term sheet the payer
// accepts: it survives a generic JSON round trip exactly, cannot be re-quoted
// under changed catalog terms, is accepted only by the verified interactive
// payer before expiry, and re-anchors its period at acceptance.
func TestInitialMembershipQuote(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 123456000, time.UTC)
	mid, customer, psp, custodian := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
	product := models.Product{ID: uuid.New(), DisplayName: "Quoted membership", Entitlements: []string{"quota"}}
	price := models.Price{ID: uuid.New(), ProductID: product.ID, Amount: 9_990_000, Currency: "USD", BillingIntervalHours: intPtr(720), AccessDurationHours: intPtr(720)}
	expiry := now.Add(time.Hour)
	session := models.CheckoutAttempt{ID: uuid.New(), CustomerID: customer, PspID: psp, PriceID: &price.ID, Mode: models.CheckoutAttemptModeSubscription, Rail: models.RailNMI,
		Status: models.CheckoutAttemptStatusRequiresAction, Amount: new(price.Amount), Currency: new(price.Currency), ExpiresAt: &expiry}
	method := gen.BillingPaymentMethod{ID: uuid.New(), MerchantID: mid, CustomerID: customer, Rail: "nmi", Custodian: models.CustodianHyperSwitch, CustodianID: &custodian, RailCustomerRef: new("customer"), RailMethodRef: new("method")}

	require.NoError(t, quoteInitialMembership(ctx, &session, &price, &product, method, now))
	encoded, err := json.Marshal(session)
	require.NoError(t, err)
	var restored models.CheckoutAttempt
	require.NoError(t, json.Unmarshal(encoded, &restored))
	quoted, err := readInitialMembershipQuote(&restored)
	require.NoError(t, err)
	require.Equal(t, []string{"quota"}, quoted.Entitlements)
	require.Equal(t, 720*time.Hour, quoted.PeriodEnd.Sub(quoted.PeriodStart))

	view := (&CheckoutAttemptService{}).sessionToResponse(&restored)
	require.Equal(t, "Quoted membership", view.MembershipQuote.ProductName)
	view.MembershipQuote.Entitlements[0] = "changed"
	again, err := readInitialMembershipQuote(&restored)
	require.NoError(t, err)
	require.Equal(t, quoted, again, "the display projection cannot mutate the accepted quote")

	repriced := price
	repriced.Amount = 123
	require.ErrorIs(t, quoteInitialMembership(ctx, &restored, &repriced, &product, method, now), ErrCheckoutAttemptConflict, "a quoted session is never re-quoted")

	principal := billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: billing.MerchantID(mid), SubjectID: customer.String()}
	accepted, err := acceptedInitialMembershipQuote(ctx, &restored, principal, now.Add(10*time.Minute))
	require.NoError(t, err)
	require.Equal(t, quoted.PaymentID, accepted.PaymentID)
	require.Equal(t, quoted.Amount, accepted.Amount)
	require.Equal(t, now.Add(10*time.Minute), accepted.PeriodStart)
	require.Equal(t, 720*time.Hour, accepted.PeriodEnd.Sub(accepted.PeriodStart))
	for _, mutate := range []func(*billingauth.DelegatedPrincipal){
		func(p *billingauth.DelegatedPrincipal) { p.CredentialClass = billingauth.CredentialClassAutomation },
		func(p *billingauth.DelegatedPrincipal) { p.CredentialClass = billingauth.CredentialClassUnknown },
		func(p *billingauth.DelegatedPrincipal) { p.Invoker = "agent" },
		func(p *billingauth.DelegatedPrincipal) { p.MerchantID = billing.MerchantID(uuid.New()) },
		func(p *billingauth.DelegatedPrincipal) { p.SubjectID = uuid.NewString() },
	} {
		bad := principal
		mutate(&bad)
		_, err := acceptedInitialMembershipQuote(ctx, &restored, bad, now)
		require.ErrorIs(t, err, ErrCheckoutAttemptForbidden)
	}
	_, err = acceptedInitialMembershipQuote(ctx, &restored, principal, expiry)
	require.ErrorIs(t, err, ErrCheckoutAttemptExpired)

	tampered := restored
	tampered.Amount = new(int64(1))
	_, err = readInitialMembershipQuote(&tampered)
	require.ErrorIs(t, err, ErrCheckoutAttemptConflict, "the session row and its quote must agree")
	require.Nil(t, restored.PaymentID, "quoting claims no payment")

	for name, mutate := range map[string]func(*models.Price, *gen.BillingPaymentMethod){
		"free":           func(p *models.Price, _ *gen.BillingPaymentMethod) { p.Amount = 0 },
		"trial":          func(p *models.Price, _ *gen.BillingPaymentMethod) { p.TrialUnitAmount = new(int64(0)) },
		"not recurring":  func(p *models.Price, _ *gen.BillingPaymentMethod) { p.BillingIntervalHours = nil },
		"archived":       func(p *models.Price, _ *gen.BillingPaymentMethod) { p.Archived = true },
		"foreign method": func(_ *models.Price, m *gen.BillingPaymentMethod) { m.CustomerID = uuid.New() },
		"other PSP method": func(_ *models.Price, m *gen.BillingPaymentMethod) {
			other := uuid.New()
			m.Custodian, m.CustodianID, m.PspID = models.CustodianPSP, nil, &other
		},
		"parked method": func(_ *models.Price, m *gen.BillingPaymentMethod) { m.ParkReason = new("pending deletion") },
	} {
		candidate := session
		candidate.RailState = nil
		p, m := price, method
		mutate(&p, &m)
		require.Error(t, quoteInitialMembership(ctx, &candidate, &p, &product, m, now), name)
		require.Empty(t, candidate.RailState, name)
	}
}
