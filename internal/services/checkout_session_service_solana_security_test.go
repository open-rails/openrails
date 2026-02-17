package services

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestInitializeSolanaSession_TransactionRequestRequiresPersistedQuote(t *testing.T) {
	t.Parallel()

	svc := &CheckoutSessionService{
		config:                   testSolanaCheckoutConfig(),
		solanaTransactionService: &SolanaTransactionService{},
	}
	session := &models.CheckoutSession{
		ID:       uuid.New(),
		UserID:   "user_123",
		PriceID:  uuid.New(),
		Amount:   1000,
		Currency: "eur",
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
		solanaTransactionService: &SolanaTransactionService{},
	}
	session := &models.CheckoutSession{
		ID:       uuid.New(),
		UserID:   "user_123",
		PriceID:  uuid.New(),
		Amount:   0,
		Currency: "usd",
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
		solanaTransactionService: &SolanaTransactionService{},
		checkoutService:          &CheckoutService{},
	}
	ref := "11111111111111111111111111111112"
	session := &models.CheckoutSession{
		ID:        uuid.New(),
		UserID:    "user_123",
		PriceID:   uuid.New(),
		Amount:    1000,
		Currency:  "usd",
		Reference: &ref,
		ProcessorState: map[string]any{
			"token_symbol": "USDC",
			"token_mint":   devnetUSDCMint,
			"recipient":    testRecipientWallet,
		},
	}
	req := &CheckoutSessionConfirmRequest{Payment: CheckoutSessionConfirmPayment{Signature: testSignature}}

	_, err := svc.confirmSolanaSession(context.Background(), session, req, &UserIdentity{ID: session.UserID})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCheckoutSessionValidation)
	require.Contains(t, err.Error(), "token_amount missing or invalid")
}

func TestConfirmSolanaSession_RequiresRecipientAndReference(t *testing.T) {
	t.Parallel()

	svc := &CheckoutSessionService{
		config:                   testSolanaCheckoutConfig(),
		solanaTransactionService: &SolanaTransactionService{},
		checkoutService:          &CheckoutService{},
	}

	t.Run("missing recipient", func(t *testing.T) {
		t.Parallel()

		ref := "11111111111111111111111111111112"
		session := &models.CheckoutSession{
			ID:        uuid.New(),
			UserID:    "user_123",
			PriceID:   uuid.New(),
			Amount:    1000,
			Currency:  "usd",
			Reference: &ref,
			ProcessorState: map[string]any{
				"token_symbol": "USDC",
				"token_mint":   devnetUSDCMint,
				"token_amount": uint64(1234567),
			},
		}
		req := &CheckoutSessionConfirmRequest{Payment: CheckoutSessionConfirmPayment{Signature: testSignature}}

		_, err := svc.confirmSolanaSession(context.Background(), session, req, &UserIdentity{ID: session.UserID})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrCheckoutSessionValidation)
		require.Contains(t, err.Error(), "recipient missing")
	})

	t.Run("missing reference", func(t *testing.T) {
		t.Parallel()

		session := &models.CheckoutSession{
			ID:       uuid.New(),
			UserID:   "user_123",
			PriceID:  uuid.New(),
			Amount:   1000,
			Currency: "usd",
			ProcessorState: map[string]any{
				"token_symbol": "USDC",
				"token_mint":   devnetUSDCMint,
				"token_amount": uint64(1234567),
				"recipient":    testRecipientWallet,
			},
		}
		req := &CheckoutSessionConfirmRequest{Payment: CheckoutSessionConfirmPayment{Signature: testSignature}}

		_, err := svc.confirmSolanaSession(context.Background(), session, req, &UserIdentity{ID: session.UserID})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrCheckoutSessionValidation)
		require.Contains(t, err.Error(), "reference missing")
	})
}

const (
	devnetUSDCMint      = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	testRecipientWallet = "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh"
	testSignature       = "3zJ4f8M2wQnV6r9P5kL2xT7hN4bC1dF8sG6mY3qR9uP2aV7eH5jK1nM8tC4xB6rD9pL2wQ7fN5gH3kJ1mV8x"
)

func testSolanaCheckoutConfig() *config.Config {
	return &config.Config{
		Processors: map[string]*config.ProcessorConfig{
			"solana": {
				Type:            config.ProcessorTypeSolana,
				Network:         "devnet",
				RecipientWallet: testRecipientWallet,
				SupportedTokens: map[string]config.TokenConfig{
					"USDC": {
						Symbol:   "USDC",
						Mint:     devnetUSDCMint,
						Decimals: 6,
						Enabled:  true,
					},
				},
			},
		},
	}
}
