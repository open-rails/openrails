package payment

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/writeposture"
)

const (
	defaultOrderAttemptTTL = 15 * time.Minute
	// solanaFinalizeWait bounds how long a page confirm waits for a transfer
	// to finalize (~13 s normally) before answering "processing".
	solanaFinalizeWait = 60 * time.Second
)

// IdempotencyLease is how long a silent purchase request keeps its claim.
// The owner renews it every quarter lease while it works, so only a dead
// owner's claim lapses, never a slow provider call's (xs-007 row 39).
// IdempotencyTTL is how long a completed request replays its response.
const (
	IdempotencyLease = 30 * time.Second
	IdempotencyTTL   = 24 * time.Hour
)

// orderRailTargets is the multi-PSP resolution capability the session
// service REQUIRES of its executor (or#893, #704, #848): resolve a wire
// selector — a PSP key or a bare rail kind — to the concrete armed account, and
// say when a key is declared-but-archived rather than unknown.
//
// It is REQUIRED, not optional, because every session must land on a real PSP:
// order_attempts.psp_id is NOT NULL, and a session nobody can attribute
// would be invisible to a PSP-scoped prune and would collide with a sibling
// account's reference under the nil-uuid lane 0063 deleted. The methods are
// unexported so only this package can satisfy it — test fakes implement it
// (see stubRailTargets) instead of the session path branching on the
// executor's concrete type.
type orderRailTargets interface {
	resolveRailTarget(ctx context.Context, selector string) (railTarget, error)
	pspKeyArchived(ctx context.Context, key string) bool
	railSource() railresolve.Source
}

type solanaPaymentService interface {
	// RegisterReference gives a transaction-request attempt its one reference.
	RegisterReference(ctx context.Context, kind solanamodule.ReferenceKind, sessionID uuid.UUID, reference string, quoteExpiresAt time.Time) (gen.BillingSolanaPayReference, error)
}

type solanaTransactionService interface {
	BuildPaymentTransactionFromQuote(ctx context.Context, req *solanamodule.PaymentTransactionBuildRequest) (*solanamodule.TransactionBuildResponse, error)
	ObserveTransfer(ctx context.Context, req solana.ObserveTransferRequest) (*solana.TransferObservation, error)
	BlockHeight(ctx context.Context) (uint64, error)
	ReferenceHasOurTransfer(ctx context.Context, reference, recipient, mint string, sessionID uuid.UUID) (bool, error)
}

type OrderAttemptService struct {
	db                       *db.DB
	repo                     *OrderAttemptRepo
	priceService             *catalog.PriceService
	productService           *catalog.ProductService
	paymentMethodService     *paymentmethods.PaymentMethodService
	purchaseService          orderRailTargets
	solanaPayService         solanaPaymentService
	solanaTransactionService solanaTransactionService
	fxProvider               fx.Provider
	priceProvider            solanamodule.TokenPriceProvider
	// solanaMints reads SPL mint decimals from the chain (#817). Late-bound —
	// it needs the per-merchant RPC resolver. nil = solana quotes fail closed.
	solanaMints solanamodule.MintDecimalsSource
	// solanaMintInfo reads a mint's transfer hook at quote time.
	solanaMintInfo solanamodule.MintInfoSource
	config         *config.Config
	rails          railresolve.Source
	clock          clockwork.Clock
	// cardFailures is the durable card-testing ledger (SEC-30).
	cardFailures *abuse.FailureLedger

	// Recurring Solana (#261/#262), injected via SetSolanaRecurring at the
	// composition root. nil -> solana+subscription purchase returns 503.
	solanaPrepareSubscribe *recurring.PrepareSubscribeService
	solanaEnroll           *recurring.EnrollService

	// pspDisarmed reports a PSP whose credentials failed posture verification.
	pspDisarmed func(uuid.UUID) bool

	// orders settles the orders order attempts pay (#1168).
	orders *orders.Service

	// paymentOrigin is the payment page's origin, "" without one: its
	// pages are return URLs too.
	paymentOrigin func() string
}

// SetPaymentOrigin wires the payment page's origin, set once it is served.
func (s *OrderAttemptService) SetPaymentOrigin(origin func() string) {
	s.paymentOrigin = origin
}

// SetPSPPosture wires the verdict purchase consults before offering a PSP.
func (s *OrderAttemptService) SetPSPPosture(disarmed func(uuid.UUID) bool) {
	s.pspDisarmed = disarmed
}

