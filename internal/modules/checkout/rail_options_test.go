package checkout

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckoutModeForRail(t *testing.T) {
	t.Parallel()

	recurringPrice := &models.Price{AutoRenew: true}
	solanaRecurringPrice := &models.Price{
		AutoRenew: true,
		PSPLinks: map[string]map[string]string{
			"solana": {
				models.RailKeyRail:  "solana",
				"plan_id":           "1",
				"amount_base_units": "100",
				"period_hours":      "720",
				"mint_symbol":       "USDC",
			},
		},
	}

	tests := []struct {
		name     string
		price    *models.Price
		rail     string
		expected models.CheckoutSessionMode
	}{
		{name: "recurring card price", price: recurringPrice, rail: "stripe", expected: models.CheckoutSessionModeSubscription},
		{name: "solana recurring price without plan stays recurring", price: recurringPrice, rail: "solana", expected: models.CheckoutSessionModeSubscription},
		{name: "solana with plan is recurring", price: solanaRecurringPrice, rail: "solana", expected: models.CheckoutSessionModeSubscription},
		{name: "one off price", price: &models.Price{}, rail: "nmi", expected: models.CheckoutSessionModeOneOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, checkoutModeForRail(tt.price, tt.rail))
		})
	}
}

func TestCheckoutRailSkipReason(t *testing.T) {
	hours := 720
	terms := &models.Price{ID: uuid.New(), Amount: 1000000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours}
	oneoff := &models.Price{ID: uuid.New(), Amount: 1000000, Currency: "USD"}
	for _, tc := range []struct {
		name, rail string
		price      *models.Price
		provider   *config.PSPConfig
		mode       models.CheckoutSessionMode
		want       string
	}{
		{"stripe recurring local terms", "stripe", terms, &config.PSPConfig{Stripe: &config.StripeRailConfig{SecretKey: "sk_test_fixture"}}, models.CheckoutSessionModeSubscription, ""},
		{"nmi recurring local terms", "nmi", terms, &config.PSPConfig{NMI: &config.NMIRailConfig{SecurityKey: "fixture"}}, models.CheckoutSessionModeSubscription, ""},
		{"stripe missing credentials", "stripe", terms, &config.PSPConfig{}, models.CheckoutSessionModeSubscription, models.CheckoutRoutingSkipCredentialsMissing},
		{"nmi missing credentials", "nmi", terms, &config.PSPConfig{}, models.CheckoutSessionModeSubscription, models.CheckoutRoutingSkipCredentialsMissing},
		{"ccbill new enrollment unsupported", "ccbill", terms, &config.PSPConfig{CCBill: &config.CCBillRailConfig{}}, models.CheckoutSessionModeSubscription, models.CheckoutRoutingSkipModeUnsupported},
		{"solana new enrollment unsupported", "solana", terms, &config.PSPConfig{Solana: &config.SolanaRailConfig{}}, models.CheckoutSessionModeSubscription, models.CheckoutRoutingSkipModeUnsupported},
		{"zero amount terms unsupported", "stripe", &models.Price{AutoRenew: true, AccessDurationHours: &hours}, &config.PSPConfig{Stripe: &config.StripeRailConfig{SecretKey: "sk_test_fixture"}}, models.CheckoutSessionModeSubscription, models.CheckoutRoutingSkipModeUnsupported},
		{"stripe oneoff requires no remote price", "stripe", oneoff, &config.PSPConfig{Stripe: &config.StripeRailConfig{SecretKey: "sk_test_fixture"}}, models.CheckoutSessionModeOneOff, ""},
		{"ccbill oneoff unsupported", "ccbill", oneoff, &config.PSPConfig{CCBill: &config.CCBillRailConfig{}}, models.CheckoutSessionModeOneOff, models.CheckoutRoutingSkipModeUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &CheckoutSessionService{}
			require.Equal(t, tc.want, service.checkoutRailSkipReason(tc.price, railTarget{Rail: tc.rail}, tc.provider, tc.mode))
		})
	}
}

func TestListCheckoutRailOptionsForPrice_ReturnsExecutableSelectors(t *testing.T) {
	t.Parallel()

	hours := 720
	price := &models.Price{Amount: 1000000, Currency: "USD", AccessDurationHours: &hours,
		ID:        uuid.New(),
		AutoRenew: true,
		PSPLinks: map[string]map[string]string{
			"stripe": {
				models.RailKeyRail:          "stripe",
				models.RailKeyStripePriceID: "price_test",
			},
			"nmi": {
				models.RailKeyRail:   "nmi",
				models.RailKeyPlanID: "plan_test",
			},
			"ccbill": {
				models.RailKeyRail:           "ccbill",
				models.RailKeyCCBillFormName: "form_test",
				models.RailKeyCCBillFlexID:   "flex_test",
			},
		},
	}
	rails := railresolve.FixedSet{
		"stripe": {
			Rail:      models.RailStripe,
			AccountID: "acct_stripe",
			Stripe:    &config.StripeRailConfig{SecretKey: "sk_test_value"},
		},
		"nmi": {
			Rail:      models.RailNMI,
			AccountID: "acct_nmi",
			NMI:       &config.NMIRailConfig{SecurityKey: "security_test"},
		},
		"ccbill": {
			Rail:      models.RailCCBill,
			AccountID: "945280-0000",
			CCBill:    &config.CCBillRailConfig{},
		},
	}
	checkoutService := &CheckoutService{
		Config:          &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
		Rails:           rails,
		ProviderSecrets: fakePSPCatalog{scopes: routingScopes(rails)},
	}
	svc := &CheckoutSessionService{
		config:          &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
		checkoutService: checkoutService,
	}

	options, err := svc.listCheckoutRailOptionsForPrice(routingContext(), price, &models.Product{})
	require.NoError(t, err)
	require.Equal(t, []CheckoutRailOption{
		{Selector: "stripe", PSPID: merchants.PspID("stripe", "live", "acct_stripe"), Rail: "stripe", Mode: "subscription"},
		{Selector: "nmi", PSPID: merchants.PspID("nmi", "live", "acct_nmi"), Rail: "nmi", Mode: "subscription"},
	}, options)

	for _, option := range options {
		mode, err := svc.resolveMode(option.Mode, option.Selector, price)
		require.NoError(t, err, option.Selector)
		require.Equal(t, models.CheckoutSessionModeSubscription, mode, option.Selector)

		payment := &CheckoutSessionPaymentRequest{Rail: option.Selector}
		user := &UserIdentity{ID: uuid.NewString()}
		switch option.Rail {
		case "nmi":
			payment.PaymentToken = "token_test"
		case "ccbill":
			verifiedEmail := "buyer@example.test"
			user.Email = &verifiedEmail
			payment.NameOnCard = "Test Buyer"
			payment.Zip = "12345"
			payment.Country = "US"
		}
		require.NoError(t, svc.validatePayment(routingContext(), option.Selector, payment, user), option.Selector)
	}
}

