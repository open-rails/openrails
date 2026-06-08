package solana

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	log "github.com/sirupsen/logrus"
)

// SolanaTransactionService builds real Solana transactions for payments.
type SolanaTransactionService struct {
	db           *db.DB
	rpc          *solanarpc.RPCClient
	cfg          *config.Config
	priceService *catalog.PriceService
	fxProvider   fx.Provider
	clock        clockwork.Clock
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *SolanaTransactionService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// NewSolanaTransactionService creates a new transaction service.
func NewSolanaTransactionService(db *db.DB, rpc *solanarpc.RPCClient, cfg *config.Config, price *catalog.PriceService, fxProvider fx.Provider, clocks ...clockwork.Clock) *SolanaTransactionService {
	return &SolanaTransactionService{
		db:           db,
		rpc:          rpc,
		cfg:          cfg,
		priceService: price,
		fxProvider:   fxProvider,
		clock:        timeutil.FirstClock(clocks...),
	}
}

func (s *SolanaTransactionService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *SolanaTransactionService) Clock() clockwork.Clock {
	return s.clock
}

// BuildPaymentTransactionFromQuote creates a payment transaction from the quote already bound to a checkout session.
func (s *SolanaTransactionService) BuildPaymentTransactionFromQuote(ctx context.Context, req *PaymentTransactionBuildRequest) (*TransactionBuildResponse, error) {
	if s.rpc == nil {
		return nil, fmt.Errorf("solana rpc client unavailable")
	}
	if req == nil {
		return nil, fmt.Errorf("payment transaction request is required")
	}
	if strings.TrimSpace(req.UserWallet) == "" {
		return nil, fmt.Errorf("user wallet is required")
	}
	if strings.TrimSpace(req.Recipient) == "" {
		return nil, fmt.Errorf("merchant wallet not configured")
	}
	if req.TokenAmount == 0 {
		return nil, fmt.Errorf("token amount is required")
	}
	if !isNativeTokenSymbol(req.TokenSymbol) && strings.TrimSpace(req.TokenMint) == "" {
		return nil, fmt.Errorf("token mint is required")
	}
	if !isNativeTokenSymbol(req.TokenSymbol) && IsNativeSOLMint(req.TokenMint) {
		return nil, fmt.Errorf("non-SOL token cannot use native SOL mint")
	}

	referenceStr := ""
	if req.Reference != nil {
		referenceStr = strings.TrimSpace(*req.Reference)
	}

	txResp, err := s.rpc.BuildTransferTransaction(ctx, solanarpc.TransferRequest{
		FromWallet:  strings.TrimSpace(req.UserWallet),
		ToWallet:    strings.TrimSpace(req.Recipient),
		TokenSymbol: strings.TrimSpace(req.TokenSymbol),
		TokenMint:   strings.TrimSpace(req.TokenMint),
		Amount:      req.TokenAmount,
		Reference:   referenceStr,
	})
	if err != nil {
		return nil, err
	}

	expiresAt := s.now().Add(10 * time.Minute)

	log.WithFields(log.Fields{
		"user_id":      req.UserID,
		"price_id":     req.PriceID,
		"token":        req.TokenSymbol,
		"amount_cents": req.Amount,
		"token_amount": req.TokenAmount,
		"from_wallet":  req.UserWallet,
		"to_wallet":    req.Recipient,
	}).Info("Built Solana payment transaction from checkout quote")

	return &TransactionBuildResponse{
		TransactionBase64: txResp.TransactionBase64,
		Amount:            req.Amount,
		TokenAmount:       req.TokenAmount,
		TokenSymbol:       req.TokenSymbol,
		ExpiresAt:         expiresAt,
		Instructions:      fmt.Sprintf("Sign this transaction to pay %s %s using %s", moneyutil.FormatCentsDecimal(req.Amount), req.Currency, req.TokenSymbol),
	}, nil
}

func isNativeTokenSymbol(symbol string) bool {
	return strings.EqualFold(strings.TrimSpace(symbol), "SOL")
}

// VerifyTransactionWithContent verifies a transaction against expected recipient, amount, and reference.
func (s *SolanaTransactionService) VerifyTransactionWithContent(ctx context.Context, signature string, expectedAmount uint64, expectedRecipient string, expectedTokenMint string, expectedPayer string, expectedReference *string, processedNotAfter *time.Time) error {
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

	return s.rpc.VerifyTransfer(ctx, solanarpc.VerifyTransferRequest{
		Signature:         strings.TrimSpace(signature),
		ExpectedAmount:    expectedAmount,
		ExpectedRecipient: strings.TrimSpace(expectedRecipient),
		ExpectedTokenMint: strings.TrimSpace(expectedTokenMint),
		ExpectedPayer:     strings.TrimSpace(expectedPayer),
		ExpectedReference: reference,
		ProcessedNotAfter: processedNotAfter,
	})
}
