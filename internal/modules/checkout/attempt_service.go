package checkout

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
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
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/writeposture"
)

const (
	defaultCheckoutAttemptTTL = 15 * time.Minute
	// solanaFinalizeWait bounds how long a page confirm waits for a transfer
	// to finalize (~13 s normally) before answering "processing".
	solanaFinalizeWait = 60 * time.Second
)

// IdempotencyLease is how long a silent checkout request keeps its claim.
// The owner renews it every quarter lease while it works, so only a dead
// owner's claim lapses, never a slow provider call's (xs-007 row 39).
// IdempotencyTTL is how long a completed request replays its response.
const (
	IdempotencyLease = 30 * time.Second
	IdempotencyTTL   = 24 * time.Hour
)

// checkoutRailTargets is the multi-PSP resolution capability the session
// service REQUIRES of its executor (or#893, #704, #848): resolve a wire
// selector — a PSP key or a bare rail kind — to the concrete armed account, and
// say when a key is declared-but-archived rather than unknown.
//
// It is REQUIRED, not optional, because every session must land on a real PSP:
// checkout_attempts.psp_id is NOT NULL, and a session nobody can attribute
// would be invisible to a PSP-scoped prune and would collide with a sibling
// account's reference under the nil-uuid lane 0063 deleted. The methods are
// unexported so only this package can satisfy it — test fakes implement it
// (see stubRailTargets) instead of the session path branching on the
// executor's concrete type.
type checkoutRailTargets interface {
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

type CheckoutAttemptService struct {
	db                       *db.DB
	repo                     *CheckoutAttemptRepo
	priceService             *catalog.PriceService
	productService           *catalog.ProductService
	paymentMethodService     *paymentmethods.PaymentMethodService
	checkoutService          checkoutRailTargets
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
	// composition root. nil -> solana+subscription checkout returns 503.
	solanaPrepareSubscribe *recurring.PrepareSubscribeService
	solanaEnroll           *recurring.EnrollService

	// pspDisarmed reports a PSP whose credentials failed posture verification.
	pspDisarmed func(uuid.UUID) bool

	// orders settles the orders checkout attempts pay (#1168).
	orders *orders.Service
}

// SetPSPPosture wires the verdict checkout consults before offering a PSP.
func (s *CheckoutAttemptService) SetPSPPosture(disarmed func(uuid.UUID) bool) {
	s.pspDisarmed = disarmed
}

// SetSolanaRecurring wires the recurring-Solana subscribe (prepare) + enroll
// (confirm) services. Done via a setter so the constructor signature (called by
// embedded hosts) stays stable.
func (s *CheckoutAttemptService) SetSolanaRecurring(prepare *recurring.PrepareSubscribeService, enroll *recurring.EnrollService) {
	s.solanaPrepareSubscribe = prepare
	s.solanaEnroll = enroll
}

func NewCheckoutAttemptService(
	db *db.DB,
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	paymentMethodService *paymentmethods.PaymentMethodService,
	checkoutService checkoutRailTargets,
	solanaPayService solanaPaymentService,
	solanaTransactionService solanaTransactionService,
	fxProvider fx.Provider,
	priceProvider solanamodule.TokenPriceProvider,
	cfg *config.Config,
	rails railresolve.Source,
	clocks ...clockwork.Clock,
) *CheckoutAttemptService {
	return &CheckoutAttemptService{
		db:                       db,
		repo:                     NewCheckoutAttemptRepo(db),
		priceService:             priceService,
		productService:           productService,
		paymentMethodService:     paymentMethodService,
		checkoutService:          checkoutService,
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
func (s *CheckoutAttemptService) SetSolanaMintInfo(chain solanamodule.MintInfoSource) {
	s.solanaMintInfo = chain
}

// SetSolanaMintDecimals arms the on-chain mint-decimals resolver (#817).
func (s *CheckoutAttemptService) SetSolanaMintDecimals(mints solanamodule.MintDecimalsSource) {
	s.solanaMints = mints
}

func (s *CheckoutAttemptService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *CheckoutAttemptService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *CheckoutAttemptService) Clock() clockwork.Clock {
	return s.clock
}

func (s *CheckoutAttemptService) requireProviderWrites(ctx context.Context) error {
	if s == nil || s.config == nil {
		return fmt.Errorf("%w: provider writes are disabled", ErrCheckoutAttemptValidation)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if (writeposture.View{Config: s.config, DB: s.db}).Posture(ctx, mid.UUID()).ReadOnly() {
		return fmt.Errorf("%w: provider writes are disabled", ErrCheckoutAttemptValidation)
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
		return t, fmt.Errorf("%w: price has no solana plan config", ErrCheckoutAttemptValidation)
	}
	var err error
	if t.planID, err = strconv.ParseUint(cfg["plan_id"], 10, 64); err != nil {
		return t, fmt.Errorf("%w: invalid solana plan_id", ErrCheckoutAttemptValidation)
	}
	if t.amount, err = strconv.ParseUint(cfg["amount_base_units"], 10, 64); err != nil || t.amount == 0 {
		return t, fmt.Errorf("%w: invalid solana amount_base_units", ErrCheckoutAttemptValidation)
	}
	if t.period, err = strconv.ParseUint(cfg["period_hours"], 10, 64); err != nil || t.period == 0 {
		return t, fmt.Errorf("%w: invalid solana period_hours", ErrCheckoutAttemptValidation)
	}
	t.createdAt, _ = strconv.ParseInt(cfg["created_at"], 10, 64)
	t.mintSymbol = strings.TrimSpace(cfg["mint_symbol"])
	return t, nil
}

// solanaSignerAvailable refuses (503) while the Solana rail's signer is
// unavailable or its identity change awaits approval (#1101).
func (s *CheckoutAttemptService) solanaSignerAvailable(ctx context.Context) error {
	if _, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails); err != nil && errors.Is(err, vault.ErrUnavailable) {
		return err
	}
	return nil
}

func (s *CheckoutAttemptService) isTerminal(status models.CheckoutAttemptStatus) bool {
	switch status {
	case models.CheckoutAttemptStatusSucceeded,
		models.CheckoutAttemptStatusFailed,
		models.CheckoutAttemptStatusExpired,
		models.CheckoutAttemptStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *CheckoutAttemptService) isExpired(session *models.CheckoutAttempt) bool {
	if session.ExpiresAt == nil || session.ExpiresAt.IsZero() {
		return false
	}
	return session.ExpiresAt.Before(s.now())
}

// getAPIBaseURL returns the API base URL for building Solana Pay URLs.
// Uses config.PublicBillingBaseURL which should be set to the full base URL where billing routes are mounted.
//
// Standalone: "https://api.mysite.com" → routes at /v1/*
// Embedded:   "https://api.mysite.com/billing" → routes at /billing/v1/*
//
// Generated URLs follow the pattern: PublicBillingBaseURL + "/v1/checkout-attempts/:id/solana-pay"
func (s *CheckoutAttemptService) getAPIBaseURL() string {
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

func (s *CheckoutAttemptService) MarkSucceeded(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutAttemptNotFound
	}
	if s.isTerminal(session.Status) {
		if session.Status == models.CheckoutAttemptStatusSucceeded {
			return nil
		}
		if session.Status == models.CheckoutAttemptStatusExpired && session.Rail == models.RailSolana && paymentID != uuid.Nil && strings.TrimSpace(transactionID) != "" {
			// A wallet may broadcast before expiry but the app may confirm after expiry.
			// The caller has already verified the signature against the session-bound quote.
		} else {
			return ErrCheckoutAttemptConflict
		}
	}
	if s.isExpired(session) && session.Rail != models.RailSolana {
		_ = s.MarkExpired(ctx, session.ID, "checkout attempt expired")
		return ErrCheckoutAttemptExpired
	}

	session.Status = models.CheckoutAttemptStatusSucceeded
	session.UpdatedAt = s.now()
	if paymentID != uuid.Nil {
		session.PaymentID = &paymentID
	}
	if strings.TrimSpace(transactionID) != "" {
		session.TransactionID = normalize.OptionalString(transactionID)
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutAttemptService) MarkFailed(ctx context.Context, sessionID uuid.UUID, reason, code string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutAttemptNotFound
	}
	if s.isTerminal(session.Status) {
		switch session.Status {
		case models.CheckoutAttemptStatusFailed,
			models.CheckoutAttemptStatusSucceeded,
			models.CheckoutAttemptStatusExpired,
			models.CheckoutAttemptStatusCanceled:
			return nil
		default:
			return ErrCheckoutAttemptConflict
		}
	}

	session.Status = models.CheckoutAttemptStatusFailed
	session.UpdatedAt = s.now()
	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	if msg := strings.TrimSpace(reason); msg != "" {
		session.RailState["message"] = msg
		session.RailState["failure_reason"] = msg
	}
	if strings.TrimSpace(code) != "" {
		session.RailState["failure_code"] = strings.TrimSpace(code)
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutAttemptService) MarkExpired(ctx context.Context, sessionID uuid.UUID, message string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutAttemptNotFound
	}
	if s.isTerminal(session.Status) {
		return nil
	}

	if session.RailState["capture"] != nil || session.RailState["kind"] == "stripe_engine_setup" {
		owner, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		_, err = s.db.Gen(ctx).ExpireCheckoutAttemptByID(ctx, gen.ExpireCheckoutAttemptByIDParams{ID: sessionID, MerchantID: owner.UUID(), Now: s.now()})
		return err
	}

	session.Status = models.CheckoutAttemptStatusExpired
	session.UpdatedAt = s.now()
	if msg := strings.TrimSpace(message); msg != "" {
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState["message"] = msg
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutAttemptService) MarkSucceededWithSubscription(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string, subscriptionID uuid.UUID) error {
	return s.markSucceededWithSubscription(ctx, sessionID, paymentID, transactionID, subscriptionID, false)
}

// markSucceededWithSubscription is MarkSucceededWithSubscription with the
// settled flag. settled=true says the caller has already verified the payment
// on-chain (a verified first payment, a mirrored cancel/tier-change): the
// session window is quote validity, never a refusal of money that actually
// moved (xs-007 row 35), so a session whose window elapsed while the poller was
// not looking — or that a client poll already flipped to expired — still
// becomes succeeded. Refusing it would leave a paid subscriber whose checkout
// says otherwise and a reference the poller can never finalize. failed and
// canceled are not clock outcomes and stay refused.
func (s *CheckoutAttemptService) markSucceededWithSubscription(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string, subscriptionID uuid.UUID, settled bool) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutAttemptNotFound
	}
	proceed, err := succeedTransition(session.Status, settled)
	if err != nil || !proceed {
		return err
	}
	if !settled && s.isExpired(session) {
		_ = s.MarkExpired(ctx, session.ID, "checkout attempt expired")
		return ErrCheckoutAttemptExpired
	}

	session.Status = models.CheckoutAttemptStatusSucceeded
	session.UpdatedAt = s.now()
	if paymentID != uuid.Nil {
		session.PaymentID = &paymentID
	}
	if subscriptionID != uuid.Nil {
		session.SubscriptionID = &subscriptionID
	}
	if strings.TrimSpace(transactionID) != "" {
		session.TransactionID = normalize.OptionalString(transactionID)
	}

	return s.repo.Update(ctx, session)
}

// succeedTransition decides whether a session in status may move to succeeded.
// (false, nil) is the idempotent no-op for an already-succeeded session.
func succeedTransition(status models.CheckoutAttemptStatus, settled bool) (bool, error) {
	switch status {
	case models.CheckoutAttemptStatusSucceeded:
		return false, nil
	case models.CheckoutAttemptStatusExpired:
		if settled {
			return true, nil
		}
		return false, ErrCheckoutAttemptConflict
	case models.CheckoutAttemptStatusFailed, models.CheckoutAttemptStatusCanceled:
		return false, ErrCheckoutAttemptConflict
	default:
		return true, nil
	}
}

func (s *CheckoutAttemptService) FindOpenByUserPriceRail(ctx context.Context, userID string, priceID uuid.UUID, rail models.Rail) (*models.CheckoutAttempt, error) {
	if s.repo == nil {
		return nil, ErrCheckoutAttemptNotFound
	}
	session, err := s.repo.GetLatestOpenByUserPriceRail(ctx, userID, priceID, rail, s.now())
	if err != nil {
		if db.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

func (s *CheckoutAttemptService) FindOpenCCBillReservation(ctx context.Context, reservationID string, userID string, priceID uuid.UUID) (*models.CheckoutAttempt, error) {
	if s.repo == nil {
		return nil, ErrCheckoutAttemptNotFound
	}
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return nil, sql.ErrNoRows
	}
	sessionID, err := billing.ParseCheckoutAttemptID(reservationID)
	if err != nil || sessionID.IsZero() {
		return nil, sql.ErrNoRows
	}
	session, err := s.repo.GetByID(ctx, sessionID.UUID())
	if err != nil {
		return nil, err
	}
	if session.CustomerID.String() != userID || session.Rail != models.RailCCBill || session.AcceptedPriceID == nil || *session.AcceptedPriceID != priceID {
		return nil, ErrCheckoutAttemptConflict
	}
	if s.isTerminal(session.Status) || s.isExpired(session) {
		return nil, ErrCheckoutAttemptExpired
	}
	return session, nil
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
		return fmt.Errorf("%w: rail_state unavailable", ErrCheckoutAttemptValidation)
	}
	if tokenAmount == 0 {
		return fmt.Errorf("%w: token_amount must be greater than 0", ErrCheckoutAttemptValidation)
	}
	if quotedAt.IsZero() {
		return fmt.Errorf("%w: quote timestamp missing", ErrCheckoutAttemptValidation)
	}
	if quoteExpiresAt.IsZero() {
		return fmt.Errorf("%w: quote expiry missing", ErrCheckoutAttemptValidation)
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

// BuildSolanaPayTransaction builds a Solana transaction for the given checkout attempt and wallet account.
// This implements the POST endpoint of the Solana Pay Transaction Request spec.
func (s *CheckoutAttemptService) BuildSolanaPayTransaction(ctx context.Context, sessionID uuid.UUID, account string) (*solanamodule.PayTransactionResponse, error) {
	if err := s.requireProviderWrites(ctx); err != nil {
		return nil, err
	}
	if s.repo == nil {
		return nil, ErrCheckoutAttemptNotFound
	}
	if s.solanaTransactionService == nil {
		return nil, fmt.Errorf("%w: solana transaction service unavailable", ErrCheckoutAttemptValidation)
	}
	account = strings.TrimSpace(account)
	if account == "" {
		return nil, fmt.Errorf("%w: account is required", ErrCheckoutAttemptValidation)
	}

	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, ErrCheckoutAttemptNotFound
		}
		return nil, err
	}
	// Validate it's a Solana session
	if session.Rail != models.RailSolana {
		return nil, ErrCheckoutAttemptNotSolana
	}

	// Check if expired: a one-off quote is offered only until it expires,
	// though its reference keeps crediting a transfer already sent.
	if session.ExpiresAt != nil && s.now().After(*session.ExpiresAt) {
		return nil, ErrCheckoutAttemptExpired
	}
	if quote, ok := solanaQuoteExpiresAt(session); ok && s.now().After(quote) {
		return nil, ErrCheckoutAttemptExpired
	}

	// Check if already completed
	if session.Status == models.CheckoutAttemptStatusSucceeded ||
		session.Status == models.CheckoutAttemptStatusCanceled {
		return nil, ErrCheckoutAttemptAlreadyCompleted
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
		return nil, fmt.Errorf("%w: token_symbol missing from session", ErrCheckoutAttemptValidation)
	}

	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	if existingPayer := strings.TrimSpace(getStringField(session.RailState, "payer")); existingPayer != "" && existingPayer != account {
		return nil, fmt.Errorf("%w: solana checkout attempt is already bound to a different payer", ErrCheckoutAttemptConflict)
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
		return nil, fmt.Errorf("%w: %v", ErrCheckoutAttemptConflict, err)
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

// offerSolanaTransaction returns the one transaction a checkout attempt may
// be paid with. While its blockhash can still land, every request gets the
// same transaction back, so a wallet cannot be handed a second payable one.
// A new one is built only once the chain can no longer include the previous
// one and nothing has landed on the reference.
func (s *CheckoutAttemptService) offerSolanaTransaction(ctx context.Context, ref gen.BillingSolanaPayReference, req *solanamodule.PaymentTransactionBuildRequest) (string, error) {
	if ref.Status != solanamodule.ReferencePending {
		return "", ErrCheckoutAttemptAlreadyCompleted
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
			return "", fmt.Errorf("%w: a transfer for this checkout may be in flight; try again shortly", ErrCheckoutAttemptConflict)
		}
		if err != nil {
			return "", err
		}
		if landed {
			return "", fmt.Errorf("%w: a transfer for this checkout has already landed and is being confirmed", ErrCheckoutAttemptConflict)
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
		return "", ErrCheckoutAttemptAlreadyCompleted
	}
	return *current.BuiltTransaction, nil
}

// registerSolanaReference puts the session's bound reference under watch.
func (s *CheckoutAttemptService) registerSolanaReference(ctx context.Context, kind solanamodule.ReferenceKind, session *models.CheckoutAttempt) (gen.BillingSolanaPayReference, error) {
	if s.solanaPayService == nil || session.Reference == nil {
		return gen.BillingSolanaPayReference{}, fmt.Errorf("%w: solana pay service unavailable", ErrCheckoutAttemptValidation)
	}
	expires := s.now().Add(defaultCheckoutAttemptTTL)
	if quote, ok := solanaQuoteExpiresAt(session); ok {
		expires = quote
	} else if session.ExpiresAt != nil {
		expires = *session.ExpiresAt
	}
	ref, err := s.solanaPayService.RegisterReference(ctx, kind, session.ID, *session.Reference, expires)
	if err != nil {
		return ref, fmt.Errorf("%w: register solana pay reference: %v", ErrCheckoutAttemptConflict, err)
	}
	return ref, nil
}

func solanaBuildRequestFromSession(session *models.CheckoutAttempt, account, tokenSymbol string) (*solanamodule.PaymentTransactionBuildRequest, error) {
	if session == nil {
		return nil, fmt.Errorf("%w: session is required", ErrCheckoutAttemptValidation)
	}
	tokenAmount := getUint64Field(session.RailState, "token_amount")
	if tokenAmount == 0 {
		return nil, fmt.Errorf("%w: token_amount missing from session", ErrCheckoutAttemptValidation)
	}
	tokenMint := getStringField(session.RailState, "token_mint")
	if tokenMint == "" {
		return nil, fmt.Errorf("%w: token_mint missing from session", ErrCheckoutAttemptValidation)
	}
	if !strings.EqualFold(tokenSymbol, "SOL") && solanamodule.IsNativeSOLMint(tokenMint) {
		return nil, fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrCheckoutAttemptValidation)
	}
	recipient := getStringField(session.RailState, "recipient")
	if recipient == "" {
		return nil, fmt.Errorf("%w: recipient missing from session", ErrCheckoutAttemptValidation)
	}

	return &solanamodule.PaymentTransactionBuildRequest{
		UserID:      session.CustomerID.String(),
		PriceID:     uuidOrNil(session.AcceptedPriceID),
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
func (s *CheckoutAttemptService) buildSolanaSubscribeTransaction(ctx context.Context, session *models.CheckoutAttempt, account string) (*solanamodule.PayTransactionResponse, error) {
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return nil, fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutAttemptValidation)
	}
	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	// The subscribe Solana Pay session is bound to the first wallet that POSTs its
	// account (it becomes the subscriber + fee payer). A second, different wallet is
	// rejected so the QR can't be hijacked mid-flow.
	if existingPayer := strings.TrimSpace(getStringField(session.RailState, "payer")); existingPayer != "" && existingPayer != account {
		return nil, fmt.Errorf("%w: solana checkout attempt is already bound to a different payer", ErrCheckoutAttemptConflict)
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
		return nil, fmt.Errorf("%w: %v", ErrCheckoutAttemptConflict, err)
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
		return nil, fmt.Errorf("%w: no subscribe transaction produced", ErrCheckoutAttemptConflict)
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
func (s *CheckoutAttemptService) pollerConfirmContext(ctx context.Context, session *models.CheckoutAttempt) context.Context {
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
func (s *CheckoutAttemptService) ConfirmSolanaSubscribeSession(ctx context.Context, attemptID uuid.UUID, signature string) error {
	attempt, err := s.repo.GetByID(ctx, attemptID)
	if err != nil {
		return ErrCheckoutAttemptNotFound
	}
	if attempt.Status == models.CheckoutAttemptStatusSucceeded {
		return nil
	}
	if attempt.OrderID == nil || !solanaSubscribes(attempt) {
		return fmt.Errorf("%w: attempt %s subscribes to nothing", ErrCheckoutAttemptValidation, attemptID)
	}
	if strings.TrimSpace(signature) == "" {
		return fmt.Errorf("%w: signature is required", ErrCheckoutAttemptValidation)
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
func solanaQuoteExpiresAt(session *models.CheckoutAttempt) (time.Time, bool) {
	raw := getStringField(session.RailState, "quote_expires_at")
	if raw == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, raw)
	return at, err == nil
}