// SetSolanaRecurring wires the recurring-Solana subscribe (prepare) + enroll
// (confirm) services. Done via a setter so the constructor signature (called by
// embedded hosts) stays stable.
func (s *OrderAttemptService) SetSolanaRecurring(prepare *recurring.PrepareSubscribeService, enroll *recurring.EnrollService) {
	s.solanaPrepareSubscribe = prepare
	s.solanaEnroll = enroll
}

func NewOrderAttemptService(
	db *db.DB,
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	paymentMethodService *paymentmethods.PaymentMethodService,
	purchaseService orderRailTargets,
	solanaPayService solanaPaymentService,
	solanaTransactionService solanaTransactionService,
	fxProvider fx.Provider,
	priceProvider solanamodule.TokenPriceProvider,
	cfg *config.Config,
	rails railresolve.Source,
	clocks ...clockwork.Clock,
) *OrderAttemptService {
	return &OrderAttemptService{
		db:                       db,
		repo:                     NewOrderAttemptRepo(db),
		priceService:             priceService,
		productService:           productService,
		paymentMethodService:     paymentMethodService,
		purchaseService:          purchaseService,
		solanaPayService:         solanaPayService,
		solanaTransactionService: solanaTransactionService,
		fxProvider:               fxProvider,
		priceProvider:            priceProvider,
		config:                   cfg,
		rails:                    rails,
		clock:                    timeutil.FirstClock(clocks...),
	}
}

// SetSolanaMintInfo arms the quote-time mint reader.
func (s *OrderAttemptService) SetSolanaMintInfo(chain solanamodule.MintInfoSource) {
	s.solanaMintInfo = chain
}

// SetSolanaMintDecimals arms the on-chain mint-decimals resolver (#817).
func (s *OrderAttemptService) SetSolanaMintDecimals(mints solanamodule.MintDecimalsSource) {
	s.solanaMints = mints
}

func (s *OrderAttemptService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *OrderAttemptService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *OrderAttemptService) Clock() clockwork.Clock {
	return s.clock
}

func (s *OrderAttemptService) requireProviderWrites(ctx context.Context) error {
	if s == nil || s.config == nil {
		return fmt.Errorf("%w: provider writes are disabled", ErrOrderAttemptValidation)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if (writeposture.View{Config: s.config, DB: s.db}).Posture(ctx, mid.UUID()).ReadOnly() {
		return fmt.Errorf("%w: provider writes are disabled", ErrOrderAttemptValidation)
	}
	return nil
}

// priceHasSolanaRecurring reports whether a price carries a published Solana
// recurring plan config (the keys PlanService.ToRailConfig writes).
func priceHasSolanaRecurring(price *models.Price) bool {
	if price == nil {
		return false
	}
	cfg := price.PSPLinkForRail(models.RailSolana)
	if cfg == nil {
		return false
	}
	return strings.TrimSpace(cfg["plan_id"]) != "" &&
		strings.TrimSpace(cfg["amount_base_units"]) != "" &&
		strings.TrimSpace(cfg["period_hours"]) != "" &&
		strings.TrimSpace(cfg["mint_symbol"]) != ""
}

type solanaPlanTerms struct {
	planID     uint64
	mintSymbol string
	amount     uint64
	period     uint64
	createdAt  int64
}

func parseSolanaPlanTerms(cfg map[string]string) (solanaPlanTerms, error) {
	var t solanaPlanTerms
	if cfg == nil {
		return t, fmt.Errorf("%w: price has no solana plan config", ErrOrderAttemptValidation)
	}
	var err error
	if t.planID, err = strconv.ParseUint(cfg["plan_id"], 10, 64); err != nil {
		return t, fmt.Errorf("%w: invalid solana plan_id", ErrOrderAttemptValidation)
	}
	if t.amount, err = strconv.ParseUint(cfg["amount_base_units"], 10, 64); err != nil || t.amount == 0 {
		return t, fmt.Errorf("%w: invalid solana amount_base_units", ErrOrderAttemptValidation)
	}
	if t.period, err = strconv.ParseUint(cfg["period_hours"], 10, 64); err != nil || t.period == 0 {
		return t, fmt.Errorf("%w: invalid solana period_hours", ErrOrderAttemptValidation)
	}
	t.createdAt, _ = strconv.ParseInt(cfg["created_at"], 10, 64)
	t.mintSymbol = strings.TrimSpace(cfg["mint_symbol"])
	return t, nil
}

// solanaSignerAvailable refuses (503) while the Solana rail's signer is
// unavailable or its identity change awaits approval (#1101).
func (s *OrderAttemptService) solanaSignerAvailable(ctx context.Context) error {
	if _, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails); err != nil && errors.Is(err, vault.ErrUnavailable) {
		return err
	}
	return nil
}

