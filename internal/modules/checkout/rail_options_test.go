package checkout

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
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
	t.Parallel()

	stripePrice := railOptionPrice(true, "stripe", map[string]string{
		models.RailKeyStripePriceID: "price_test",
	})
	nmiPrice := railOptionPrice(true, "mobius", map[string]string{
		models.RailKeyRail:   "nmi",
		models.RailKeyPlanID: "plan_test",
	})
	ccbillPrice := railOptionPrice(true, "ccbill", map[string]string{
		models.RailKeyCCBillFormName: "form_test",
		models.RailKeyCCBillFlexID:   "flex_test",
	})
	solanaPrice := railOptionPrice(true, "solana", map[string]string{
		"plan_id":           "1",
		"amount_base_units": "100",
		"period_hours":      "720",
		"mint_symbol":       "USDC",
	})

	solanaReady := &CheckoutSessionService{
		solanaPrepareSubscribe: &recurring.PrepareSubscribeService{},
		solanaEnroll:           &recurring.EnrollService{},
	}

	tests := []struct {
		name           string
		service        *CheckoutSessionService
		price          *models.Price
		target         railTarget
		providerConfig *config.PSPConfig
		mode           models.CheckoutSessionMode
		// wantSkip is the or#288 skip class, "" when the PSP is ready.
		wantSkip string
	}{
		{
			name:    "stripe recurring ready",
			service: &CheckoutSessionService{},
			price:   stripePrice,
			target:  railTarget{PSP: "stripe", Rail: "stripe"},
			providerConfig: &config.PSPConfig{
				Rail:   models.RailStripe,
				Stripe: &config.StripeRailConfig{SecretKey: "sk_test_value"},
			},
			mode: models.CheckoutSessionModeSubscription,
		},
		{
			name:    "stripe missing price link",
			service: &CheckoutSessionService{},
			price:   &models.Price{ID: uuid.New(), AutoRenew: true},
			target:  railTarget{PSP: "stripe", Rail: "stripe"},
			providerConfig: &config.PSPConfig{
				Rail:   models.RailStripe,
				Stripe: &config.StripeRailConfig{SecretKey: "sk_test_value"},
			},
			mode:     models.CheckoutSessionModeSubscription,
			wantSkip: models.CheckoutRoutingSkipLinkMissing,
		},
		{
			name:    "stripe ambiguous account links",
			service: &CheckoutSessionService{},
			price: &models.Price{ID: uuid.New(), AutoRenew: true, PSPLinks: map[string]map[string]string{
				"stripe": {models.RailKeyRail: "stripe", models.RailKeyStripePriceID: "price_active"},
				"old":    {models.RailKeyRail: "stripe", models.RailKeyStripePriceID: "price_stale"},
			}},
			target: railTarget{PSP: "stripe", Rail: "stripe"},
			providerConfig: &config.PSPConfig{
				Rail:   models.RailStripe,
				Stripe: &config.StripeRailConfig{SecretKey: "sk_test_value"},
			},
			mode:     models.CheckoutSessionModeSubscription,
			wantSkip: models.CheckoutRoutingSkipLinkMissing,
		},
		{
			name:    "nmi recurring ready for exact provider",
			service: &CheckoutSessionService{},
			price:   nmiPrice,
			target:  railTarget{PSP: "mobius", Rail: "nmi"},
			providerConfig: &config.PSPConfig{
				Rail: models.RailNMI,
				NMI:  &config.NMIRailConfig{SecurityKey: "security_test"},
			},
			mode: models.CheckoutSessionModeSubscription,
		},
		{
			name:    "nmi recurring missing exact provider plan",
			service: &CheckoutSessionService{},
			price:   nmiPrice,
			target:  railTarget{PSP: "other", Rail: "nmi"},
			providerConfig: &config.PSPConfig{
				Rail: models.RailNMI,
				NMI:  &config.NMIRailConfig{SecurityKey: "security_test"},
			},
			mode:     models.CheckoutSessionModeSubscription,
			wantSkip: models.CheckoutRoutingSkipLinkMissing,
		},
		{
			name:    "ccbill recurring ready",
			service: &CheckoutSessionService{},
			price:   ccbillPrice,
			target:  railTarget{PSP: "ccbill", Rail: "ccbill"},
			providerConfig: &config.PSPConfig{
				Rail:      models.RailCCBill,
				AccountID: "945280-0000",
				CCBill:    &config.CCBillRailConfig{},
			},
			mode: models.CheckoutSessionModeSubscription,
		},
		{
			name:    "ccbill one off unsupported",
			service: &CheckoutSessionService{},
			price:   ccbillPrice,
			target:  railTarget{PSP: "ccbill", Rail: "ccbill"},
			providerConfig: &config.PSPConfig{
				Rail:      models.RailCCBill,
				AccountID: "945280-0000",
				CCBill:    &config.CCBillRailConfig{},
			},
			mode:     models.CheckoutSessionModeOneOff,
			wantSkip: models.CheckoutRoutingSkipModeUnsupported,
		},
		{
			name:    "solana recurring ready",
			service: solanaReady,
			price:   solanaPrice,
			target:  railTarget{PSP: "solana", Rail: "solana"},
			providerConfig: &config.PSPConfig{
				Rail: models.RailSolana,
				Solana: &config.SolanaRailConfig{Tokens: map[string]config.TokenConfig{
					"USDC": {Mint: "mint_test"},
				}},
			},
			mode: models.CheckoutSessionModeSubscription,
		},
		{
			name:    "solana recurring services unavailable",
			service: &CheckoutSessionService{},
			price:   solanaPrice,
			target:  railTarget{PSP: "solana", Rail: "solana"},
			providerConfig: &config.PSPConfig{
				Rail: models.RailSolana,
				Solana: &config.SolanaRailConfig{Tokens: map[string]config.TokenConfig{
					"USDC": {Mint: "mint_test"},
				}},
			},
			mode:     models.CheckoutSessionModeSubscription,
			wantSkip: models.CheckoutRoutingSkipServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.wantSkip, tt.service.checkoutRailSkipReason(tt.price, tt.target, tt.providerConfig, tt.mode))
		})
	}
}

func TestListCheckoutRailOptionsForPrice_ReturnsExecutableSelectors(t *testing.T) {
	t.Parallel()

	price := &models.Price{
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
		{Selector: "ccbill", PSPID: merchants.PspID("ccbill", "live", "945280-0000"), Rail: "ccbill", Mode: "subscription"},
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
