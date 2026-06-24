package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func TestInitializeSolanaSession_TransactionRequestRequiresPersistedQuote(t *testing.T) {
	t.Parallel()

	svc := &CheckoutSessionService{
		config:                   testSolanaCheckoutConfig(),
		rails:                    testSolanaCheckoutRails(),
		solanaTransactionService: &stubSolanaTransactionService{},
	}
	session := &models.CheckoutSession{
		ID:         uuid.New(),
		CustomerID: identity.CustomerIDFromString("user_123").UUID(),
		PriceID:    uuid.New(),
		Amount:     1000,
		Currency:   "eur",
	}
	payment := &CheckoutSessionPaymentRequest{
		TokenSymbol: "USDC",
		Flow:        "transaction_request",
	}

	err := svc.initializeSolanaSession(context.Background(), session, payment)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCheckoutSessionValidation)
	require.Contains(t, err.Error(), "failed to calculate solana token quote")
}

func TestInitializeSolanaSession_TransactionRequestRejectsZeroTokenAmount(t *testing.T) {
	t.Parallel()

	svc := &CheckoutSessionService{
		config:                   testSolanaCheckoutConfig(),
		rails:                    testSolanaCheckoutRails(),
		solanaTransactionService: &stubSolanaTransactionService{},
	}
	session := &models.CheckoutSession{
		ID:         uuid.New(),
		CustomerID: identity.CustomerIDFromString("user_123").UUID(),
		PriceID:    uuid.New(),
		Amount:     0,
		Currency:   "usd",
	}
	payment := &CheckoutSessionPaymentRequest{
		TokenSymbol: "USDC",
		Flow:        "transaction_request",
	}

	err := svc.initializeSolanaSession(context.Background(), session, payment)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCheckoutSessionValidation)
	require.Contains(t, err.Error(), "token_amount must be greater than 0")
}

func TestConfirmSolanaSession_RequiresTokenAmount(t *testing.T) {
	t.Parallel()

	svc := &CheckoutSessionService{
		config:                   testSolanaCheckoutConfig(),
		rails:                    testSolanaCheckoutRails(),
		solanaTransactionService: &stubSolanaTransactionService{},
		checkoutService:          &stubCheckoutExecutor{},
	}
	ref := "11111111111111111111111111111112"
	session := &models.CheckoutSession{
		ID:         uuid.New(),
		CustomerID: identity.CustomerIDFromString("user_123").UUID(),
		PriceID:    uuid.New(),
		Amount:     1000,
		Currency:   "usd",
		Reference:  &ref,
		RailState: map[string]any{
			"token_symbol": "USDC",
			"token_mint":   devnetUSDCMint,
			"recipient":    testRecipientWallet,
		},
	}
	req := &CheckoutSessionConfirmRequest{Payment: CheckoutSessionConfirmPayment{Signature: testSignature}}

	_, err := svc.confirmSolanaSession(context.Background(), session, req, &UserIdentity{ID: session.CustomerID.String()})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCheckoutSessionValidation)
	require.Contains(t, err.Error(), "token_amount missing or invalid")
}

func TestConfirmSolanaSession_RequiresRecipientAndReference(t *testing.T) {
	t.Parallel()

	svc := &CheckoutSessionService{
		config:                   testSolanaCheckoutConfig(),
		rails:                    testSolanaCheckoutRails(),
		solanaTransactionService: &stubSolanaTransactionService{},
		checkoutService:          &stubCheckoutExecutor{},
	}

	t.Run("missing recipient", func(t *testing.T) {
		t.Parallel()

		ref := "11111111111111111111111111111112"
		session := &models.CheckoutSession{
			ID:         uuid.New(),
			CustomerID: identity.CustomerIDFromString("user_123").UUID(),
			PriceID:    uuid.New(),
			Amount:     1000,
			Currency:   "usd",
			Reference:  &ref,
			RailState: map[string]any{
				"token_symbol": "USDC",
				"token_mint":   devnetUSDCMint,
				"token_amount": uint64(1234567),
			},
		}
		req := &CheckoutSessionConfirmRequest{Payment: CheckoutSessionConfirmPayment{Signature: testSignature}}

		_, err := svc.confirmSolanaSession(context.Background(), session, req, &UserIdentity{ID: session.CustomerID.String()})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrCheckoutSessionValidation)
		require.Contains(t, err.Error(), "recipient missing")
	})

	t.Run("missing reference", func(t *testing.T) {
		t.Parallel()

		session := &models.CheckoutSession{
			ID:         uuid.New(),
			CustomerID: identity.CustomerIDFromString("user_123").UUID(),
			PriceID:    uuid.New(),
			Amount:     1000,
			Currency:   "usd",
			RailState: map[string]any{
				"token_symbol": "USDC",
				"token_mint":   devnetUSDCMint,
				"token_amount": uint64(1234567),
				"recipient":    testRecipientWallet,
			},
		}
		req := &CheckoutSessionConfirmRequest{Payment: CheckoutSessionConfirmPayment{Signature: testSignature}}

		_, err := svc.confirmSolanaSession(context.Background(), session, req, &UserIdentity{ID: session.CustomerID.String()})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrCheckoutSessionValidation)
		require.Contains(t, err.Error(), "reference missing")
	})
}