// getAPIBaseURL returns the API base URL for building Solana Pay URLs.
// Uses config.PublicBillingBaseURL which should be set to the full base URL where billing routes are mounted.
//
// Standalone: "https://api.mysite.com" → routes at /v1/*
// Embedded:   "https://api.mysite.com/billing" → routes at /billing/v1/*
//
// Generated URLs follow the pattern: PublicBillingBaseURL + "/v1/order-attempts/:id/solana-pay"
func (s *OrderAttemptService) getAPIBaseURL() string {
	if s.config == nil {
		return ""
	}
	apiURL := strings.TrimSpace(s.config.PublicBillingBaseURL)
	if apiURL == "" {
		return ""
	}
	// Ensure it doesn't end with a slash (we add the version path later)
	return strings.TrimSuffix(apiURL, "/")
}

func getStringField(fields map[string]any, key string) string {
	if fields == nil {
		return ""
	}
	raw, ok := fields[key]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	default:
		return ""
	}
}

func getUint64Field(fields map[string]any, key string) uint64 {
	if fields == nil {
		return 0
	}
	raw, ok := fields[key]
	if !ok || raw == nil {
		return 0
	}
	switch val := raw.(type) {
	case uint64:
		return val
	case uint32:
		return uint64(val)
	case uint:
		return uint64(val)
	case int64:
		if val < 0 {
			return 0
		}
		return uint64(val)
	case int:
		if val < 0 {
			return 0
		}
		return uint64(val)
	case string:
		if parsed, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

func setSolanaQuoteState(railState map[string]any, tokenAmount uint64, tokenPriceUSD, fxRate float64, fxCurrency string, quotedAt, quoteExpiresAt time.Time) error {
	if railState == nil {
		return fmt.Errorf("%w: rail_state unavailable", ErrOrderAttemptValidation)
	}
	if tokenAmount == 0 {
		return fmt.Errorf("%w: token_amount must be greater than 0", ErrOrderAttemptValidation)
	}
	if quotedAt.IsZero() {
		return fmt.Errorf("%w: quote timestamp missing", ErrOrderAttemptValidation)
	}
	if quoteExpiresAt.IsZero() {
		return fmt.Errorf("%w: quote expiry missing", ErrOrderAttemptValidation)
	}

	// MONEY-3: the token AMOUNT is written as a decimal string. A JSONB
	// round-trip decodes numbers as float64, so a base-unit count past 2^53
	// would come back wrong. The two RATES below are rates, not amounts.
	railState["token_amount"] = strconv.FormatUint(tokenAmount, 10)
	railState["token_price_usd"] = tokenPriceUSD
	railState["fx_rate"] = fxRate
	railState["fx_currency"] = strings.TrimSpace(fxCurrency)
	railState["quoted_at"] = quotedAt.UTC().Format(time.RFC3339)
	railState["quote_expires_at"] = quoteExpiresAt.UTC().Format(time.RFC3339)

	return nil
}

// BuildSolanaPayTransaction builds a Solana transaction for the given order attempt and wallet account.
// This implements the POST endpoint of the Solana Pay Transaction Request spec.
func (s *OrderAttemptService) BuildSolanaPayTransaction(ctx context.Context, sessionID uuid.UUID, account string) (*solanamodule.PayTransactionResponse, error) {
	if err := s.requireProviderWrites(ctx); err != nil {
		return nil, err
	}
	if s.repo == nil {
		return nil, ErrOrderAttemptNotFound
	}
	if s.solanaTransactionService == nil {
		return nil, fmt.Errorf("%w: solana transaction service unavailable", ErrOrderAttemptValidation)
	}
	account = strings.TrimSpace(account)
	if account == "" {
		return nil, fmt.Errorf("%w: account is required", ErrOrderAttemptValidation)
	}

	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, ErrOrderAttemptNotFound
		}
		return nil, err
	}
	// Validate it's a Solana session
	if session.Rail != models.RailSolana {
		return nil, ErrOrderAttemptNotSolana
	}

	// Check if expired: a one-off quote is offered only until it expires,
	// though its reference keeps crediting a transfer already sent.
	if session.ExpiresAt != nil && s.now().After(*session.ExpiresAt) {
		return nil, ErrOrderAttemptExpired
	}
	if quote, ok := solanaQuoteExpiresAt(session); ok && s.now().After(quote) {
		return nil, ErrOrderAttemptExpired
	}

	// Check if already completed
	if session.Status == models.OrderAttemptStatusSucceeded ||
		session.Status == models.OrderAttemptStatusCanceled {
		return nil, ErrOrderAttemptAlreadyCompleted
	}

	// A subscribe builds the atomic [init?, subscribe, first pull] transaction
	// for the POSTed account, with the reference injected and put under the
	// poller's watch.
	if solanaSubscribes(session) {
		resp, err := s.buildSolanaSubscribeTransaction(ctx, session, account)
		if err != nil {
			return nil, err
		}
		if _, err := s.registerSolanaReference(ctx, solanamodule.ReferenceSubscribe, session); err != nil {
			return nil, err
		}
		return resp, nil
	}

	// Get token symbol from rail state
	tokenSymbol := getStringField(session.RailState, "token_symbol")
	if tokenSymbol == "" {
		return nil, fmt.Errorf("%w: token_symbol missing from session", ErrOrderAttemptValidation)
	}

	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	if existingPayer := strings.TrimSpace(getStringField(session.RailState, "payer")); existingPayer != "" && existingPayer != account {
		return nil, fmt.Errorf("%w: solana order attempt is already bound to a different payer", ErrOrderAttemptConflict)
	}

	// Generate and persist the payment binding before returning a transaction to the wallet.
	if session.Reference == nil || *session.Reference == "" {
		reference, err := solana.GenerateReference()
		if err != nil {
			return nil, fmt.Errorf("failed to generate reference: %w", err)
		}
		session.Reference = &reference
	}
	session.RailState["payer"] = account
	if err := s.repo.BindSolanaTransactionRequest(ctx, session, account, s.now()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOrderAttemptConflict, err)
	}
	buildReq, err := solanaBuildRequestFromSession(session, account, tokenSymbol)
	if err != nil {
		return nil, err
	}
	ref, err := s.registerSolanaReference(ctx, solanamodule.ReferencePurchase, session)
	if err != nil {
		return nil, err
	}
	tx, err := s.offerSolanaTransaction(ctx, ref, buildReq)
	if err != nil {
		return nil, err
	}
	return &solanamodule.PayTransactionResponse{
		TransactionBase64: tx,
		Message:           solanamodule.PaymentInstructions(buildReq.Amount, buildReq.Currency, tokenSymbol),
	}, nil
}

