package solana

import (
	"context"
	"fmt"
	"strings"
	"time"

	chainrpc "github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// SolanaTransactionService builds real Solana transactions for payments.
type SolanaTransactionService struct {
	db           *db.DB
	rpc          *solanarpc.RPCClient
	rpcBuilder   *MerchantRPCBuilder
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

// SetMerchantRPC installs the store-aware per-merchant RPC builder (#728/#788):
// transaction builds/verifies arm from the ctx merchant's declared account.
func (s *SolanaTransactionService) SetMerchantRPC(b *MerchantRPCBuilder) {
	if b != nil {
		s.rpcBuilder = b
	}
}

// rpcClient resolves the RPC client for this call: the injected test client
// when set, else the ctx merchant's armed client. nil = not armed (callers
// fail closed with their existing "not configured" errors).
func (s *SolanaTransactionService) rpcClient(ctx context.Context) *solanarpc.RPCClient {
	if s.rpc != nil {
		return s.rpc
	}
	if s.rpcBuilder == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil
	}
	client, err := s.rpcBuilder.Resolve(ctx, mid)
	if err != nil {
		return nil
	}
	return client
}

func (s *SolanaTransactionService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

// WithRPC returns a shallow copy bound to rpc — the #728 per-merchant-pass
// seam (the poller verifies each merchant's payments with that merchant's
// store-armed client). nil-receiver-safe.
func (s *SolanaTransactionService) WithRPC(rpc *solanarpc.RPCClient) *SolanaTransactionService {
	if s == nil {
		return nil
	}
	cp := *s
	cp.rpc = rpc
	return &cp
}

func (s *SolanaTransactionService) Clock() clockwork.Clock {
	return s.clock
}

// BuildPaymentTransactionFromQuote creates a payment transaction from the quote already bound to a checkout session.
func (s *SolanaTransactionService) BuildPaymentTransactionFromQuote(ctx context.Context, req *PaymentTransactionBuildRequest) (*TransactionBuildResponse, error) {
	rpc := s.rpcClient(ctx)
	if rpc == nil {
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
	if req.SessionID == uuid.Nil {
		// #713: every purchase tx we construct carries the memo stamp; refuse to
		// build unstamped rather than silently omit it.
		return nil, fmt.Errorf("checkout session id is required (purchase memo local-id)")
	}

	referenceStr := ""
	if req.Reference != nil {
		referenceStr = strings.TrimSpace(*req.Reference)
	}

	txResp, err := rpc.BuildTransferTransaction(ctx, solanarpc.TransferRequest{
		FromWallet:  strings.TrimSpace(req.UserWallet),
		ToWallet:    strings.TrimSpace(req.Recipient),
		TokenSymbol: strings.TrimSpace(req.TokenSymbol),
		TokenMint:   strings.TrimSpace(req.TokenMint),
		Amount:      req.TokenAmount,
		Reference:   referenceStr,
		Memo:        solanarpc.PurchaseMemo(req.SessionID),
	})
	if err != nil {
		return nil, err
	}

	// Price validity for the wallet's display, nothing more (xs-007 row 35):
	// the chain bounds when a built transaction can still land (its
	// blockhash), and a landing is verified by content, not by this clock.
	expiresAt := s.now().Add(10 * time.Minute)

	log.WithFields(log.Fields{
		"user_id":       req.UserID,
		"price_id":      req.PriceID,
		"token":         req.TokenSymbol,
		"amount_micros": req.Amount,
		"token_amount":  req.TokenAmount,
		"from_wallet":   req.UserWallet,
		"to_wallet":     req.Recipient,
	}).Info("Built Solana payment transaction from checkout quote")

	return &TransactionBuildResponse{
		TransactionBase64:    txResp.TransactionBase64,
		Amount:               req.Amount,
		TokenAmount:          req.TokenAmount,
		TokenSymbol:          req.TokenSymbol,
		ExpiresAt:            expiresAt,
		Instructions:         PaymentInstructions(req.Amount, req.Currency, req.TokenSymbol),
		LastValidBlockHeight: txResp.LastValidBlockHeight,
	}, nil
}

func isNativeTokenSymbol(symbol string) bool {
	return strings.EqualFold(strings.TrimSpace(symbol), "SOL")
}

// ObserveTransfer reads what a landed transaction paid the recipient for a
// reference. A transfer that landed moved the buyer's money whatever the
// quote clock says (xs-007 row 35); what it settles is the ledger's decision.
func (s *SolanaTransactionService) ObserveTransfer(ctx context.Context, req solanarpc.ObserveTransferRequest) (*solanarpc.TransferObservation, error) {
	rpc := s.rpcClient(ctx)
	if rpc == nil {
		return nil, fmt.Errorf("solana rpc client unavailable")
	}
	return rpc.ObserveTransfer(ctx, req)
}

// BlockHeight is the cluster's confirmed block height: a built transaction can
// land only while it is at most the blockhash's last valid height.
func (s *SolanaTransactionService) BlockHeight(ctx context.Context) (uint64, error) {
	rpc := s.rpcClient(ctx)
	if rpc == nil {
		return 0, fmt.Errorf("solana rpc client unavailable")
	}
	return rpc.GetBlockHeight(ctx, chainrpc.CommitmentConfirmed)
}

// ReferenceHasTransfers reports whether any transaction naming the reference
// has landed at confirmed commitment.
func (s *SolanaTransactionService) ReferenceHasTransfers(ctx context.Context, reference string) (bool, error) {
	rpc := s.rpcClient(ctx)
	if rpc == nil {
		return false, fmt.Errorf("solana rpc client unavailable")
	}
	return rpc.HasConfirmedSignatures(ctx, reference)
}

// PaymentInstructions is the wallet message for a one-off payment.
func PaymentInstructions(amount int64, currency, tokenSymbol string) string {
	return fmt.Sprintf("Sign this transaction to pay %s using %s", moneyutil.FormatAmount(amount, currency), tokenSymbol)
}
