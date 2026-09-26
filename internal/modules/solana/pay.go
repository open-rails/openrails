package solana

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// pendingPaymentTTL is how long a transfer-request quote is offered. It is
// price validity, never a refusal of settled money: a transfer landing within
// SettlementGrace after it is still credited at the quoted amount.
const pendingPaymentTTL = 15 * time.Minute

type purchaseEligibilityChecker interface {
	CheckPurchaseEligibility(ctx context.Context, userID string, priceID uuid.UUID) (*PurchaseEligibilityResult, error)
}

type PurchaseEligibilityResult struct {
	Status string
	Reason string
}

const (
	eligibilityAllowed   = "allowed"
	eligibilityBlocked   = "blocked"
	eligibilityUpgrade   = "upgrade"
	eligibilityDowngrade = "downgrade"
)

// SolanaPayService handles Solana Pay Transfer Request flow
type SolanaPayService struct {
	db                 *db.DB
	cfg                *config.Config
	rails              railresolve.Source
	clock              clockwork.Clock
	priceService       *catalog.PriceService
	productService     *catalog.ProductService
	eligibilityChecker purchaseEligibilityChecker
	fxProvider         fx.Provider
	priceProvider      TokenPriceProvider
	// mints reads SPL mint decimals from the chain (#817). Late-bound like the
	// poller's merchant RPC: it needs the per-merchant RPC resolver, which is
	// armed after service construction. nil = quotes fail closed.
	mints MintDecimalsSource
}

// NewSolanaPayService creates a new SolanaPayService
func NewSolanaPayService(
	db *db.DB,
	cfg *config.Config,
	railSet railresolve.Source,
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	eligibilityChecker purchaseEligibilityChecker,
	fxProvider fx.Provider,
	priceProvider TokenPriceProvider,
	clocks ...clockwork.Clock,
) *SolanaPayService {
	return &SolanaPayService{
		db:                 db,
		cfg:                cfg,
		rails:              railSet,
		priceService:       priceService,
		productService:     productService,
		eligibilityChecker: eligibilityChecker,
		fxProvider:         fxProvider,
		priceProvider:      priceProvider,
		clock:              timeutil.FirstClock(clocks...),
	}
}

// SetMintDecimals arms the on-chain mint-decimals resolver (#817).
func (s *SolanaPayService) SetMintDecimals(mints MintDecimalsSource) {
	s.mints = mints
}

func (s *SolanaPayService) SetEligibilityChecker(checker purchaseEligibilityChecker) {
	s.eligibilityChecker = checker
}

func (s *SolanaPayService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *SolanaPayService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *SolanaPayService) Clock() clockwork.Clock {
	return s.clock
}