// offerSolanaTransaction returns the one transaction a order attempt may
// be paid with. While its blockhash can still land, every request gets the
// same transaction back, so a wallet cannot be handed a second payable one.
// A new one is built only once the chain can no longer include the previous
// one and nothing has landed on the reference.
func (s *OrderAttemptService) offerSolanaTransaction(ctx context.Context, ref gen.BillingSolanaPayReference, req *solanamodule.PaymentTransactionBuildRequest) (string, error) {
	if ref.Status != solanamodule.ReferencePending {
		return "", ErrOrderAttemptAlreadyCompleted
	}
	if ref.BuiltTransaction != nil {
		height, err := s.solanaTransactionService.BlockHeight(ctx)
		if err != nil {
			return "", err
		}
		if height <= uint64(max(*ref.BuiltValidHeight, 0)) {
			return *ref.BuiltTransaction, nil
		}
		landed, err := s.solanaTransactionService.ReferenceHasOurTransfer(ctx, ref.Reference, req.Recipient, req.TokenMint, req.SessionID)
		if errors.Is(err, solanamodule.ErrReferenceUndetermined) {
			return "", fmt.Errorf("%w: a transfer for this checkout may be in flight; try again shortly", ErrOrderAttemptConflict)
		}
		if err != nil {
			return "", err
		}
		if landed {
			return "", fmt.Errorf("%w: a transfer for this checkout has already landed and is being confirmed", ErrOrderAttemptConflict)
		}
	}
	built, err := s.solanaTransactionService.BuildPaymentTransactionFromQuote(ctx, req)
	if err != nil {
		return "", err
	}
	ledger := solanamodule.NewPayLedger(s.db)
	stored, err := ledger.StoreBuilt(ctx, ref.Reference, built.TransactionBase64, built.LastValidBlockHeight, ref.BuiltValidHeight, s.now())
	if err != nil {
		return "", err
	}
	if stored {
		return built.TransactionBase64, nil
	}
	current, err := ledger.Get(ctx, ref.Reference)
	if err != nil {
		return "", err
	}
	if current.Status != solanamodule.ReferencePending || current.BuiltTransaction == nil {
		return "", ErrOrderAttemptAlreadyCompleted
	}
	return *current.BuiltTransaction, nil
}