func TestIsSolanaTransferRequestFlow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		session *models.CheckoutSession
		want    bool
	}{
		{
			name: "empty session defaults to transfer request",
			session: &models.CheckoutSession{
				RailState: map[string]any{},
			},
			want: true,
		},
		{
			name: "explicit transfer request",
			session: &models.CheckoutSession{
				RailState: map[string]any{"flow": "transfer_request"},
			},
			want: true,
		},
		{
			name: "transaction request is false",
			session: &models.CheckoutSession{
				RailState: map[string]any{"flow": "transaction_request"},
			},
			want: false,
		},
		{
			name: "nil session is false",
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, isSolanaTransferRequestFlow(tc.session))
		})
	}
}

func TestSessionToResponse_TransactionRequestSolanaPayURLUsesCanonicalV1Path(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		apiURL         string
		expectedPrefix string
	}{
		{
			name:           "standalone api url",
			apiURL:         "https://api.test.com",
			expectedPrefix: "solana:https://api.test.com/v1/checkout/",
		},
		{
			name:           "embedded api url",
			apiURL:         "https://api.test.com/billing",
			expectedPrefix: "solana:https://api.test.com/billing/v1/checkout/",
		},
		{
			name:           "api url with trailing slash",
			apiURL:         "https://api.test.com/",
			expectedPrefix: "solana:https://api.test.com/v1/checkout/",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := testSolanaCheckoutConfig()
			cfg.APIURL = tc.apiURL

			svc := &CheckoutSessionService{config: cfg, rails: testSolanaCheckoutRails()}
			session := &models.CheckoutSession{
				ID:     uuid.New(),
				Status: models.CheckoutSessionStatusRequiresAction,
				Rail:   models.RailSolana,
				RailState: map[string]any{
					"flow": "transaction_request",
				},
			}

			resp := svc.sessionToResponse(session)
			require.NotNil(t, resp)
			require.Contains(t, resp.Payment.SolanaPayURL, tc.expectedPrefix)
			require.Contains(t, resp.Payment.SolanaPayURL, "/solana-pay")
		})
	}
}

func TestSolanaBuildRequestFromSessionUsesPersistedQuote(t *testing.T) {
	t.Parallel()

	ref := "11111111111111111111111111111112"
	priceID := uuid.New()
	tenantSubjectID := uuid.New()
	session := &models.CheckoutSession{
		ID:         uuid.New(),
		CustomerID: tenantSubjectID,
		PriceID:    priceID,
		Amount:     10000,
		Currency:   "usd",
		Reference:  &ref,
		RailState: map[string]any{
			"token_symbol": "USDC",
			"token_mint":   devnetUSDCMint,
			"token_amount": uint64(100_000_000),
			"recipient":    testRecipientWallet,
		},
	}

	req, err := solanaBuildRequestFromSession(session, "payer_wallet", "USDC")
	require.NoError(t, err)
	require.Equal(t, tenantSubjectID.String(), req.UserID)
	require.Equal(t, priceID, req.PriceID)
	require.Equal(t, "USDC", req.TokenSymbol)
	require.Equal(t, "payer_wallet", req.UserWallet)
	require.Equal(t, &ref, req.Reference)
	require.Equal(t, uint64(100_000_000), req.TokenAmount)
	require.Equal(t, devnetUSDCMint, req.TokenMint)
	require.Equal(t, testRecipientWallet, req.Recipient)
	require.Equal(t, int64(10000), req.Amount)
	require.Equal(t, "usd", req.Currency)

	session.RailState["token_amount"] = uint64(1_000_000)
	req, err = solanaBuildRequestFromSession(session, "payer_wallet", "USDC")
	require.NoError(t, err)
	require.Equal(t, uint64(1_000_000), req.TokenAmount)
}

const (
	devnetUSDCMint      = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	testRecipientWallet = "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh"
	testSignature       = "3zJ4f8M2wQnV6r9P5kL2xT7hN4bC1dF8sG6mY3qR9uP2aV7eH5jK1nM8tC4xB6rD9pL2wQ7fN5gH3kJ1mV8x"
)

func testSolanaCheckoutConfig() *config.Config {
	return &config.Config{
		TestMode: true,
	}
}

func testSolanaCheckoutRails() config.RailSet {
	return config.RailSet{
		"solana": {
			Type:            config.RailTypeSolana,
			Network:         "devnet",
			RecipientWallet: testRecipientWallet,
			Tokens: map[string]config.TokenConfig{
				"USDC": {
					Mint:     devnetUSDCMint,
					Decimals: 6,
				},
			},
		},
	}
}

type stubSolanaTransactionService struct{}

func (s *stubSolanaTransactionService) BuildPaymentTransactionFromQuote(ctx context.Context, req *solanamodule.PaymentTransactionBuildRequest) (*solanamodule.TransactionBuildResponse, error) {
	return nil, nil
}

func (s *stubSolanaTransactionService) VerifyTransactionWithContent(ctx context.Context, signature string, expectedAmount uint64, expectedRecipient string, expectedTokenMint string, expectedPayer string, expectedReference *string, processedNotAfter *time.Time) error {
	return nil
}

type stubCheckoutExecutor struct{}

func (s *stubCheckoutExecutor) Checkout(ctx context.Context, req *CheckoutRequest, user *UserIdentity) (*CheckoutResponse, error) {
	return nil, nil
}

func (s *stubCheckoutExecutor) RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error) {
	return nil, nil
}

func (s *stubCheckoutExecutor) CheckSubscriptionConflict(ctx context.Context, userID string, price *models.Price, product *models.Product) (*SubscriptionConflict, error) {
	return &SubscriptionConflict{}, nil
}