func TestCheckoutPSPLinkForTarget(t *testing.T) {
	t.Parallel()

	active := map[string]string{models.RailKeyRail: "nmi", models.RailKeyPlanID: "active-plan"}
	stale := map[string]string{models.RailKeyRail: "nmi", models.RailKeyPlanID: "stale-plan"}
	price := &models.Price{PSPLinks: map[string]map[string]string{
		"mobius": active,
		"other":  stale,
	}}

	assert.Equal(t, active, checkoutPSPLinkForTarget(price, railTarget{PSP: "mobius", Rail: "nmi"}))
	assert.Equal(t, stale, checkoutPSPLinkForTarget(price, railTarget{PSP: "other", Rail: "nmi"}))
	assert.Nil(t, checkoutPSPLinkForTarget(price, railTarget{PSP: "missing", Rail: "nmi"}))

	railNamed := &models.Price{PSPLinks: map[string]map[string]string{"nmi": active}}
	assert.Equal(t, active, checkoutPSPLinkForTarget(railNamed, railTarget{PSP: "nmi", Rail: "nmi"}))
	assert.Nil(t, checkoutPSPLinkForTarget(railNamed, railTarget{PSP: "mobius", Rail: "nmi"}))
}

func TestCheckoutSessionServiceRequireProviderWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      string
		nilConfig bool
		expectErr bool
	}{
		{name: "readonly blocks checkout", mode: config.ProviderWriteModeReadOnly, expectErr: true},
		{name: "missing config blocks checkout", nilConfig: true, expectErr: true},
		{name: "limited allows user checkout", mode: config.ProviderWriteModeLimited},
		{name: "full allows checkout", mode: config.ProviderWriteModeFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cfg *config.Config
			if !tt.nilConfig {
				cfg = &config.Config{ProviderWriteMode: tt.mode}
			}
			svc := &CheckoutSessionService{config: cfg}
			err := svc.requireProviderWrites()
			if tt.expectErr {
				require.ErrorIs(t, err, ErrCheckoutSessionValidation)
				return
			}
			require.NoError(t, err)
		})
	}

	// The gate must run before session persistence or provider interaction.
	svc := &CheckoutSessionService{config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}}
	_, err := svc.CreateSession(context.Background(), &CheckoutSessionCreateRequest{}, &UserIdentity{ID: uuid.NewString()})
	require.ErrorIs(t, err, ErrCheckoutSessionValidation)
}

func railOptionPrice(autoRenew bool, provider string, link map[string]string) *models.Price {
	if link[models.RailKeyRail] == "" {
		link[models.RailKeyRail] = provider
	}
	return &models.Price{
		ID:        uuid.New(),
		AutoRenew: autoRenew,
		PSPLinks:  map[string]map[string]string{provider: link},
	}
}

func TestEngineRoutingUsesLocalTermsAndRejectsUnsupportedRoutes(t *testing.T) {
	hours := 720
	price := &models.Price{Amount: 9990000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours}
	service := &CheckoutSessionService{config: &config.Config{}}
	for _, rail := range []string{"nmi", "stripe"} {
		cfg := &config.PSPConfig{NMI: &config.NMIRailConfig{SecurityKey: "synthetic"}, Stripe: &config.StripeRailConfig{SecretKey: "synthetic"}}
		require.Empty(t, service.checkoutRailSkipReason(price, railTarget{Rail: rail}, cfg, models.CheckoutSessionModeSubscription), "engine needs no provider catalog binding")
	}
	require.Equal(t, models.CheckoutRoutingSkipModeUnsupported, service.checkoutRailSkipReason(price, railTarget{Rail: "ccbill"}, &config.PSPConfig{}, models.CheckoutSessionModeSubscription))
	trial := int64(0)
	price.TrialUnitAmount = &trial
	require.Equal(t, models.CheckoutRoutingSkipModeUnsupported, service.checkoutRailSkipReason(price, railTarget{Rail: "stripe"}, &config.PSPConfig{}, models.CheckoutSessionModeSubscription))
}
