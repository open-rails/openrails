package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	log "github.com/sirupsen/logrus"
)

// SolanaTransactionService builds real Solana transactions for payments.
type SolanaTransactionService struct {
	db           *db.DB
	rpc          *solana.RPCClient
	cfg          *config.Config
	priceService *catalog.PriceService
	paymentSvc   *payments.PaymentService
	fxProvider   fx.Provider
	Clock        clockwork.Clock
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *SolanaTransactionService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// NewSolanaTransactionService creates a new transaction service.
func NewSolanaTransactionService(db *db.DB, rpc *solana.RPCClient, cfg *config.Config, price *catalog.PriceService, payment *payments.PaymentService, fxProvider fx.Provider) *SolanaTransactionService {
	return &SolanaTransactionService{
		db:           db,
		rpc:          rpc,
		cfg:          cfg,
		priceService: price,
		paymentSvc:   payment,
		fxProvider:   fxProvider,
	}
}

// BuildPaymentTransaction creates a Solana transaction for payment.
func (s *SolanaTransactionService) BuildPaymentTransaction(ctx context.Context, userID string, priceID uuid.UUID, tokenSymbol, userWallet string, reference *string) (*payments.SolanaTransactionBuildResponse, error) {
	if s.rpc == nil {
		return nil, fmt.Errorf("solana rpc client unavailable")
	}

	price, err := s.priceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get price: %w", err)
	}

	solanaProc, err := payments.RequireSolanaProcessorConfig(s.cfg)
	if err != nil {
		return nil, err
	}

	tokenCfg, ok := solanaProc.SupportedTokens[tokenSymbol]
	if !ok || !tokenCfg.Enabled {
		return nil, fmt.Errorf("token %s not supported", tokenSymbol)
	}

	merchantWallet := solanaProc.RecipientWallet
	if merchantWallet == "" {
		return nil, fmt.Errorf("merchant wallet not configured")
	}

	quote, err := payments.CalculateTokenQuote(ctx, tokenCfg, price.Amount, price.Currency, s.fxProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate token amount: %w", err)
	}
	tokenAmount := quote.Units
	tokenAmountDecimal := quote.Decimal
	if tokenAmount == 0 {
		return nil, fmt.Errorf("calculated token amount is zero")
	}

	referenceStr := ""
	if reference != nil {
		referenceStr = strings.TrimSpace(*reference)
	}

	txResp, err := s.rpc.BuildTransferTransaction(ctx, solana.TransferRequest{
		FromWallet:  userWallet,
		ToWallet:    merchantWallet,
		TokenSymbol: tokenSymbol,
		TokenMint:   tokenCfg.Mint,
		Amount:      tokenAmount,
		Reference:   referenceStr,
	})
	if err != nil {
		return nil, err
	}

	expiresAt := s.now().Add(10 * time.Minute)

	log.WithFields(log.Fields{
		"user_id":      userID,
		"price_id":     priceID,
		"token":        tokenSymbol,
		"amount_cents": price.Amount,
		"token_amount": tokenAmountDecimal,
		"from_wallet":  userWallet,
		"to_wallet":    merchantWallet,
	}).Info("Built Solana payment transaction")

	return &payments.SolanaTransactionBuildResponse{
		TransactionBase64: txResp.TransactionBase64,
		Amount:            price.Amount,
		TokenAmount:       tokenAmount,
		TokenSymbol:       tokenSymbol,
		ExpiresAt:         expiresAt,
		Instructions:      fmt.Sprintf("Sign this transaction to pay %s %s using %s", moneyutil.FormatCentsDecimal(price.Amount), price.Currency, tokenSymbol),
	}, nil
}

// VerifyTransactionWithContent verifies a transaction against expected recipient, amount, and reference.
func (s *SolanaTransactionService) VerifyTransactionWithContent(ctx context.Context, signature string, expectedAmount uint64, expectedRecipient string, expectedTokenMint string, expectedPayer string, expectedReference *string) error {
	if s.rpc == nil {
		return fmt.Errorf("solana rpc client unavailable")
	}
	if expectedAmount == 0 {
		return fmt.Errorf("expected amount must be greater than 0")
	}
	if strings.TrimSpace(expectedRecipient) == "" {
		return fmt.Errorf("expected recipient is required")
	}

	reference := ""
	if expectedReference != nil {
		reference = strings.TrimSpace(*expectedReference)
	}
	if reference == "" {
		return fmt.Errorf("expected reference is required")
	}

	return s.rpc.VerifyTransfer(ctx, solana.VerifyTransferRequest{
		Signature:         strings.TrimSpace(signature),
		ExpectedAmount:    expectedAmount,
		ExpectedRecipient: strings.TrimSpace(expectedRecipient),
		ExpectedTokenMint: strings.TrimSpace(expectedTokenMint),
		ExpectedPayer:     strings.TrimSpace(expectedPayer),
		ExpectedReference: reference,
	})
}