// registerSolanaReference puts the session's bound reference under watch.
func (s *OrderAttemptService) registerSolanaReference(ctx context.Context, kind solanamodule.ReferenceKind, session *models.OrderAttempt) (gen.BillingSolanaPayReference, error) {
	if s.solanaPayService == nil || session.Reference == nil {
		return gen.BillingSolanaPayReference{}, fmt.Errorf("%w: solana pay service unavailable", ErrOrderAttemptValidation)
	}
	expires := s.now().Add(defaultOrderAttemptTTL)
	if quote, ok := solanaQuoteExpiresAt(session); ok {
		expires = quote
	} else if session.ExpiresAt != nil {
		expires = *session.ExpiresAt
	}
	ref, err := s.solanaPayService.RegisterReference(ctx, kind, session.ID, *session.Reference, expires)
	if err != nil {
		return ref, fmt.Errorf("%w: register solana pay reference: %v", ErrOrderAttemptConflict, err)
	}
	return ref, nil
}

func solanaBuildRequestFromSession(session *models.OrderAttempt, account, tokenSymbol string) (*solanamodule.PaymentTransactionBuildRequest, error) {
	if session == nil {
		return nil, fmt.Errorf("%w: session is required", ErrOrderAttemptValidation)
	}
	tokenAmount := getUint64Field(session.RailState, "token_amount")
	if tokenAmount == 0 {
		return nil, fmt.Errorf("%w: token_amount missing from session", ErrOrderAttemptValidation)
	}
	tokenMint := getStringField(session.RailState, "token_mint")
	if tokenMint == "" {
		return nil, fmt.Errorf("%w: token_mint missing from session", ErrOrderAttemptValidation)
	}
	if !strings.EqualFold(tokenSymbol, "SOL") && solanamodule.IsNativeSOLMint(tokenMint) {
		return nil, fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrOrderAttemptValidation)
	}
	recipient := getStringField(session.RailState, "recipient")
	if recipient == "" {
		return nil, fmt.Errorf("%w: recipient missing from session", ErrOrderAttemptValidation)
	}

	return &solanamodule.PaymentTransactionBuildRequest{
		UserID:      session.CustomerID.String(),
		TokenSymbol: tokenSymbol,
		UserWallet:  account,
		Reference:   session.Reference,
		TokenAmount: tokenAmount,
		TokenMint:   tokenMint,
		Recipient:   recipient,
		Amount:      *session.Amount,
		Currency:    *session.Currency,
		SessionID:   session.ID, // #713 memo local-id
	}, nil
}