// GeneratePayment creates a new pending Solana payment and returns the Transfer Request URL.
// It first checks purchase eligibility to prevent duplicate purchases.
func (s *SolanaPayService) GeneratePayment(ctx context.Context, userID string, priceID uuid.UUID, tokenSymbol string, sessionID *uuid.UUID) (*PayResult, error) {
	if sessionID == nil || *sessionID == uuid.Nil {
		return nil, fmt.Errorf("checkout session id is required for solana payments")
	}
	tokenSymbol = strings.ToUpper(strings.TrimSpace(tokenSymbol))
	if tokenSymbol == "" {
		return nil, fmt.Errorf("token symbol is required")
	}

	// Check purchase eligibility BEFORE generating the payment URL
	if s.eligibilityChecker != nil {
		eligibility, err := s.eligibilityChecker.CheckPurchaseEligibility(ctx, userID, priceID)
		if err != nil {
			return nil, fmt.Errorf("failed to check purchase eligibility: %w", err)
		}

		switch eligibility.Status {
		case eligibilityBlocked:
			return nil, fmt.Errorf("purchase blocked: %s", eligibility.Reason)
		case eligibilityUpgrade, eligibilityDowngrade:
			// Solana doesn't support subscription upgrades/downgrades
			return nil, fmt.Errorf("solana does not support subscription tier changes; please cancel existing subscription first")
		case eligibilityAllowed:
			// Continue with payment generation
		}
	}

	// Validate price
	price, err := s.priceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if !price.IsPurchasable() {
		return nil, fmt.Errorf("price is not active")
	}

	// Validate product
	if s.productService != nil {
		product, err := s.productService.GetByID(ctx, price.ProductID)
		if err != nil {
			return nil, fmt.Errorf("product not found: %w", err)
		}
		if !product.IsPurchasable() {
			return nil, fmt.Errorf("product is not active")
		}
	}

	// Validate Solana config
	solanaProc, err := RequireSolanaRailConfig(ctx, s.rails)
	if err != nil {
		return nil, err
	}
	if solanaProc.Solana == nil {
		return nil, fmt.Errorf("solana rail is not configured")
	}
	tokenCfg, ok := solanaProc.Solana.Tokens[tokenSymbol]
	if !ok {
		return nil, fmt.Errorf("invalid or unsupported token: %s", tokenSymbol)
	}

	// Decimals come from the MINT on-chain, never from config (#817).
	decimals, err := RequireMintDecimals(ctx, s.mints, tokenCfg.Mint)
	if err != nil {
		return nil, err
	}

	// Calculate token amount from fiat price with FX conversion if needed
	quote, err := CalculateTokenQuote(ctx, tokenSymbol, tokenCfg.Mint, decimals, moneyutil.Micros(price.Amount), price.Currency, s.fxProvider, s.priceProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate token quote: %w", err)
	}
	tokenUnits := quote.Units
	if tokenUnits == 0 {
		return nil, fmt.Errorf("calculated token amount is zero")
	}

	// Generate reference for Solana Pay
	reference, err := solanarpc.GenerateReference()
	if err != nil {
		return nil, fmt.Errorf("failed to generate reference: %w", err)
	}

	recipient, err := ResolveRecipientWallet(ctx, s.db, s.cfg)
	if err != nil {
		return nil, err
	}

	// Get token mint
	tokenMint := tokenCfg.Mint

	now := s.now()
	expiresAt := now.Add(pendingPaymentTTL)

	if _, err := NewPayLedger(s.db).Register(ctx, ReferencePurchase, *sessionID, reference, expiresAt, now); err != nil {
		return nil, fmt.Errorf("failed to register solana pay reference: %w", err)
	}

	// Build Solana Pay Transfer Request URL. The memo field (#713) stamps the
	// checkout session id on the wallet-built tx: per the Solana Pay spec the
	// wallet includes it as an SPL Memo instruction BEFORE the transfer.
	// Discovery hint, never money truth.
	url := s.buildTransferRequestURL(ctx, recipient, tokenUnits, decimals, tokenMint, tokenSymbol, reference, solanarpc.PurchaseMemo(*sessionID))

	return &PayResult{
		URL:            url,
		Reference:      reference,
		Amount:         price.Amount,
		Currency:       price.Currency,
		TokenAmount:    FormatBaseUnits(tokenUnits, decimals),
		TokenUnits:     tokenUnits,
		TokenMint:      tokenMint,
		Recipient:      recipient,
		TokenPriceUSD:  quote.TokenPriceUSD,
		FXRate:         quote.FXRate,
		FXCurrency:     quote.FXCurrency,
		QuotedAt:       quote.QuotedAt,
		QuoteExpiresAt: expiresAt,
		Token:          tokenSymbol,
		ExpiresAt:      expiresAt,
	}, nil
}

// buildTransferRequestURL constructs the solana: URL per the Solana Pay spec.
// `decimals` is the caller's already-resolved ON-CHAIN mint precision (#817) —
// re-reading it here from a map without an ok-check turned an unknown symbol
// into decimals=0, i.e. the raw base-unit count on the wire (a 10^d overcharge).
func (s *SolanaPayService) buildTransferRequestURL(ctx context.Context, recipient string, amount uint64, decimals int, tokenMint, tokenSymbol, reference, memo string) string {
	// Base URL: solana:<recipient>
	baseURL := fmt.Sprintf("solana:%s", recipient)

	// Add query params
	params := fmt.Sprintf("?amount=%s", FormatBaseUnits(amount, decimals))

	// Add spl-token param if not native SOL
	if tokenMint != "" && tokenSymbol != "SOL" {
		params += fmt.Sprintf("&spl-token=%s", tokenMint)
	}

	// Add reference for payment detection
	params += fmt.Sprintf("&reference=%s", reference)

	// #713 self-recognition memo (SPL Memo instruction, placed by the wallet
	// before the transfer per spec).
	if memo != "" {
		params += fmt.Sprintf("&memo=%s", url.QueryEscape(memo))
	}

	// Add label
	label := "Purchase"
	if s.db != nil {
		if cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx); err == nil {
			if name := strings.TrimSpace(cfg.Profile.DisplayName); name != "" {
				label = name + " Purchase"
			}
		}
	}
	params += fmt.Sprintf("&label=%s", url.QueryEscape(label))

	return baseURL + params
}

// RegisterReference gives a transaction-request checkout attempt its one
// reference and puts it under the poller's watch. Idempotent per attempt.
func (s *SolanaPayService) RegisterReference(ctx context.Context, kind ReferenceKind, sessionID uuid.UUID, reference string, quoteExpiresAt time.Time) (gen.OpenrailsSolanaPayReference, error) {
	return NewPayLedger(s.db).Register(ctx, kind, sessionID, strings.TrimSpace(reference), quoteExpiresAt, s.now())
}