// buildSolanaSubscribeTransaction builds the RECURRING subscribe tx for a Solana
// Pay (transaction-request) subscribe session. The decision to be here is purely
// price-driven (mode==subscription, resolved from the price's recurring config) —
// there is no client-supplied one-off override. It binds the Solana Pay reference
// + payer to the POSTed account, calls PrepareSubscribeService for that wallet
// with the reference injected, and tracks the init→subscribe step on
// RailState so a first-timer's second POST (after init lands) returns the
// subscribe tx. The poller mirrors the confirmed subscribe via ConfirmEnrollment.
func (s *OrderAttemptService) buildSolanaSubscribeTransaction(ctx context.Context, session *models.OrderAttempt, account string) (*solanamodule.PayTransactionResponse, error) {
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return nil, fmt.Errorf("%w: solana recurring billing is not configured", ErrOrderAttemptValidation)
	}
	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	// The subscribe Solana Pay session is bound to the first wallet that POSTs its
	// account (it becomes the subscriber + fee payer). A second, different wallet is
	// rejected so the QR can't be hijacked mid-flow.
	if existingPayer := strings.TrimSpace(getStringField(session.RailState, "payer")); existingPayer != "" && existingPayer != account {
		return nil, fmt.Errorf("%w: solana order attempt is already bound to a different payer", ErrOrderAttemptConflict)
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		reference, err := solana.GenerateReference()
		if err != nil {
			return nil, fmt.Errorf("failed to generate reference: %w", err)
		}
		session.Reference = &reference
	}
	reference := strings.TrimSpace(*session.Reference)
	session.RailState["payer"] = account
	session.RailState["subscriber_wallet"] = account
	if err := s.repo.BindSolanaTransactionRequest(ctx, session, account, s.now()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOrderAttemptConflict, err)
	}

	terms := solanaPlanTerms{
		planID:     getUint64Field(session.RailState, "plan_id"),
		mintSymbol: getStringField(session.RailState, "mint_symbol"),
		amount:     getUint64Field(session.RailState, "amount_base_units"),
		period:     getUint64Field(session.RailState, "period_hours"),
	}
	if v := strings.TrimSpace(getStringField(session.RailState, "plan_created_at")); v != "" {
		terms.createdAt, _ = strconv.ParseInt(v, 10, 64)
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}

	res, err := s.solanaPrepareSubscribe.Prepare(ctx, recurring.PrepareSubscribeInput{
		MerchantID:       tid,
		SubscriberWallet: account,
		PlanID:           terms.planID,
		MintSymbol:       terms.mintSymbol,
		AmountBaseUnits:  terms.amount,
		PeriodHours:      terms.period,
		PlanCreatedAt:    terms.createdAt,
		Reference:        reference,
	})
	if err != nil {
		return nil, err
	}
	if len(res.Transactions) == 0 {
		return nil, fmt.Errorf("%w: no subscribe transaction produced", ErrOrderAttemptConflict)
	}

	// Record the PDA for the poller's confirm. The bundle is one-step (init
	// folded in for a first-time subscriber), so one signature settles it.
	session.RailState["subscription_pda"] = res.SubscriptionPDA
	session.UpdatedAt = s.now()
	if err := s.repo.Update(ctx, session); err != nil {
		return nil, err
	}

	return &solanamodule.PayTransactionResponse{
		TransactionBase64: res.Transactions[0],
		Message:           "Sign to start your subscription",
	}, nil
}

// pollerConfirmContext prepares the context for a confirm driven by the Solana
// Pay poller rather than an HTTP request. or#893/#704: such a context carries no
// request-scoped PSP, yet every provider-bound row the confirm writes
// (membership, payment, mirror) must be attributable to the account the session
// was minted against — exactly what the HTTP confirm path pins — or the repos
// refuse the write (db.ErrNoPSPInContext) and the landed payment is never
// enrolled.
func (s *OrderAttemptService) pollerConfirmContext(ctx context.Context, session *models.OrderAttempt) context.Context {
	if session == nil {
		return ctx
	}
	return db.WithPSPID(ctx, session.PspID)
}

// ConfirmSolanaSubscribeSession enrolls an order's Solana subscription from a
// confirmed transaction the reference poller found carrying the attempt's
// reference. Anyone can put a public reference in a transaction, so the
// signature only names a candidate: enrollment verifies it is the attempt's
// first payment. A transaction not yet readable stays pending; one that is not
// the payment is refused and the poller moves on to the reference's other
// signatures. A settled attempt confirms again as a no-op.
func (s *OrderAttemptService) ConfirmSolanaSubscribeSession(ctx context.Context, attemptID uuid.UUID, signature string) error {
	attempt, err := s.repo.GetByID(ctx, attemptID)
	if err != nil {
		return ErrOrderAttemptNotFound
	}
	if attempt.Status == models.OrderAttemptStatusSucceeded {
		return nil
	}
	if attempt.OrderID == nil || !solanaSubscribes(attempt) {
		return fmt.Errorf("%w: attempt %s subscribes to nothing", ErrOrderAttemptValidation, attemptID)
	}
	if strings.TrimSpace(signature) == "" {
		return fmt.Errorf("%w: signature is required", ErrOrderAttemptValidation)
	}
	return s.enrollSolanaOrder(ctx, attempt, signature)
}

// solanaSettleDeadline: a Solana one-off attempt stays open while its
// reference still credits a transfer, so it never reads expired while it can
// still be paid.
func solanaSettleDeadline(quoteExpiresAt time.Time) time.Time {
	return quoteExpiresAt.Add(solana.LateSettlementWindow)
}

// solanaQuoteExpiresAt is when the attempt's Solana quote stops being offered.
func solanaQuoteExpiresAt(session *models.OrderAttempt) (time.Time, bool) {
	raw := getStringField(session.RailState, "quote_expires_at")
	if raw == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, raw)
	return at, err == nil
}
