package checkout

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/tenant"
)

const (
	checkoutSessionIdempotencyOp = "checkout_session_create"
	defaultCheckoutSessionTTL    = 15 * time.Minute
	redirectCheckoutSessionTTL   = 24 * time.Hour
)

type IdempotencyStatus string

const (
	IdempotencyStatusPending IdempotencyStatus = "pending"
	IdempotencyStatusSuccess IdempotencyStatus = "success"
	IdempotencyStatusFailed  IdempotencyStatus = "failed"
)

type IdempotencyRecord struct {
	Status    IdempotencyStatus
	Result    json.RawMessage
	Error     string
	CreatedAt time.Time
}

type checkoutSessionIdempotencyResult struct {
	RequestFingerprint string                   `json:"request_fingerprint"`
	Response           *CheckoutSessionResponse `json:"response"`
}

type sessionIdempotencyStore interface {
	Begin(ctx context.Context, operation, key string) (*IdempotencyRecord, bool, error)
	Fail(ctx context.Context, operation, key string, operationErr error) error
	Complete(ctx context.Context, operation, key string, result any) error
}

type checkoutSessionExecutor interface {
	Checkout(ctx context.Context, req *CheckoutRequest, user *UserIdentity) (*CheckoutResponse, error)
	RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error)
	// CheckSubscriptionConflict is the shared duplicate-billing guard (issue
	// #269): blocks a second non-terminal subscription in the same exact price or
	// tier-group. The Solana subscribe path runs it before preparing any
	// transaction.
	CheckSubscriptionConflict(ctx context.Context, userID string, price *models.Price, product *models.Product) (*SubscriptionConflict, error)
}

type solanaPaymentService interface {
	GeneratePayment(ctx context.Context, userID string, priceID uuid.UUID, tokenSymbol string, sessionID *uuid.UUID) (*solanamodule.PayResult, error)
	ConsumeAndRemovePending(ctx context.Context, reference, transactionID string) error
	// RegisterPendingReference seeds a transaction-request reference into the
	// poller's pending set (the transfer-request flow does this via GeneratePayment).
	RegisterPendingReference(ctx context.Context, reference string) error
}

type solanaTransactionService interface {
	BuildPaymentTransactionFromQuote(ctx context.Context, req *solanamodule.PaymentTransactionBuildRequest) (*solanamodule.TransactionBuildResponse, error)
	VerifyTransactionWithContent(ctx context.Context, signature string, expectedAmount uint64, expectedRecipient string, expectedTokenMint string, expectedPayer string, expectedReference *string, processedNotAfter *time.Time) error
}

type CheckoutSessionService struct {
	db                       *db.DB
	repo                     *repo.CheckoutSessionRepo
	priceService             *catalog.PriceService
	productService           *catalog.ProductService
	paymentMethodService     *vault.PaymentMethodService
	idempotencyService       sessionIdempotencyStore
	checkoutService          checkoutSessionExecutor
	solanaPayService         solanaPaymentService
	solanaTransactionService solanaTransactionService
	fxProvider               fx.Provider
	priceProvider            solanamodule.TokenPriceProvider
	config                   *config.Config
	clock                    clockwork.Clock

	// Recurring Solana (#261/#262), injected via SetSolanaRecurring at the
	// composition root. nil -> solana+subscription checkout returns 503.
	solanaPrepareSubscribe *recurring.PrepareSubscribeService
	solanaEnroll           *recurring.EnrollService

	// Solana subscription-lifecycle services that extend the Solana Pay
	// transaction-request flow to CANCEL + TIER-CHANGE (new checkout modes). nil
	// -> a solana_cancel / solana_tier_change session returns 503. Wired via
	// SetSolanaLifecycle at the composition root.
	solanaPrepareCancel     solanaLifecyclePrepareCancel
	solanaPrepareTierChange solanaLifecyclePrepareTierChange
	solanaConfirmCancel     solanaLifecycleConfirmCancel
	solanaConfirmTierChange solanaLifecycleConfirmTierChange
	subscriptionReader      subscriptionReader
	solanaSubscriptionRows  solanaSubscriptionRowReader
}

// solanaLifecyclePrepareCancel builds the unsigned cancel_subscription tx with an
// optional Solana Pay reference (satisfied by *recurring.PrepareCancelService).
type solanaLifecyclePrepareCancel interface {
	PrepareWithReference(ctx context.Context, subscriptionID uuid.UUID, reference string) (*recurring.PrepareCancelResult, error)
}

// solanaLifecyclePrepareTierChange builds the atomic tier-change tx (satisfied by
// *recurring.PrepareTierChangeService).
type solanaLifecyclePrepareTierChange interface {
	Prepare(ctx context.Context, in recurring.PrepareTierChangeInput) (*recurring.PrepareTierChangeResult, error)
}

// solanaLifecycleConfirmCancel mirrors a confirmed on-chain cancel into the DB
// (satisfied by *recurring.ConfirmCancelService).
type solanaLifecycleConfirmCancel interface {
	Confirm(ctx context.Context, subscriptionID uuid.UUID, signature string) error
}

// solanaLifecycleConfirmTierChange mirrors a confirmed on-chain tier change into
// the DB (satisfied by *recurring.ConfirmTierChangeService).
type solanaLifecycleConfirmTierChange interface {
	Confirm(ctx context.Context, in recurring.ConfirmTierChangeInput) (*recurring.ConfirmTierChangeResult, error)
}

// subscriptionReader loads a lifecycle subscription for ownership checks
// (satisfied by *subscriptions.SubscriptionService).
type subscriptionReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*models.Subscription, error)
}

// solanaSubscriptionRowReader loads the stored on-chain identifiers for a
// subscription (satisfied by *repo.SolanaSubscriptionRepo).
type solanaSubscriptionRowReader interface {
	GetBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.SolanaSubscription, error)
}

// SetSolanaRecurring wires the recurring-Solana subscribe (prepare) + enroll
// (confirm) services. Done via a setter so the constructor signature (called by
// embedded hosts) stays stable.
func (s *CheckoutSessionService) SetSolanaRecurring(prepare *recurring.PrepareSubscribeService, enroll *recurring.EnrollService) {
	s.solanaPrepareSubscribe = prepare
	s.solanaEnroll = enroll
}

// SetSolanaLifecycle wires the cancel + tier-change services that back the
// solana_cancel / solana_tier_change checkout modes. Done via a setter so the
// constructor signature stays stable for embedded hosts. Passing a nil concrete
// service leaves that capability unconfigured (the matching mode returns 503).
func (s *CheckoutSessionService) SetSolanaLifecycle(
	prepareCancel *recurring.PrepareCancelService,
	prepareTierChange *recurring.PrepareTierChangeService,
	confirmCancel *recurring.ConfirmCancelService,
	confirmTierChange *recurring.ConfirmTierChangeService,
	subs subscriptionReader,
	rows solanaSubscriptionRowReader,
) {
	// Assign through nil-guarding so a nil concrete pointer stays a nil interface
	// (an interface holding a typed-nil pointer is non-nil and would bypass the
	// "is this configured?" checks).
	if prepareCancel != nil {
		s.solanaPrepareCancel = prepareCancel
	}
	if prepareTierChange != nil {
		s.solanaPrepareTierChange = prepareTierChange
	}
	if confirmCancel != nil {
		s.solanaConfirmCancel = confirmCancel
	}
	if confirmTierChange != nil {
		s.solanaConfirmTierChange = confirmTierChange
	}
	if subs != nil {
		s.subscriptionReader = subs
	}
	if rows != nil {
		s.solanaSubscriptionRows = rows
	}
}

// SetSolanaLifecycleForTest wires the lifecycle dependencies from interface
// values so unit tests can inject fakes without constructing the real services.
func (s *CheckoutSessionService) SetSolanaLifecycleForTest(
	prepareCancel solanaLifecyclePrepareCancel,
	prepareTierChange solanaLifecyclePrepareTierChange,
	confirmCancel solanaLifecycleConfirmCancel,
	confirmTierChange solanaLifecycleConfirmTierChange,
	subs subscriptionReader,
	rows solanaSubscriptionRowReader,
) {
	s.solanaPrepareCancel = prepareCancel
	s.solanaPrepareTierChange = prepareTierChange
	s.solanaConfirmCancel = confirmCancel
	s.solanaConfirmTierChange = confirmTierChange
	s.subscriptionReader = subs
	s.solanaSubscriptionRows = rows
}

func NewCheckoutSessionService(
	db *db.DB,
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	paymentMethodService *vault.PaymentMethodService,
	idempotencyService sessionIdempotencyStore,
	checkoutService checkoutSessionExecutor,
	solanaPayService solanaPaymentService,
	solanaTransactionService solanaTransactionService,
	fxProvider fx.Provider,
	priceProvider solanamodule.TokenPriceProvider,
	cfg *config.Config,
	clocks ...clockwork.Clock,
) *CheckoutSessionService {
	return &CheckoutSessionService{
		db:                       db,
		repo:                     repo.NewCheckoutSessionRepo(db),
		priceService:             priceService,
		productService:           productService,
		paymentMethodService:     paymentMethodService,
		idempotencyService:       idempotencyService,
		checkoutService:          checkoutService,
		solanaPayService:         solanaPayService,
		solanaTransactionService: solanaTransactionService,
		fxProvider:               fxProvider,
		priceProvider:            priceProvider,
		config:                   cfg,
		clock:                    timeutil.FirstClock(clocks...),
	}
}

func (s *CheckoutSessionService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *CheckoutSessionService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *CheckoutSessionService) Clock() clockwork.Clock {
	return s.clock
}

func (s *CheckoutSessionService) CreateSession(ctx context.Context, req *CheckoutSessionCreateRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return nil, fmt.Errorf("%w: user is required", ErrCheckoutSessionValidation)
	}
	if req == nil {
		return nil, fmt.Errorf("%w: request is required", ErrCheckoutSessionValidation)
	}

	fingerprint := checkoutSessionRequestFingerprint(req)
	req.IdempotencyKey = scopeIdempotencyKey(user.ID, req.IdempotencyKey)

	claimed := false
	if s.idempotencyService != nil && strings.TrimSpace(req.IdempotencyKey) != "" {
		rec, exists, err := s.idempotencyService.Begin(ctx, checkoutSessionIdempotencyOp, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if exists {
			switch rec.Status {
			case IdempotencyStatusSuccess:
				cached, err := decodeCheckoutSessionIdempotencyResult(rec.Result, fingerprint)
				if err != nil {
					return nil, err
				}
				return cached, nil
			case IdempotencyStatusPending:
				return nil, ErrCheckoutSessionPending
			case IdempotencyStatusFailed:
				return nil, fmt.Errorf("%w: previous request failed: %s", ErrCheckoutSessionConflict, rec.Error)
			}
		}
		claimed = true
	}

	resp, err := s.createSessionWithValidation(ctx, req, user)
	if err != nil {
		if claimed && s.idempotencyService != nil && strings.TrimSpace(req.IdempotencyKey) != "" {
			_ = s.idempotencyService.Fail(ctx, checkoutSessionIdempotencyOp, req.IdempotencyKey, err)
		}
		return nil, err
	}

	if claimed && s.idempotencyService != nil && strings.TrimSpace(req.IdempotencyKey) != "" {
		payload, _ := json.Marshal(checkoutSessionIdempotencyResult{RequestFingerprint: fingerprint, Response: resp})
		_ = s.idempotencyService.Complete(ctx, checkoutSessionIdempotencyOp, req.IdempotencyKey, payload)
	}

	return resp, nil
}

func checkoutSessionRequestFingerprint(req *CheckoutSessionCreateRequest) string {
	if req == nil {
		return ""
	}
	payload, _ := json.Marshal(struct {
		PriceID  string
		Mode     string
		Payment  CheckoutSessionPaymentRequest
		Metadata map[string]string
	}{
		PriceID:  strings.TrimSpace(req.PriceID),
		Mode:     strings.TrimSpace(req.Mode),
		Payment:  req.Payment,
		Metadata: normalizeMetadata(req.Metadata),
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func decodeCheckoutSessionIdempotencyResult(payload json.RawMessage, fingerprint string) (*CheckoutSessionResponse, error) {
	var cached checkoutSessionIdempotencyResult
	if err := json.Unmarshal(payload, &cached); err == nil && cached.Response != nil {
		if cached.RequestFingerprint != "" && fingerprint != "" && cached.RequestFingerprint != fingerprint {
			return nil, fmt.Errorf("%w: idempotency key reused with different checkout session parameters", ErrCheckoutSessionConflict)
		}
		return cached.Response, nil
	}
	return nil, fmt.Errorf("failed to decode cached checkout session response")
}

func (s *CheckoutSessionService) createSessionWithValidation(ctx context.Context, req *CheckoutSessionCreateRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	// Solana subscription-lifecycle modes (cancel / tier-change) are owner-gated
	// actions on an EXISTING subscription, not a price purchase — route them to
	// their dedicated builder before the price-first validation below.
	switch models.CheckoutSessionMode(strings.TrimSpace(req.Mode)) {
	case models.CheckoutSessionModeSolanaCancel, models.CheckoutSessionModeSolanaTierChange:
		return s.createSolanaLifecycleSession(ctx, req, user)
	}

	if strings.TrimSpace(req.PriceID) == "" {
		return nil, fmt.Errorf("%w: price_id is required", ErrCheckoutSessionValidation)
	}
	if strings.TrimSpace(req.Payment.Processor) == "" {
		return nil, fmt.Errorf("%w: payment.processor is required", ErrCheckoutSessionValidation)
	}

	priceID, err := api.ParsePriceID(req.PriceID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid price_id", ErrCheckoutSessionValidation)
	}
	price, err := s.priceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("%w: price not found", ErrCheckoutSessionValidation)
	}
	if !price.IsPurchasable() {
		return nil, fmt.Errorf("%w: price is not active", ErrCheckoutSessionValidation)
	}
	product, err := s.productService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("%w: product not found", ErrCheckoutSessionValidation)
	}
	if !product.IsPurchasable() {
		return nil, fmt.Errorf("%w: product is not active", ErrCheckoutSessionValidation)
	}

	processor := strings.ToLower(strings.TrimSpace(req.Payment.Processor))
	mode, err := s.resolveMode(req.Mode, processor, price)
	if err != nil {
		return nil, err
	}

	if err := s.validatePayment(ctx, processor, &req.Payment, user); err != nil {
		return nil, fmt.Errorf("error validating payment: %w", err)
	}

	now := s.now()
	ttl := defaultCheckoutSessionTTL
	if processor == "ccbill" || processor == "stripe" {
		ttl = redirectCheckoutSessionTTL
	}
	subjectID, err := s.ensureSubjectID(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant subject for checkout session: %w", err)
	}

	session := &models.CheckoutSession{
		ID:              uuidutil.NewV7(),
		TenantSubjectID: subjectID,
		PriceID:         price.ID,
		Mode:            mode,
		Processor:       models.Processor(processor),
		Status:          models.CheckoutSessionStatusCreated,
		Amount:          price.Amount,
		Currency:        price.Currency,
		ExpiresAt:       timePtr(now.Add(ttl)),
		Metadata:        normalizeMetadata(req.Metadata),
		ProcessorFields: s.buildProcessorFields(processor, &req.Payment),
		ProcessorState:  map[string]any{},
		CreatedAt:       now,
		UpdatedAt:       now,
		LastFour:        &req.Payment.LastFour,
		CardType:        &req.Payment.CardType,
		ExpiryDate:      &req.Payment.ExpiryDate,
	}

	if strings.TrimSpace(req.IdempotencyKey) != "" {
		session.IdempotencyKey = normalize.OptionalString(req.IdempotencyKey)
	}

	if err := s.repo.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create checkout session: %w", err)
	}

	if err := s.initializeSession(ctx, session, &req.Payment, user); err != nil {
		_ = s.MarkFailed(ctx, session.ID, err.Error(), "")
		return nil, err
	}

	session.UpdatedAt = s.now()
	if err := s.repo.Update(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to update checkout session: %w", err)
	}

	return s.sessionToResponse(session), nil
}

func (s *CheckoutSessionService) GetSession(ctx context.Context, sessionID uuid.UUID, user *UserIdentity) (*CheckoutSessionResponse, error) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return nil, ErrCheckoutSessionNotFound
	}
	if user == nil || strings.TrimSpace(user.ID) == "" || session.TenantSubjectID.String() != user.ID {
		return nil, ErrCheckoutSessionForbidden
	}

	if s.isExpired(session) && !s.isTerminal(session.Status) {
		session.Status = models.CheckoutSessionStatusExpired
		session.UpdatedAt = s.now()
		if updateErr := s.repo.Update(ctx, session); updateErr != nil {
			return nil, fmt.Errorf("failed to update expired session: %w", updateErr)
		}
	}

	return s.sessionToResponse(session), nil
}

func (s *CheckoutSessionService) ConfirmSession(ctx context.Context, sessionID uuid.UUID, req *CheckoutSessionConfirmRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return nil, ErrCheckoutSessionNotFound
	}
	if user == nil || strings.TrimSpace(user.ID) == "" || session.TenantSubjectID.String() != user.ID {
		return nil, ErrCheckoutSessionForbidden
	}

	if s.isTerminal(session.Status) {
		if session.Status == models.CheckoutSessionStatusSucceeded {
			transactionID := ""
			if session.TransactionID != nil {
				transactionID = strings.TrimSpace(*session.TransactionID)
			}
			_ = s.finalizeSolanaTransferReference(ctx, session, transactionID)
			return s.sessionToResponse(session), nil
		}
		if session.Status != models.CheckoutSessionStatusExpired {
			return nil, ErrCheckoutSessionConflict
		}
	}
	processor := strings.ToLower(strings.TrimSpace(req.Payment.Processor))
	if processor == "" {
		return nil, fmt.Errorf("%w: payment.processor is required", ErrCheckoutSessionValidation)
	}
	if processor != strings.ToLower(string(session.Processor)) {
		return nil, fmt.Errorf("%w: processor mismatch", ErrCheckoutSessionValidation)
	}
	if s.isExpired(session) && processor != string(models.ProcessorSolana) {
		if !s.isTerminal(session.Status) {
			_ = s.MarkExpired(ctx, session.ID, "checkout session expired")
		}
		return nil, ErrCheckoutSessionExpired
	}

	switch processor {
	case "solana":
		if session.Mode == models.CheckoutSessionModeSubscription {
			return s.confirmSolanaSubscriptionSession(ctx, session, req, user)
		}
		return s.confirmSolanaSession(ctx, session, req, user)
	default:
		return nil, fmt.Errorf("%w: confirmation not implemented for processor %s", ErrCheckoutSessionConflict, processor)
	}
}

func (s *CheckoutSessionService) resolveMode(mode string, processor string, price *models.Price) (models.CheckoutSessionMode, error) {
	if processor == "" {
		return "", fmt.Errorf("%w: processor is required", ErrCheckoutSessionValidation)
	}

	trimmedMode := strings.TrimSpace(mode)
	if processor == "solana" {
		hasRecurring := priceHasSolanaRecurring(price)
		if trimmedMode == string(models.CheckoutSessionModeSubscription) {
			if !hasRecurring {
				return "", fmt.Errorf("%w: price is not configured for Solana recurring billing", ErrCheckoutSessionValidation)
			}
			return models.CheckoutSessionModeSubscription, nil
		}
		if trimmedMode == string(models.CheckoutSessionModeOneOff) {
			return models.CheckoutSessionModeOneOff, nil
		}
		// Mode unspecified: a price with a published Solana recurring plan defaults
		// to subscription (Solana = subscription by default); otherwise one-off.
		if hasRecurring {
			return models.CheckoutSessionModeSubscription, nil
		}
		return models.CheckoutSessionModeOneOff, nil
	}

	expected := models.CheckoutSessionModeOneOff
	if price.BillingCycleDays != nil {
		expected = models.CheckoutSessionModeSubscription
	}
	if trimmedMode == "" {
		return expected, nil
	}
	if trimmedMode != string(expected) {
		return "", fmt.Errorf("%w: mode does not match price configuration", ErrCheckoutSessionValidation)
	}
	return models.CheckoutSessionMode(trimmedMode), nil
}

// validatePayment dispatches checkout-input validation to the per-processor
// validator that owns that processor's required-input contract. Keeping each
// processor's rules in its own method (rather than a shared switch body) is what
// keeps the validation contract from drifting out of sync with what the
// processor's executor actually consumes — the drift that previously made Stripe
// demand billing fields its hosted-checkout path never reads.
// ensureSubjectID resolves-or-creates the payable tenant-subject id for a raw
// user/subject string (#364): deriving via identity.TenantSubjectIDFromString
// yields uuid.Nil for legacy (non-UUID) subjects and mis-attributes the session.
func (s *CheckoutSessionService) ensureSubjectID(ctx context.Context, userID string) (uuid.UUID, error) {
	return repo.EnsureTenantSubjectID(ctx, s.db.Qx(ctx), uuid.Nil, userID)
}

func (s *CheckoutSessionService) validatePayment(ctx context.Context, processor string, payment *CheckoutSessionPaymentRequest, user *UserIdentity) error {
	switch {
	case processors.IsNMIBacked(processor):
		return s.validateNMIInput(ctx, payment, user)
	case processor == "stripe":
		return s.validateStripeInput(payment)
	case processor == "solana":
		return s.validateSolanaInput(payment)
	case processor == "ccbill":
		return s.validateCCBillInput(payment)
	default:
		return fmt.Errorf("%w: unsupported processor", ErrCheckoutSessionValidation)
	}
}

// validateNMIInput requires exactly one of payment_token or payment_method_id;
// a saved method must belong to the caller. The NMI executor charges with the
// token or vaulted method, so both inputs are genuinely consumed downstream.
func (s *CheckoutSessionService) validateNMIInput(ctx context.Context, payment *CheckoutSessionPaymentRequest, user *UserIdentity) error {
	hasToken := strings.TrimSpace(payment.PaymentToken) != ""
	hasMethod := strings.TrimSpace(payment.PaymentMethodID) != ""
	if hasToken == hasMethod {
		return fmt.Errorf("%w: provide either payment_token or payment_method_id", ErrCheckoutSessionValidation)
	}
	if hasMethod {
		pmID, err := api.ParsePaymentMethodID(payment.PaymentMethodID)
		if err != nil {
			return fmt.Errorf("%w: invalid payment_method_id", ErrCheckoutSessionValidation)
		}
		if s.paymentMethodService == nil {
			return fmt.Errorf("%w: payment method service unavailable", ErrCheckoutSessionValidation)
		}
		if err := s.paymentMethodService.ValidateOwnership(ctx, pmID, user.ID); err != nil {
			return fmt.Errorf("%w: payment method not authorized", ErrCheckoutSessionValidation)
		}
	}
	return nil
}

// validateStripeInput validates inputs for Stripe hosted Checkout. Stripe's
// hosted page collects the customer's email and billing address itself, and
// createStripeCheckoutSession sends none of those fields, so they are NOT
// required here. Saved payment methods are not supported in the redirect flow.
func (s *CheckoutSessionService) validateStripeInput(payment *CheckoutSessionPaymentRequest) error {
	if strings.TrimSpace(payment.PaymentMethodID) != "" {
		return fmt.Errorf("%w: saved payment methods are not supported for stripe checkout", ErrCheckoutSessionValidation)
	}
	return nil
}

// validateSolanaInput requires a token symbol so the executor can resolve which
// SPL mint to charge.
func (s *CheckoutSessionService) validateSolanaInput(payment *CheckoutSessionPaymentRequest) error {
	if strings.TrimSpace(payment.TokenSymbol) == "" {
		return fmt.Errorf("%w: token_symbol is required", ErrCheckoutSessionValidation)
	}
	return nil
}

// validateCCBillInput requires billing fields: CCBill's FlexForm flow submits
// them with the charge, so they are genuinely consumed.
func (s *CheckoutSessionService) validateCCBillInput(payment *CheckoutSessionPaymentRequest) error {
	return requireBillingFields(payment)
}

func (s *CheckoutSessionService) initializeSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest, user *UserIdentity) error {
	if session == nil {
		return fmt.Errorf("%w: session is required", ErrCheckoutSessionValidation)
	}
	if payment == nil {
		return fmt.Errorf("%w: payment is required", ErrCheckoutSessionValidation)
	}

	processor := strings.ToLower(string(session.Processor))
	// Route to processor-specific initialization based on config type detection
	// This allows adding new NMI providers via config without code changes
	switch {
	case processor == "solana":
		return s.initializeSolanaSession(ctx, session, payment)
	case processors.IsNMIBacked(processor):
		return s.initializeCheckoutSession(ctx, session, payment, user)
	case processor == "ccbill" || processor == "stripe":
		return s.initializeCheckoutSession(ctx, session, payment, user)
	default:
		return fmt.Errorf("%w: unsupported processor", ErrCheckoutSessionValidation)
	}
}

func (s *CheckoutSessionService) initializeSolanaSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest) error {
	// Recurring Solana subscription (#261): distinct from the one-off Solana Pay
	// flow — the subscriber signs init_subscription_authority + subscribe in their
	// wallet, so we return UNSIGNED transactions to sign rather than a Pay URL.
	if session.Mode == models.CheckoutSessionModeSubscription {
		return s.initializeSolanaSubscriptionSession(ctx, session, payment)
	}

	solanaProc, err := solanamodule.RequireSolanaProcessorConfig(s.config)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCheckoutSessionValidation, err)
	}

	tokenSymbol := strings.ToUpper(strings.TrimSpace(payment.TokenSymbol))
	if tokenSymbol == "" {
		return fmt.Errorf("%w: token_symbol is required", ErrCheckoutSessionValidation)
	}

	flow := strings.TrimSpace(payment.Flow)
	if flow == "" {
		flow = "transfer_request"
	}

	tokenCfg, ok := solanaProc.Tokens[tokenSymbol]
	if !ok {
		return fmt.Errorf("%w: unsupported token", ErrCheckoutSessionValidation)
	}
	tokenMint := tokenCfg.Mint
	if !strings.EqualFold(tokenSymbol, "SOL") && solanamodule.IsNativeSOLMint(tokenMint) {
		return fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrCheckoutSessionValidation)
	}

	switch flow {
	case "transfer_request":
		if s.solanaPayService == nil {
			return fmt.Errorf("%w: solana pay service unavailable", ErrCheckoutSessionValidation)
		}
		result, err := s.solanaPayService.GeneratePayment(ctx, session.TenantSubjectID.String(), session.PriceID, tokenSymbol, &session.ID)
		if err != nil {
			return err
		}
		session.Status = models.CheckoutSessionStatusRequiresAction
		session.Reference = &result.Reference
		session.ExpiresAt = &result.ExpiresAt
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["transaction_url"] = result.URL
		session.ProcessorState["flow"] = flow
		session.ProcessorState["token_symbol"] = tokenSymbol
		tokenMintValue := strings.TrimSpace(result.TokenMint)
		if tokenMintValue == "" {
			tokenMintValue = tokenMint
		}
		recipient := strings.TrimSpace(result.Recipient)
		if recipient == "" {
			return fmt.Errorf("%w: recipient missing from payment quote", ErrCheckoutSessionValidation)
		}
		session.ProcessorState["token_mint"] = tokenMintValue
		session.ProcessorState["recipient"] = recipient
		if err := setSolanaQuoteState(session.ProcessorState, result.TokenUnits, result.TokenPriceUSD, result.FXRate, result.FXCurrency, result.QuotedAt, result.QuoteExpiresAt); err != nil {
			return err
		}
	case "transaction_request":
		// Transaction Request flow per Solana Pay spec:
		// - Wallet address is NOT required at session creation
		// - Transaction is built later when wallet calls POST /v1/checkout/:id/solana-pay
		// - Session just stores flow and token info, returns solana_pay_url for wallet
		if s.solanaTransactionService == nil {
			return fmt.Errorf("%w: solana transaction service unavailable", ErrCheckoutSessionValidation)
		}
		session.Status = models.CheckoutSessionStatusRequiresAction
		expiresAt := s.now().Add(defaultCheckoutSessionTTL)
		session.ExpiresAt = &expiresAt
		recipient := strings.TrimSpace(solanaProc.RecipientWallet)
		if recipient == "" {
			return fmt.Errorf("%w: recipient wallet not configured", ErrCheckoutSessionValidation)
		}
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["flow"] = flow
		session.ProcessorState["token_symbol"] = tokenSymbol
		session.ProcessorState["token_mint"] = tokenMint
		session.ProcessorState["recipient"] = recipient
		quote, err := solanamodule.CalculateTokenQuote(ctx, tokenSymbol, tokenCfg, session.Amount, session.Currency, s.fxProvider, s.priceProvider)
		if err != nil {
			return fmt.Errorf("%w: failed to calculate solana token quote: %v", ErrCheckoutSessionValidation, err)
		}
		if err := setSolanaQuoteState(session.ProcessorState, quote.Units, quote.TokenPriceUSD, quote.FXRate, quote.FXCurrency, quote.QuotedAt, expiresAt); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unsupported solana flow", ErrCheckoutSessionValidation)
	}

	return nil
}

// priceHasSolanaRecurring reports whether a price carries a published Solana
// recurring plan config (the keys PlanService.ToProcessorConfig writes).
func priceHasSolanaRecurring(price *models.Price) bool {
	if price == nil {
		return false
	}
	cfg := price.GetProcessorConfig(models.ProcessorSolana)
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
		return t, fmt.Errorf("%w: price has no solana plan config", ErrCheckoutSessionValidation)
	}
	var err error
	if t.planID, err = strconv.ParseUint(cfg["plan_id"], 10, 64); err != nil {
		return t, fmt.Errorf("%w: invalid solana plan_id", ErrCheckoutSessionValidation)
	}
	if t.amount, err = strconv.ParseUint(cfg["amount_base_units"], 10, 64); err != nil || t.amount == 0 {
		return t, fmt.Errorf("%w: invalid solana amount_base_units", ErrCheckoutSessionValidation)
	}
	if t.period, err = strconv.ParseUint(cfg["period_hours"], 10, 64); err != nil || t.period == 0 {
		return t, fmt.Errorf("%w: invalid solana period_hours", ErrCheckoutSessionValidation)
	}
	t.createdAt, _ = strconv.ParseInt(cfg["created_at"], 10, 64)
	t.mintSymbol = strings.TrimSpace(cfg["mint_symbol"])
	return t, nil
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// getStringSliceField reads a []string persisted in ProcessorState (JSONB decodes
// it back as []any of strings).
func getStringSliceField(fields map[string]any, key string) []string {
	if fields == nil {
		return nil
	}
	raw, ok := fields[key]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

// initializeSolanaSubscriptionSession prepares the UNSIGNED init/subscribe
// transaction(s) the subscriber's wallet must sign to start a recurring Solana
// subscription (#261). It stores the canonical plan terms + the current step on
// the session; the response renders next_action: solana_sign_transactions.
func (s *CheckoutSessionService) initializeSolanaSubscriptionSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest) error {
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutSessionValidation)
	}
	wallet := strings.TrimSpace(payment.Wallet)
	// A subscribe session created WITHOUT a connected wallet is a Solana Pay
	// (transaction-request) subscribe: the wallet is unknown until it scans the QR
	// and POSTs its account to /solana-pay, where BuildSolanaPayTransaction calls
	// PrepareSubscribeService and returns the init/subscribe tx. This is the
	// recurring counterpart of the one-off transaction_request flow — driven purely
	// by the price being recurring; the client never sends mode:one_off.
	if wallet == "" {
		return s.initializeSolanaSubscriptionPayRequest(ctx, session)
	}
	price, err := s.priceService.GetByID(ctx, session.PriceID)
	if err != nil || price == nil {
		return fmt.Errorf("%w: price not found", ErrCheckoutSessionValidation)
	}

	// Duplicate-billing guard (issue #269): a user must never hold two concurrent
	// non-terminal subscriptions in the same product/tier-group (even at different
	// tiers — that is double-billing; the correct operation is change-tier). Run
	// this BEFORE preparing any on-chain transaction so we neither create the
	// session nor ask the wallet to sign anything for a duplicate. A tier change on
	// an EXISTING Solana subscription does NOT go through this subscribe flow — it
	// uses the dedicated atomic prepare/confirm tier-change endpoints (#272).
	if s.checkoutService != nil {
		product, err := s.productService.GetByID(ctx, price.ProductID)
		if err != nil || product == nil {
			return fmt.Errorf("%w: product not found", ErrCheckoutSessionValidation)
		}
		conflict, err := s.checkoutService.CheckSubscriptionConflict(ctx, session.TenantSubjectID.String(), price, product)
		if err != nil {
			return fmt.Errorf("%w: failed to check existing subscriptions: %v", ErrCheckoutSessionValidation, err)
		}
		if conflict != nil && conflict.Blocked {
			return fmt.Errorf("%w: %s", ErrCheckoutSessionConflict, conflict.Message)
		}
	}

	terms, err := parseSolanaPlanTerms(price.GetProcessorConfig(models.ProcessorSolana))
	if err != nil {
		return err
	}

	res, err := s.solanaPrepareSubscribe.Prepare(ctx, recurring.PrepareSubscribeInput{
		TenantID:         tenant.FromContextOrDefault(ctx),
		SubscriberWallet: wallet,
		PlanID:           terms.planID,
		MintSymbol:       terms.mintSymbol,
		AmountBaseUnits:  terms.amount,
		PeriodHours:      terms.period,
		PlanCreatedAt:    terms.createdAt,
	})
	if err != nil {
		return err
	}

	expiresAt := s.now().Add(defaultCheckoutSessionTTL)
	session.Status = models.CheckoutSessionStatusRequiresAction
	session.ExpiresAt = &expiresAt
	if session.ProcessorState == nil {
		session.ProcessorState = map[string]any{}
	}
	session.ProcessorState["flow"] = "subscription"
	session.ProcessorState["subscriber_wallet"] = wallet
	session.ProcessorState["plan_id"] = strconv.FormatUint(terms.planID, 10)
	session.ProcessorState["mint_symbol"] = terms.mintSymbol
	session.ProcessorState["amount_base_units"] = strconv.FormatUint(terms.amount, 10)
	session.ProcessorState["period_hours"] = strconv.FormatUint(terms.period, 10)
	session.ProcessorState["plan_created_at"] = strconv.FormatInt(terms.createdAt, 10)
	session.ProcessorState["subscription_pda"] = res.SubscriptionPDA
	session.ProcessorState["subscribe_step"] = res.Step
	session.ProcessorState["sign_transactions"] = toAnySlice(res.Transactions)
	return nil
}

// initializeSolanaSubscriptionPayRequest sets up a RECURRING subscribe over the
// Solana Pay transaction-request flow (no wallet at create time). It persists the
// canonical plan terms and marks the session flow=transaction_request so
// sessionToResponse surfaces a solana_pay_url; the actual init/subscribe tx is
// built later by BuildSolanaPayTransaction when the scanning wallet POSTs its
// account. The decision to land here is purely price-driven (the price carries a
// published Solana recurring plan → mode resolved to subscription) — no client
// mode override. The duplicate-billing guard still runs up front so we never
// hand out a QR that would double-bill.
func (s *CheckoutSessionService) initializeSolanaSubscriptionPayRequest(ctx context.Context, session *models.CheckoutSession) error {
	price, err := s.priceService.GetByID(ctx, session.PriceID)
	if err != nil || price == nil {
		return fmt.Errorf("%w: price not found", ErrCheckoutSessionValidation)
	}

	// Duplicate-billing guard (issue #269) — same as the wallet subscribe path:
	// refuse to issue a Solana Pay QR that would create a second concurrent
	// non-terminal subscription in the same product/tier-group.
	if s.checkoutService != nil {
		product, perr := s.productService.GetByID(ctx, price.ProductID)
		if perr != nil || product == nil {
			return fmt.Errorf("%w: product not found", ErrCheckoutSessionValidation)
		}
		conflict, cerr := s.checkoutService.CheckSubscriptionConflict(ctx, session.TenantSubjectID.String(), price, product)
		if cerr != nil {
			return fmt.Errorf("%w: failed to check existing subscriptions: %v", ErrCheckoutSessionValidation, cerr)
		}
		if conflict != nil && conflict.Blocked {
			return fmt.Errorf("%w: %s", ErrCheckoutSessionConflict, conflict.Message)
		}
	}

	terms, err := parseSolanaPlanTerms(price.GetProcessorConfig(models.ProcessorSolana))
	if err != nil {
		return err
	}

	expiresAt := s.now().Add(defaultCheckoutSessionTTL)
	session.Status = models.CheckoutSessionStatusRequiresAction
	session.ExpiresAt = &expiresAt
	if session.ProcessorState == nil {
		session.ProcessorState = map[string]any{}
	}
	// flow=transaction_request is what makes sessionToResponse build the
	// solana_pay_url; the subscribe terms travel on ProcessorState exactly like the
	// wallet path so BuildSolanaPayTransaction can re-derive the PrepareSubscribe
	// input without trusting client input. subscribe_step starts empty — the first
	// POST resolves it to "init" or "subscribe" from on-chain authority state.
	session.ProcessorState["flow"] = "transaction_request"
	session.ProcessorState["plan_id"] = strconv.FormatUint(terms.planID, 10)
	session.ProcessorState["mint_symbol"] = terms.mintSymbol
	session.ProcessorState["amount_base_units"] = strconv.FormatUint(terms.amount, 10)
	session.ProcessorState["period_hours"] = strconv.FormatUint(terms.period, 10)
	session.ProcessorState["plan_created_at"] = strconv.FormatInt(terms.createdAt, 10)
	return nil
}

// confirmSolanaSubscriptionSession advances the two-step subscribe flow (#262).
// When the current step is "init", the just-signed init has landed → re-prepare
// the subscribe transaction and stay requires_action. When "subscribe", the
// on-chain subscription exists → enroll (verify + first crank + create
// membership) and mark the session succeeded.
func (s *CheckoutSessionService) confirmSolanaSubscriptionSession(ctx context.Context, session *models.CheckoutSession, req *CheckoutSessionConfirmRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return nil, fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutSessionValidation)
	}
	wallet := strings.TrimSpace(getStringField(session.ProcessorState, "subscriber_wallet"))
	if reqWallet := strings.TrimSpace(req.Payment.Wallet); reqWallet != "" && wallet != "" && reqWallet != wallet {
		return nil, fmt.Errorf("%w: wallet does not match session", ErrCheckoutSessionValidation)
	}
	terms := solanaPlanTerms{
		planID:     getUint64Field(session.ProcessorState, "plan_id"),
		mintSymbol: getStringField(session.ProcessorState, "mint_symbol"),
		amount:     getUint64Field(session.ProcessorState, "amount_base_units"),
		period:     getUint64Field(session.ProcessorState, "period_hours"),
	}
	if v := strings.TrimSpace(getStringField(session.ProcessorState, "plan_created_at")); v != "" {
		terms.createdAt, _ = strconv.ParseInt(v, 10, 64)
	}
	tenantID := tenant.FromContextOrDefault(ctx)

	// Step 1: the signed transaction was init_subscription_authority. Re-prepare
	// the subscribe transaction (the authority now exists) and stay in
	// requires_action so the wallet signs the second transaction.
	if getStringField(session.ProcessorState, "subscribe_step") == "init" {
		res, err := s.solanaPrepareSubscribe.Prepare(ctx, recurring.PrepareSubscribeInput{
			TenantID:         tenantID,
			SubscriberWallet: wallet,
			PlanID:           terms.planID,
			MintSymbol:       terms.mintSymbol,
			AmountBaseUnits:  terms.amount,
			PeriodHours:      terms.period,
			PlanCreatedAt:    terms.createdAt,
		})
		if err != nil {
			return nil, err
		}
		if res.Step != "subscribe" {
			// Authority still not visible (init not yet confirmed) — ask the caller
			// to retry; keep the init transaction so it can re-sign/resend if needed.
			return nil, fmt.Errorf("%w: subscription authority not yet confirmed on-chain; retry", ErrCheckoutSessionConflict)
		}
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["subscribe_step"] = "subscribe"
		session.ProcessorState["sign_transactions"] = toAnySlice(res.Transactions)
		session.Status = models.CheckoutSessionStatusRequiresAction
		session.UpdatedAt = s.now()
		if err := s.repo.Update(ctx, session); err != nil {
			return nil, err
		}
		updated, err := s.repo.GetByID(ctx, session.ID)
		if err != nil {
			return nil, err
		}
		return s.sessionToResponse(updated), nil
	}

	// Step 2: subscribe has landed → enroll (verify PDA, first crank, membership).
	var email string
	if user != nil && user.Email != nil {
		email = *user.Email
	}

	sig := strings.TrimSpace(req.Payment.Signature)
	sub, err := s.solanaEnroll.ConfirmEnrollment(ctx, recurring.EnrollInput{
		TenantID:         tenantID,
		UserID:           session.TenantSubjectID.String(),
		UserEmail:        email,
		PriceID:          session.PriceID,
		SubscriberWallet: wallet,
		PlanID:           terms.planID,
		MintSymbol:       terms.mintSymbol,
		AmountBaseUnits:  terms.amount,
		PeriodHours:      terms.period,
		PlanCreatedAt:    terms.createdAt,
		FiatAmount:       session.Amount,
		Currency:         session.Currency,
		// The first pull happened inside the atomic subscribe tx the wallet just
		// submitted; record its signature on the membership/row (#286).
		Signature: sig,
	})
	if err != nil {
		// The subscribe tx hasn't reached the read commitment yet (confirmation lag):
		// the chain may already hold the subscription, but the server can't see the PDA
		// in this request window. Return a retryable conflict (not a 500) so the client
		// polls; the reconciler worker also settles it asynchronously. Mirrors the
		// Solana Pay poller path (ErrSolanaSubscribePending).
		if isSolanaSubscribeNotLandedErr(err) {
			return nil, fmt.Errorf("%w: subscription not yet confirmed on-chain; retry", ErrCheckoutSessionConflict)
		}
		return nil, err
	}

	if err := s.MarkSucceededWithSubscription(ctx, session.ID, uuid.Nil, sig, sub.ID); err != nil {
		return nil, err
	}
	updated, err := s.repo.GetByID(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	return s.sessionToResponse(updated), nil
}

// solanaLifecycleState is everything a cancel / tier-change Solana Pay session
// needs to (a) build the on-chain tx in BuildSolanaPayTransaction and (b) mirror
// the confirmed tx in the poller. It is derived once at create time and
// persisted (mode + ProcessorState carry it forward); the build/confirm paths
// re-derive it from the session row so they never trust client input.
type solanaLifecycleState struct {
	mode             models.CheckoutSessionMode
	subscriptionID   uuid.UUID
	subscriberWallet string
	productName      string
	tierChange       *resolvedSolanaLifecycleTierChange // nil for cancel
}

type resolvedSolanaLifecycleTierChange struct {
	newPriceID           uuid.UUID
	oldRow               *models.SolanaSubscription
	newTerms             solanaPlanTerms
	isUpgrade            bool
	firstChargeBaseUnits uint64
	oldPeriodEndsAt      *time.Time
}

// createSolanaLifecycleSession authorizes ownership and creates a Solana Pay
// session for a CANCEL or TIER-CHANGE on the caller's EXISTING Solana
// subscription. The subscription_id (and new_price_id for tier-change) is stored
// in ProcessorState; the public Solana Pay endpoint then builds the reference-
// tagged on-chain tx and the reference poller mirrors the confirmation.
func (s *CheckoutSessionService) createSolanaLifecycleSession(ctx context.Context, req *CheckoutSessionCreateRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	mode := models.CheckoutSessionMode(strings.TrimSpace(req.Mode))
	if s.subscriptionReader == nil || s.solanaSubscriptionRows == nil {
		return nil, fmt.Errorf("%w: solana subscription lifecycle is not configured", ErrCheckoutSessionValidation)
	}
	if mode == models.CheckoutSessionModeSolanaCancel && s.solanaPrepareCancel == nil {
		return nil, fmt.Errorf("%w: solana cancel is not configured", ErrCheckoutSessionValidation)
	}
	if mode == models.CheckoutSessionModeSolanaTierChange && s.solanaPrepareTierChange == nil {
		return nil, fmt.Errorf("%w: solana tier change is not configured", ErrCheckoutSessionValidation)
	}

	processor := strings.ToLower(strings.TrimSpace(req.Payment.Processor))
	if processor != string(models.ProcessorSolana) {
		return nil, fmt.Errorf("%w: %s mode requires the solana processor", ErrCheckoutSessionValidation, mode)
	}

	subscriptionID, err := api.ParseSubscriptionID(strings.TrimSpace(req.SubscriptionID))
	if err != nil {
		return nil, fmt.Errorf("%w: subscription_id is required", ErrCheckoutSessionValidation)
	}

	// Authorize: the acting user must OWN the target subscription, and it must be
	// an active Solana subscription.
	sub, err := s.subscriptionReader.GetByID(ctx, subscriptionID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, fmt.Errorf("%w: subscription not found", ErrCheckoutSessionValidation)
		}
		return nil, fmt.Errorf("failed to load subscription: %w", err)
	}
	if sub == nil || sub.TenantSubjectID.String() != user.ID {
		// Do not leak existence of someone else's subscription.
		return nil, fmt.Errorf("%w: subscription not found", ErrCheckoutSessionValidation)
	}
	if sub.Processor != models.ProcessorSolana {
		return nil, fmt.Errorf("%w: subscription is not a solana subscription", ErrCheckoutSessionValidation)
	}

	oldRow, err := s.solanaSubscriptionRows.GetBySubscriptionID(ctx, subscriptionID)
	if err != nil || oldRow == nil {
		return nil, fmt.Errorf("%w: no on-chain record for this subscription", ErrCheckoutSessionValidation)
	}

	lifecycle := &solanaLifecycleState{
		mode:             mode,
		subscriptionID:   subscriptionID,
		subscriberWallet: oldRow.SubscriberWallet,
	}

	// Session price + amount: cancel acts on the current price; tier-change uses
	// the target (new) price (and bills the prorated upgrade charge).
	sessionPriceID := sub.PriceID
	var sessionAmount int64
	var sessionCurrency string

	switch mode {
	case models.CheckoutSessionModeSolanaCancel:
		curPrice, perr := s.priceService.GetByID(ctx, sub.PriceID)
		if perr == nil && curPrice != nil {
			sessionAmount = curPrice.Amount
			sessionCurrency = curPrice.Currency
			lifecycle.productName = s.productDisplayName(ctx, curPrice.ProductID)
		}
	case models.CheckoutSessionModeSolanaTierChange:
		tc, perr := s.resolveSolanaTierChange(ctx, sub, oldRow, strings.TrimSpace(req.NewPriceID))
		if perr != nil {
			return nil, perr
		}
		lifecycle.tierChange = tc
		sessionPriceID = tc.newPriceID
		newPrice, gerr := s.priceService.GetByID(ctx, tc.newPriceID)
		if gerr == nil && newPrice != nil {
			sessionAmount = newPrice.Amount
			sessionCurrency = newPrice.Currency
			lifecycle.productName = s.productDisplayName(ctx, newPrice.ProductID)
		}
	}
	if strings.TrimSpace(sessionCurrency) == "" {
		sessionCurrency = "usd"
	}

	now := s.now()
	expiresAt := now.Add(defaultCheckoutSessionTTL)
	subjectID, err := s.ensureSubjectID(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant subject for checkout session: %w", err)
	}

	session := &models.CheckoutSession{
		ID:              uuidutil.NewV7(),
		TenantSubjectID: subjectID,
		PriceID:         sessionPriceID,
		Mode:            mode,
		Processor:       models.ProcessorSolana,
		Status:          models.CheckoutSessionStatusRequiresAction,
		Amount:          sessionAmount,
		Currency:        sessionCurrency,
		ExpiresAt:       &expiresAt,
		Metadata:        normalizeMetadata(req.Metadata),
		ProcessorFields: map[string]any{"processor": string(models.ProcessorSolana)},
		ProcessorState:  s.buildLifecycleState(lifecycle),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if strings.TrimSpace(req.IdempotencyKey) != "" {
		session.IdempotencyKey = normalize.OptionalString(req.IdempotencyKey)
	}

	if err := s.repo.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create checkout session: %w", err)
	}
	return s.sessionToResponse(session), nil
}

// buildLifecycleState serializes the resolved lifecycle facts onto ProcessorState
// (JSON-safe scalars), mirroring how the subscribe flow persists its plan terms.
// "flow" marks the session as a lifecycle session so the poller/builder branch.
func (s *CheckoutSessionService) buildLifecycleState(l *solanaLifecycleState) map[string]any {
	state := map[string]any{
		"flow":            string(l.mode),
		"subscription_id": l.subscriptionID.String(),
	}
	if strings.TrimSpace(l.subscriberWallet) != "" {
		state["subscriber_wallet"] = l.subscriberWallet
	}
	if strings.TrimSpace(l.productName) != "" {
		state["product_name"] = l.productName
	}
	if tc := l.tierChange; tc != nil {
		state["new_price_id"] = tc.newPriceID.String()
		state["tier_is_upgrade"] = tc.isUpgrade
		state["tier_first_charge_base_units"] = strconv.FormatUint(tc.firstChargeBaseUnits, 10)
		state["tier_new_plan_id"] = strconv.FormatUint(tc.newTerms.planID, 10)
		state["tier_new_mint_symbol"] = tc.newTerms.mintSymbol
		state["tier_new_amount_base_units"] = strconv.FormatUint(tc.newTerms.amount, 10)
		state["tier_new_period_hours"] = strconv.FormatUint(tc.newTerms.period, 10)
		state["tier_new_plan_created_at"] = strconv.FormatInt(tc.newTerms.createdAt, 10)
		if tc.oldPeriodEndsAt != nil {
			state["tier_old_period_ends_at"] = tc.oldPeriodEndsAt.UTC().Format(time.RFC3339)
		}
	}
	return state
}

func (s *CheckoutSessionService) productDisplayName(ctx context.Context, productID uuid.UUID) string {
	if s.productService == nil {
		return ""
	}
	product, err := s.productService.GetByID(ctx, productID)
	if err != nil || product == nil {
		return ""
	}
	return product.DisplayName
}

// resolveSolanaTierChange mirrors the resolution the auth-gated tier-change
// handler does (ownership already verified by the caller): load the OLD on-chain
// row, resolve the NEW price's canonical plan terms, decide upgrade vs downgrade
// within the tier group, and compute the Model-B prorated first charge for an
// upgrade.
func (s *CheckoutSessionService) resolveSolanaTierChange(ctx context.Context, oldSub *models.Subscription, oldRow *models.SolanaSubscription, newPriceIDStr string) (*resolvedSolanaLifecycleTierChange, error) {
	newPriceID, err := api.ParsePriceID(newPriceIDStr)
	if err != nil {
		return nil, fmt.Errorf("%w: new_price_id is required", ErrCheckoutSessionValidation)
	}
	newPrice, err := s.priceService.GetByID(ctx, newPriceID)
	if err != nil || newPrice == nil {
		return nil, fmt.Errorf("%w: target price not found", ErrCheckoutSessionValidation)
	}
	if !newPrice.IsPurchasable() {
		return nil, fmt.Errorf("%w: target price is not available", ErrCheckoutSessionValidation)
	}
	newTerms, err := parseSolanaPlanTerms(newPrice.GetProcessorConfig(models.ProcessorSolana))
	if err != nil {
		return nil, fmt.Errorf("%w: target price is not configured for Solana recurring billing", ErrCheckoutSessionValidation)
	}

	oldPrice, err := s.priceService.GetByID(ctx, oldSub.PriceID)
	if err != nil || oldPrice == nil {
		return nil, fmt.Errorf("%w: current price not found", ErrCheckoutSessionValidation)
	}
	newProduct, err := s.productService.GetByID(ctx, newPrice.ProductID)
	if err != nil || newProduct == nil {
		return nil, fmt.Errorf("%w: target product not found", ErrCheckoutSessionValidation)
	}
	oldProduct, err := s.productService.GetByID(ctx, oldPrice.ProductID)
	if err != nil || oldProduct == nil {
		return nil, fmt.Errorf("%w: current product not found", ErrCheckoutSessionValidation)
	}
	if oldProduct.ID == newProduct.ID {
		return nil, fmt.Errorf("%w: already subscribed to this product", ErrCheckoutSessionValidation)
	}
	if oldProduct.TierGroup != nil && newProduct.TierGroup != nil &&
		strings.TrimSpace(*oldProduct.TierGroup) != strings.TrimSpace(*newProduct.TierGroup) {
		return nil, fmt.Errorf("%w: tier change must stay within the same tier group", ErrCheckoutSessionValidation)
	}

	isUpgrade := newProduct.TierRank >= oldProduct.TierRank
	out := &resolvedSolanaLifecycleTierChange{
		newPriceID:      newPriceID,
		oldRow:          oldRow,
		newTerms:        newTerms,
		isUpgrade:       isUpgrade,
		oldPeriodEndsAt: oldSub.CurrentPeriodEndsAt,
	}
	if isUpgrade {
		firstChargeCents, _ := CalculateModelBUpgradeCharge(
			oldPrice.Amount,
			newPrice.Amount,
			oldSub.CurrentPeriodEndsAt,
			newPrice.BillingCycleDays,
			s.now(),
		)
		firstChargeBaseUnits := solanamodule.FiatCentsToStablecoinBaseUnits(ctx, firstChargeCents, newTerms.mintSymbol, s.priceProvider)
		if firstChargeBaseUnits == 0 {
			firstChargeBaseUnits = 1
		}
		out.firstChargeBaseUnits = firstChargeBaseUnits
	}
	return out, nil
}

func (s *CheckoutSessionService) initializeCheckoutSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest, user *UserIdentity) error {
	if s.checkoutService == nil {
		return fmt.Errorf("%w: checkout service unavailable", ErrCheckoutSessionValidation)
	}

	req := &CheckoutRequest{
		PriceID:         api.FormatPriceID(session.PriceID),
		PaymentMethodID: payment.PaymentMethodID,
		PaymentToken:    payment.PaymentToken,
		Processor:       string(session.Processor),
		Metadata:        session.Metadata,
		Email:           payment.Email,
		FirstName:       payment.FirstName,
		LastName:        payment.LastName,
		Address1:        payment.Address1,
		City:            payment.City,
		State:           payment.State,
		Zip:             payment.Zip,
		Country:         payment.Country,
		LastFour:        payment.LastFour,
		CardType:        payment.CardType,
		ExpiryDate:      payment.ExpiryDate,
	}

	if session.IdempotencyKey != nil {
		if key := strings.TrimSpace(*session.IdempotencyKey); key != "" {
			req.IdempotencyKey = fmt.Sprintf("checkout_session:%s", key)
		}
	}
	if session.Processor == models.ProcessorStripe || session.Processor == models.ProcessorCCBill {
		req.CheckoutSessionID = api.FormatCheckoutSessionID(session.ID)
	}

	resp, err := s.checkoutService.Checkout(ctx, req, user)
	if err != nil {
		return err
	}

	return s.applyCheckoutResponse(session, resp)
}

func (s *CheckoutSessionService) applyCheckoutResponse(session *models.CheckoutSession, resp *CheckoutResponse) error {
	if session == nil {
		return fmt.Errorf("%w: session is required", ErrCheckoutSessionValidation)
	}
	if resp == nil {
		return fmt.Errorf("%w: checkout response is required", ErrCheckoutSessionValidation)
	}

	switch resp.Status {
	case "success", "pending":
		session.Status = models.CheckoutSessionStatusSucceeded
		if resp.PaymentID != nil {
			session.PaymentID = resp.PaymentID
		}
		if resp.SubscriptionID != nil {
			session.SubscriptionID = resp.SubscriptionID
		}
		if strings.TrimSpace(resp.TransactionID) != "" {
			session.TransactionID = normalize.OptionalString(resp.TransactionID)
		}
	case "redirect_required":
		redirectURL := strings.TrimSpace(resp.RedirectURL)
		if redirectURL == "" {
			return fmt.Errorf("%w: redirect url missing", ErrCheckoutSessionValidation)
		}
		session.Status = models.CheckoutSessionStatusRequiresAction
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["redirect_url"] = redirectURL
	case "blocked":
		msg := strings.TrimSpace(resp.Message)
		if msg == "" {
			msg = "checkout blocked"
		}
		return fmt.Errorf("%w: %s", ErrCheckoutSessionConflict, msg)
	default:
		return fmt.Errorf("%w: unsupported checkout status", ErrCheckoutSessionConflict)
	}

	return nil
}

func requireBillingFields(payment *CheckoutSessionPaymentRequest) error {
	if strings.TrimSpace(payment.Email) == "" ||
		strings.TrimSpace(payment.FirstName) == "" ||
		strings.TrimSpace(payment.LastName) == "" ||
		strings.TrimSpace(payment.Address1) == "" ||
		strings.TrimSpace(payment.City) == "" ||
		strings.TrimSpace(payment.Zip) == "" ||
		strings.TrimSpace(payment.Country) == "" {
		return fmt.Errorf("%w: billing fields are required", ErrCheckoutSessionValidation)
	}
	if _, err := mail.ParseAddress(strings.TrimSpace(payment.Email)); err != nil {
		return fmt.Errorf("%w: email is invalid", ErrCheckoutSessionValidation)
	}
	if len(strings.TrimSpace(payment.Country)) != 2 {
		return fmt.Errorf("%w: country must be ISO-3166 alpha-2", ErrCheckoutSessionValidation)
	}
	return nil
}

func (s *CheckoutSessionService) buildProcessorFields(processor string, payment *CheckoutSessionPaymentRequest) map[string]any {
	fields := map[string]any{
		"processor": processor,
	}

	addField(fields, "payment_method_id", payment.PaymentMethodID)
	addField(fields, "token_symbol", payment.TokenSymbol)
	addField(fields, "flow", payment.Flow)
	addField(fields, "wallet", payment.Wallet)
	addField(fields, "email", payment.Email)
	addField(fields, "first_name", payment.FirstName)
	addField(fields, "last_name", payment.LastName)
	addField(fields, "address1", payment.Address1)
	addField(fields, "city", payment.City)
	addField(fields, "state", payment.State)
	addField(fields, "zip", payment.Zip)
	addField(fields, "country", payment.Country)

	return fields
}

func addField(fields map[string]any, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	fields[key] = strings.TrimSpace(value)
}

func (s *CheckoutSessionService) sessionToResponse(session *models.CheckoutSession) *CheckoutSessionResponse {
	resp := &CheckoutSessionResponse{
		Object:  "checkout_session",
		ID:      api.FormatCheckoutSessionID(session.ID),
		Status:  string(session.Status),
		Mode:    string(session.Mode),
		PriceID: api.FormatPriceID(session.PriceID),
		Payment: CheckoutSessionPaymentResponse{
			Processor: string(session.Processor),
		},
		ExpiresAt: session.ExpiresAt,
	}
	if len(session.Metadata) > 0 {
		resp.Metadata = session.Metadata
	}

	if session.Reference != nil {
		resp.Payment.Reference = *session.Reference
	}
	if session.TransactionID != nil {
		resp.Payment.TransactionID = *session.TransactionID
	}

	if session.PaymentID != nil {
		paymentID := api.FormatPaymentID(*session.PaymentID)
		resp.PaymentID = &paymentID
	}
	if session.SubscriptionID != nil {
		subID := api.FormatSubscriptionID(*session.SubscriptionID)
		resp.SubscriptionID = &subID
	}

	if session.ProcessorState != nil {
		if val, ok := session.ProcessorState["transaction_url"].(string); ok && strings.TrimSpace(val) != "" {
			resp.Payment.TransactionURL = val
		}
		// Build solana_pay_url for every Solana-Pay-capable session. The wallet POSTs
		// its account to this URL and the matching BuildSolanaPayTransaction branch
		// returns the right tx:
		//   - transaction_request flow → one-off transfer OR recurring subscribe
		//     (price-driven: BuildSolanaPayTransaction reads the session mode).
		//   - solana_cancel / solana_tier_change modes → the lifecycle tx.
		if solanaSessionUsesPayURL(session) {
			// Construct the Solana Pay URL:
			// - standalone: solana:{api_url}/v1/checkout/:id/solana-pay
			// - embedded:   solana:{api_url}/v1/checkout/:id/solana-pay (api_url typically ends with /billing)
			baseURL := s.getAPIBaseURL()
			if baseURL != "" {
				resp.Payment.SolanaPayURL = fmt.Sprintf(
					"solana:%s/v1/checkout/%s/solana-pay",
					baseURL,
					api.FormatCheckoutSessionID(session.ID),
				)
			}
		}
		if val, ok := session.ProcessorState["redirect_url"].(string); ok && strings.TrimSpace(val) != "" {
			resp.URL = strings.TrimSpace(val)
			resp.Payment.RedirectURL = resp.URL
		}
		if val, ok := session.ProcessorState["message"].(string); ok && strings.TrimSpace(val) != "" {
			resp.Message = strings.TrimSpace(val)
		} else if val, ok := session.ProcessorState["failure_reason"].(string); ok && strings.TrimSpace(val) != "" {
			resp.Message = strings.TrimSpace(val)
		}
	}

	if action := s.buildNextAction(resp); action != nil {
		resp.NextAction = action
	}

	// Recurring Solana subscribe (#261): surface the unsigned transaction(s) to
	// sign. Takes precedence over other next_actions for a subscription session.
	if resp.Status == string(models.CheckoutSessionStatusRequiresAction) {
		if txns := getStringSliceField(session.ProcessorState, "sign_transactions"); len(txns) > 0 {
			resp.NextAction = &CheckoutSessionNextAction{
				Type:         "solana_sign_transactions",
				Transactions: txns,
			}
		}
	}

	return resp
}

func normalizeMetadata(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		k := strings.TrimSpace(key)
		if k == "" {
			continue
		}
		out[k] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func scopeIdempotencyKey(userID, key string) string {
	trimmedKey := strings.TrimSpace(key)
	if strings.TrimSpace(userID) == "" || trimmedKey == "" {
		return trimmedKey
	}
	return fmt.Sprintf("%s:%s", strings.TrimSpace(userID), trimmedKey)
}

func (s *CheckoutSessionService) isTerminal(status models.CheckoutSessionStatus) bool {
	switch status {
	case models.CheckoutSessionStatusSucceeded,
		models.CheckoutSessionStatusFailed,
		models.CheckoutSessionStatusExpired,
		models.CheckoutSessionStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *CheckoutSessionService) isExpired(session *models.CheckoutSession) bool {
	if session.ExpiresAt == nil || session.ExpiresAt.IsZero() {
		return false
	}
	return session.ExpiresAt.Before(s.now())
}

func (s *CheckoutSessionService) buildNextAction(resp *CheckoutSessionResponse) *CheckoutSessionNextAction {
	if resp == nil {
		return nil
	}
	if resp.Status != string(models.CheckoutSessionStatusRequiresAction) {
		return nil
	}
	if resp.Payment.RedirectURL != "" {
		return &CheckoutSessionNextAction{
			Type: "redirect_to_url",
			RedirectToURL: &CheckoutSessionRedirectToURL{
				URL: resp.Payment.RedirectURL,
			},
		}
	}
	if resp.Payment.TransactionURL != "" {
		return &CheckoutSessionNextAction{
			Type: "solana_qr",
		}
	}
	if resp.Payment.SolanaPayURL != "" {
		return &CheckoutSessionNextAction{
			Type: "solana_pay",
		}
	}
	return nil
}

// solanaSessionUsesPayURL reports whether a session should surface a
// solana_pay_url (i.e. the wallet completes it by scanning a QR / POSTing its
// account to the solana-pay endpoint). True for the transaction_request flow
// (one-off transfer OR recurring subscribe — the build endpoint picks based on
// the session mode/price) and for the cancel / tier-change lifecycle modes.
func solanaSessionUsesPayURL(session *models.CheckoutSession) bool {
	if session == nil || session.Processor != models.ProcessorSolana {
		return false
	}
	switch session.Mode {
	case models.CheckoutSessionModeSolanaCancel, models.CheckoutSessionModeSolanaTierChange:
		return true
	}
	if flow, ok := session.ProcessorState["flow"].(string); ok && flow == "transaction_request" {
		return true
	}
	return false
}

// getAPIBaseURL returns the API base URL for building Solana Pay URLs.
// Uses config.APIURL which should be set to the full base URL where billing routes are mounted.
//
// Standalone: "https://api.mysite.com" → routes at /v1/*
// Embedded:   "https://api.mysite.com/billing" → routes at /billing/v1/*
//
// Generated URLs follow the pattern: APIURL + "/v1/checkout/:id/solana-pay"
func (s *CheckoutSessionService) getAPIBaseURL() string {
	if s.config == nil {
		return ""
	}
	apiURL := strings.TrimSpace(s.config.APIURL)
	if apiURL == "" {
		return ""
	}
	// Ensure it doesn't end with a slash (we add the version path later)
	return strings.TrimSuffix(apiURL, "/")
}

func (s *CheckoutSessionService) confirmSolanaSession(ctx context.Context, session *models.CheckoutSession, req *CheckoutSessionConfirmRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if strings.TrimSpace(req.Payment.Signature) == "" {
		return nil, fmt.Errorf("%w: signature is required", ErrCheckoutSessionValidation)
	}
	if s.solanaTransactionService == nil {
		return nil, fmt.Errorf("%w: solana transaction service unavailable", ErrCheckoutSessionValidation)
	}
	if s.checkoutService == nil {
		return nil, fmt.Errorf("%w: checkout service unavailable", ErrCheckoutSessionValidation)
	}
	solanaProc, err := solanamodule.RequireSolanaProcessorConfig(s.config)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckoutSessionValidation, err)
	}

	// Get token symbol from ProcessorState (where initializeSolanaSession stores it)
	tokenSymbol := strings.ToUpper(strings.TrimSpace(getStringField(session.ProcessorState, "token_symbol")))
	if tokenSymbol == "" {
		return nil, fmt.Errorf("%w: token_symbol missing", ErrCheckoutSessionValidation)
	}

	tokenCfg, ok := solanaProc.Tokens[tokenSymbol]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported token", ErrCheckoutSessionValidation)
	}
	tokenMint := tokenCfg.Mint
	storedTokenMint := getStringField(session.ProcessorState, "token_mint")
	if storedTokenMint == "" {
		return nil, fmt.Errorf("%w: token_mint missing", ErrCheckoutSessionValidation)
	}
	if !strings.EqualFold(tokenSymbol, "SOL") && solanamodule.IsNativeSOLMint(storedTokenMint) {
		return nil, fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrCheckoutSessionValidation)
	}
	if !strings.EqualFold(storedTokenMint, tokenMint) {
		return nil, fmt.Errorf("%w: token_mint mismatch", ErrCheckoutSessionValidation)
	}

	expectedAmount := getUint64Field(session.ProcessorState, "token_amount")
	if expectedAmount == 0 {
		return nil, fmt.Errorf("%w: token_amount missing or invalid", ErrCheckoutSessionValidation)
	}
	expectedRecipient := getStringField(session.ProcessorState, "recipient")
	if expectedRecipient == "" {
		return nil, fmt.Errorf("%w: recipient missing", ErrCheckoutSessionValidation)
	}
	// Get payer from ProcessorState (set by BuildSolanaPayTransaction)
	expectedPayer := strings.TrimSpace(getStringField(session.ProcessorState, "payer"))
	if reqWallet := strings.TrimSpace(req.Payment.Wallet); reqWallet != "" {
		if expectedPayer != "" && expectedPayer != reqWallet {
			return nil, fmt.Errorf("%w: wallet does not match session", ErrCheckoutSessionValidation)
		}
		if expectedPayer == "" {
			expectedPayer = reqWallet
		}
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		return nil, fmt.Errorf("%w: reference missing", ErrCheckoutSessionValidation)
	}
	referenceValue := strings.TrimSpace(*session.Reference)
	reference := &referenceValue

	if err := s.solanaTransactionService.VerifyTransactionWithContent(
		ctx,
		strings.TrimSpace(req.Payment.Signature),
		expectedAmount,
		expectedRecipient,
		storedTokenMint,
		expectedPayer,
		reference,
		session.ExpiresAt,
	); err != nil {
		return nil, err
	}

	signature := strings.TrimSpace(req.Payment.Signature)
	if s.db != nil {
		if existingPayment, err := repo.NewPaymentRepo(s.db).GetByTransactionID(ctx, models.ProcessorSolana, signature); err == nil {
			if err := validateSolanaPaymentMatchesSession(existingPayment, session, referenceValue); err != nil {
				return nil, err
			}
			if err := s.MarkSucceeded(ctx, session.ID, existingPayment.ID, signature); err != nil {
				return nil, err
			}
			if err := s.finalizeSolanaTransferReference(ctx, session, signature); err != nil {
				return nil, err
			}
			updated, err := s.repo.GetByID(ctx, session.ID)
			if err != nil {
				return nil, err
			}
			return s.sessionToResponse(updated), nil
		} else if !repo.IsNotFound(err) {
			return nil, fmt.Errorf("failed checking existing solana payment: %w", err)
		}
	}

	result, err := s.checkoutService.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        session.TenantSubjectID.String(),
		PriceID:       session.PriceID,
		Processor:     "solana",
		TransactionID: signature,
		Amount:        session.Amount,
		Currency:      session.Currency,
		Metadata: map[string]any{
			"solana_reference":    referenceValue,
			"checkout_session_id": session.ID.String(),
		},
	})
	if err != nil {
		return nil, err
	}
	if err := s.verifyRegisteredSolanaPayment(ctx, result.PaymentID, session, referenceValue); err != nil {
		return nil, err
	}

	if err := s.MarkSucceeded(ctx, session.ID, result.PaymentID, signature); err != nil {
		return nil, err
	}
	if err := s.finalizeSolanaTransferReference(ctx, session, signature); err != nil {
		return nil, err
	}

	updated, err := s.repo.GetByID(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	return s.sessionToResponse(updated), nil
}

func (s *CheckoutSessionService) verifyRegisteredSolanaPayment(ctx context.Context, paymentID uuid.UUID, session *models.CheckoutSession, reference string) error {
	if paymentID == uuid.Nil || session == nil || s.db == nil {
		return nil
	}
	payment, err := repo.NewPaymentRepo(s.db).GetByID(ctx, paymentID)
	if err != nil {
		return fmt.Errorf("failed to verify registered solana payment: %w", err)
	}
	return validateSolanaPaymentMatchesSession(payment, session, reference)
}

func validateSolanaPaymentMatchesSession(payment *models.Payment, session *models.CheckoutSession, reference string) error {
	if payment == nil || session == nil {
		return fmt.Errorf("%w: solana payment does not match checkout session", ErrCheckoutSessionConflict)
	}
	if payment.TenantSubjectID.String() != session.TenantSubjectID.String() || payment.PriceID != session.PriceID || payment.Amount != session.Amount || !strings.EqualFold(payment.Currency, session.Currency) {
		return fmt.Errorf("%w: solana payment does not match checkout session", ErrCheckoutSessionConflict)
	}
	if strings.TrimSpace(fmt.Sprint(payment.Metadata["solana_reference"])) != strings.TrimSpace(reference) {
		return fmt.Errorf("%w: solana signature already belongs to a different reference", ErrCheckoutSessionConflict)
	}
	if strings.TrimSpace(fmt.Sprint(payment.Metadata["checkout_session_id"])) != session.ID.String() {
		return fmt.Errorf("%w: solana signature already belongs to a different checkout session", ErrCheckoutSessionConflict)
	}
	return nil
}

func (s *CheckoutSessionService) MarkSucceeded(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if s.isTerminal(session.Status) {
		if session.Status == models.CheckoutSessionStatusSucceeded {
			return nil
		}
		if session.Status == models.CheckoutSessionStatusExpired && session.Processor == models.ProcessorSolana && paymentID != uuid.Nil && strings.TrimSpace(transactionID) != "" {
			// A wallet may broadcast before expiry but the app may confirm after expiry.
			// The caller has already verified the signature against the session-bound quote.
		} else {
			return ErrCheckoutSessionConflict
		}
	}
	if s.isExpired(session) && session.Processor != models.ProcessorSolana {
		_ = s.MarkExpired(ctx, session.ID, "checkout session expired")
		return ErrCheckoutSessionExpired
	}

	session.Status = models.CheckoutSessionStatusSucceeded
	session.UpdatedAt = s.now()
	if paymentID != uuid.Nil {
		session.PaymentID = &paymentID
	}
	if strings.TrimSpace(transactionID) != "" {
		session.TransactionID = normalize.OptionalString(transactionID)
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutSessionService) MarkFailed(ctx context.Context, sessionID uuid.UUID, reason, code string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if s.isTerminal(session.Status) {
		switch session.Status {
		case models.CheckoutSessionStatusFailed,
			models.CheckoutSessionStatusSucceeded,
			models.CheckoutSessionStatusExpired,
			models.CheckoutSessionStatusCanceled:
			return nil
		default:
			return ErrCheckoutSessionConflict
		}
	}

	session.Status = models.CheckoutSessionStatusFailed
	session.UpdatedAt = s.now()
	if session.ProcessorState == nil {
		session.ProcessorState = map[string]any{}
	}
	if msg := strings.TrimSpace(reason); msg != "" {
		session.ProcessorState["message"] = msg
		session.ProcessorState["failure_reason"] = msg
	}
	if strings.TrimSpace(code) != "" {
		session.ProcessorState["failure_code"] = strings.TrimSpace(code)
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutSessionService) MarkExpired(ctx context.Context, sessionID uuid.UUID, message string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if s.isTerminal(session.Status) {
		return nil
	}

	session.Status = models.CheckoutSessionStatusExpired
	session.UpdatedAt = s.now()
	if msg := strings.TrimSpace(message); msg != "" {
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["message"] = msg
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutSessionService) MarkSucceededWithSubscription(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string, subscriptionID uuid.UUID) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if s.isTerminal(session.Status) {
		if session.Status == models.CheckoutSessionStatusSucceeded {
			return nil
		}
		return ErrCheckoutSessionConflict
	}
	if s.isExpired(session) {
		_ = s.MarkExpired(ctx, session.ID, "checkout session expired")
		return ErrCheckoutSessionExpired
	}

	session.Status = models.CheckoutSessionStatusSucceeded
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

func (s *CheckoutSessionService) FindOpenByUserPriceProcessor(ctx context.Context, userID string, priceID uuid.UUID, processor models.Processor) (*models.CheckoutSession, error) {
	if s.repo == nil {
		return nil, ErrCheckoutSessionNotFound
	}
	session, err := s.repo.GetLatestOpenByUserPriceProcessor(ctx, userID, priceID, processor)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

func (s *CheckoutSessionService) FindOpenCCBillReservation(ctx context.Context, reservationID string, userID string, priceID uuid.UUID) (*models.CheckoutSession, error) {
	if s.repo == nil {
		return nil, ErrCheckoutSessionNotFound
	}
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return nil, sql.ErrNoRows
	}
	sessionID, err := api.ParseCheckoutSessionID(reservationID)
	if err != nil {
		return nil, err
	}
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session.TenantSubjectID.String() != userID || session.PriceID != priceID || session.Processor != models.ProcessorCCBill {
		return nil, ErrCheckoutSessionConflict
	}
	if s.isTerminal(session.Status) || s.isExpired(session) {
		return nil, ErrCheckoutSessionExpired
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
	case float64:
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

func isSolanaTransferRequestFlow(session *models.CheckoutSession) bool {
	if session == nil {
		return false
	}
	flow := strings.ToLower(strings.TrimSpace(getStringField(session.ProcessorState, "flow")))
	if flow == "" {
		// Legacy default for Solana checkout sessions.
		return true
	}
	return flow == "transfer_request"
}

func (s *CheckoutSessionService) finalizeSolanaTransferReference(ctx context.Context, session *models.CheckoutSession, transactionID string) error {
	if session == nil || session.Processor != models.ProcessorSolana {
		return nil
	}
	if !isSolanaTransferRequestFlow(session) {
		return nil
	}
	if session.Reference == nil {
		return nil
	}
	reference := strings.TrimSpace(*session.Reference)
	if reference == "" {
		return nil
	}
	if s.solanaPayService == nil {
		return nil
	}

	if err := s.solanaPayService.ConsumeAndRemovePending(ctx, reference, strings.TrimSpace(transactionID)); err != nil {
		return fmt.Errorf("failed to finalize solana reference %s: %w", reference, err)
	}

	return nil
}

func setSolanaQuoteState(processorState map[string]any, tokenAmount uint64, tokenPriceUSD, fxRate float64, fxCurrency string, quotedAt, quoteExpiresAt time.Time) error {
	if processorState == nil {
		return fmt.Errorf("%w: processor_state unavailable", ErrCheckoutSessionValidation)
	}
	if tokenAmount == 0 {
		return fmt.Errorf("%w: token_amount must be greater than 0", ErrCheckoutSessionValidation)
	}
	if quotedAt.IsZero() {
		return fmt.Errorf("%w: quote timestamp missing", ErrCheckoutSessionValidation)
	}
	if quoteExpiresAt.IsZero() {
		return fmt.Errorf("%w: quote expiry missing", ErrCheckoutSessionValidation)
	}

	processorState["token_amount"] = tokenAmount
	processorState["token_price_usd"] = tokenPriceUSD
	processorState["fx_rate"] = fxRate
	processorState["fx_currency"] = strings.TrimSpace(fxCurrency)
	processorState["quoted_at"] = quotedAt.UTC().Format(time.RFC3339)
	processorState["quote_expires_at"] = quoteExpiresAt.UTC().Format(time.RFC3339)

	return nil
}

// GetSessionForSolanaPay retrieves and validates a checkout session for Solana Pay spec endpoints.
// Returns session info needed for GET endpoint or an error if the session is invalid.
func (s *CheckoutSessionService) GetSessionForSolanaPay(ctx context.Context, sessionID uuid.UUID) (*solanamodule.PaySessionInfo, error) {
	if s.repo == nil {
		return nil, ErrCheckoutSessionNotFound
	}

	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, ErrCheckoutSessionNotFound
		}
		return nil, err
	}
	// Validate it's a Solana session
	if session.Processor != models.ProcessorSolana {
		return nil, ErrCheckoutSessionNotSolana
	}

	// Check if expired
	if session.ExpiresAt != nil && s.now().After(*session.ExpiresAt) {
		return nil, ErrCheckoutSessionExpired
	}

	// Check if already completed
	if session.Status == models.CheckoutSessionStatusSucceeded ||
		session.Status == models.CheckoutSessionStatusCanceled {
		return nil, ErrCheckoutSessionAlreadyCompleted
	}

	// Get product name for label (via price)
	var productName string
	if s.priceService != nil {
		price, err := s.priceService.GetByID(ctx, session.PriceID)
		if err == nil && s.productService != nil {
			product, err := s.productService.GetByID(ctx, price.ProductID)
			if err == nil {
				productName = product.DisplayName
			}
		}
	}

	// Lifecycle sessions render a mode-specific label (e.g. "Cancel Pro",
	// "Change to Pro") so the wallet shows the right action, not "pay".
	switch session.Mode {
	case models.CheckoutSessionModeSolanaCancel:
		productName = solanaLifecycleLabel("Cancel", productName, session.ProcessorState)
	case models.CheckoutSessionModeSolanaTierChange:
		productName = solanaLifecycleLabel("Change to", productName, session.ProcessorState)
	}

	return &solanamodule.PaySessionInfo{
		ProductName: productName,
	}, nil
}

// solanaLifecycleLabel builds the wallet label for a lifecycle Solana Pay
// session, falling back to the product name persisted on the session state when
// the price/product lookup did not resolve a display name.
func solanaLifecycleLabel(verb, productName string, state map[string]any) string {
	name := strings.TrimSpace(productName)
	if name == "" {
		name = strings.TrimSpace(getStringField(state, "product_name"))
	}
	if name == "" {
		return strings.TrimSpace(verb + " subscription")
	}
	return strings.TrimSpace(verb + " " + name)
}

// BuildSolanaPayTransaction builds a Solana transaction for the given checkout session and wallet account.
// This implements the POST endpoint of the Solana Pay Transaction Request spec.
func (s *CheckoutSessionService) BuildSolanaPayTransaction(ctx context.Context, sessionID uuid.UUID, account string) (*solanamodule.PayTransactionResponse, error) {
	if s.repo == nil {
		return nil, ErrCheckoutSessionNotFound
	}
	if s.solanaTransactionService == nil {
		return nil, fmt.Errorf("%w: solana transaction service unavailable", ErrCheckoutSessionValidation)
	}
	account = strings.TrimSpace(account)
	if account == "" {
		return nil, fmt.Errorf("%w: account is required", ErrCheckoutSessionValidation)
	}

	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, ErrCheckoutSessionNotFound
		}
		return nil, err
	}
	// Validate it's a Solana session
	if session.Processor != models.ProcessorSolana {
		return nil, ErrCheckoutSessionNotSolana
	}

	// Check if expired
	if session.ExpiresAt != nil && s.now().After(*session.ExpiresAt) {
		return nil, ErrCheckoutSessionExpired
	}

	// Check if already completed
	if session.Status == models.CheckoutSessionStatusSucceeded ||
		session.Status == models.CheckoutSessionStatusCanceled {
		return nil, ErrCheckoutSessionAlreadyCompleted
	}

	// Lifecycle modes (cancel / tier-change) build their on-chain tx via the
	// recurring services, with the Solana Pay reference injected, instead of the
	// transfer-request quote path.
	switch session.Mode {
	case models.CheckoutSessionModeSolanaCancel, models.CheckoutSessionModeSolanaTierChange, models.CheckoutSessionModeSubscription:
		var resp *solanamodule.PayTransactionResponse
		switch session.Mode {
		case models.CheckoutSessionModeSolanaCancel:
			resp, err = s.buildSolanaCancelTransaction(ctx, session, account)
		case models.CheckoutSessionModeSolanaTierChange:
			resp, err = s.buildSolanaTierChangeTransaction(ctx, session, account)
		default:
			// RECURRING subscribe over Solana Pay (price-driven): build the init or the
			// atomic [subscribe+transfer] tx via PrepareSubscribeService for the POSTed
			// account, with the Solana Pay reference injected so the poller can detect it.
			resp, err = s.buildSolanaSubscribeTransaction(ctx, session, account)
		}
		if err != nil {
			return nil, err
		}
		// The lifecycle/subscribe build binds the reference to the DB session but does
		// NOT seed the poller's pending set (only the one-off transfer flow does, via
		// GeneratePayment). Register it here so the reference poller actually picks up
		// the landed cancel/tier-change/subscribe tx; otherwise it would never confirm.
		if s.solanaPayService != nil && session.Reference != nil {
			if rerr := s.solanaPayService.RegisterPendingReference(ctx, strings.TrimSpace(*session.Reference)); rerr != nil {
				return nil, fmt.Errorf("%w: register solana pay reference: %v", ErrCheckoutSessionValidation, rerr)
			}
		}
		return resp, nil
	}

	// Get token symbol from processor state
	tokenSymbol := getStringField(session.ProcessorState, "token_symbol")
	if tokenSymbol == "" {
		return nil, fmt.Errorf("%w: token_symbol missing from session", ErrCheckoutSessionValidation)
	}

	if session.ProcessorState == nil {
		session.ProcessorState = map[string]any{}
	}
	if existingPayer := strings.TrimSpace(getStringField(session.ProcessorState, "payer")); existingPayer != "" && existingPayer != account {
		return nil, fmt.Errorf("%w: solana checkout session is already bound to a different payer", ErrCheckoutSessionConflict)
	}

	// Generate and persist the payment binding before returning a transaction to the wallet.
	if session.Reference == nil || *session.Reference == "" {
		reference, err := solana.GenerateReference()
		if err != nil {
			return nil, fmt.Errorf("failed to generate reference: %w", err)
		}
		session.Reference = &reference
	}
	session.ProcessorState["payer"] = account
	if err := s.repo.BindSolanaTransactionRequest(ctx, session, account, s.now()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckoutSessionConflict, err)
	}

	buildReq, err := solanaBuildRequestFromSession(session, account, tokenSymbol)
	if err != nil {
		return nil, err
	}

	// Build the transaction from the quote already persisted on the checkout session.
	txResp, err := s.solanaTransactionService.BuildPaymentTransactionFromQuote(ctx, buildReq)
	if err != nil {
		return nil, err
	}

	// Build message for wallet
	message := txResp.Instructions
	if message == "" {
		message = "Sign to complete your payment"
	}

	return &solanamodule.PayTransactionResponse{
		TransactionBase64: txResp.TransactionBase64,
		Message:           message,
	}, nil
}

func solanaBuildRequestFromSession(session *models.CheckoutSession, account, tokenSymbol string) (*solanamodule.PaymentTransactionBuildRequest, error) {
	if session == nil {
		return nil, fmt.Errorf("%w: session is required", ErrCheckoutSessionValidation)
	}
	tokenAmount := getUint64Field(session.ProcessorState, "token_amount")
	if tokenAmount == 0 {
		return nil, fmt.Errorf("%w: token_amount missing from session", ErrCheckoutSessionValidation)
	}
	tokenMint := getStringField(session.ProcessorState, "token_mint")
	if tokenMint == "" {
		return nil, fmt.Errorf("%w: token_mint missing from session", ErrCheckoutSessionValidation)
	}
	if !strings.EqualFold(tokenSymbol, "SOL") && solanamodule.IsNativeSOLMint(tokenMint) {
		return nil, fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrCheckoutSessionValidation)
	}
	recipient := getStringField(session.ProcessorState, "recipient")
	if recipient == "" {
		return nil, fmt.Errorf("%w: recipient missing from session", ErrCheckoutSessionValidation)
	}

	return &solanamodule.PaymentTransactionBuildRequest{
		UserID:      session.TenantSubjectID.String(),
		PriceID:     session.PriceID,
		TokenSymbol: tokenSymbol,
		UserWallet:  account,
		Reference:   session.Reference,
		TokenAmount: tokenAmount,
		TokenMint:   tokenMint,
		Recipient:   recipient,
		Amount:      session.Amount,
		Currency:    session.Currency,
	}, nil
}

// bindSolanaLifecycleReference ensures the session has a durable Solana Pay
// reference + bound payer before a lifecycle tx is returned, so the reference
// poller can recover the session via GetByReference and the build is bound to
// one wallet. The bound wallet must equal the subscription's on-chain subscriber
// wallet — only that wallet can sign the cancel / tier-change. Returns the
// reference string to inject into the tx.
func (s *CheckoutSessionService) bindSolanaLifecycleReference(ctx context.Context, session *models.CheckoutSession, account string) (string, error) {
	if session.ProcessorState == nil {
		session.ProcessorState = map[string]any{}
	}
	// The signer must be the subscription's subscriber wallet (it owns the
	// SubscriptionAuthority / fee payer slot). Reject a mismatched wallet up front.
	if subscriber := strings.TrimSpace(getStringField(session.ProcessorState, "subscriber_wallet")); subscriber != "" && subscriber != account {
		return "", fmt.Errorf("%w: account is not the subscriber wallet for this subscription", ErrCheckoutSessionConflict)
	}
	if existingPayer := strings.TrimSpace(getStringField(session.ProcessorState, "payer")); existingPayer != "" && existingPayer != account {
		return "", fmt.Errorf("%w: solana checkout session is already bound to a different payer", ErrCheckoutSessionConflict)
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		reference, err := solana.GenerateReference()
		if err != nil {
			return "", fmt.Errorf("failed to generate reference: %w", err)
		}
		session.Reference = &reference
	}
	session.ProcessorState["payer"] = account
	if err := s.repo.BindSolanaTransactionRequest(ctx, session, account, s.now()); err != nil {
		return "", fmt.Errorf("%w: %v", ErrCheckoutSessionConflict, err)
	}
	return strings.TrimSpace(*session.Reference), nil
}

// buildSolanaSubscribeTransaction builds the RECURRING subscribe tx for a Solana
// Pay (transaction-request) subscribe session. The decision to be here is purely
// price-driven (mode==subscription, resolved from the price's recurring config) —
// there is no client-supplied one-off override. It binds the Solana Pay reference
// + payer to the POSTed account, calls PrepareSubscribeService for that wallet
// with the reference injected, and tracks the init→subscribe step on
// ProcessorState so a first-timer's second POST (after init lands) returns the
// subscribe tx. The poller mirrors the confirmed subscribe via ConfirmEnrollment.
func (s *CheckoutSessionService) buildSolanaSubscribeTransaction(ctx context.Context, session *models.CheckoutSession, account string) (*solanamodule.PayTransactionResponse, error) {
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return nil, fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutSessionValidation)
	}
	if session.ProcessorState == nil {
		session.ProcessorState = map[string]any{}
	}
	// The subscribe Solana Pay session is bound to the first wallet that POSTs its
	// account (it becomes the subscriber + fee payer). A second, different wallet is
	// rejected so the QR can't be hijacked mid-flow.
	if existingPayer := strings.TrimSpace(getStringField(session.ProcessorState, "payer")); existingPayer != "" && existingPayer != account {
		return nil, fmt.Errorf("%w: solana checkout session is already bound to a different payer", ErrCheckoutSessionConflict)
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		reference, err := solana.GenerateReference()
		if err != nil {
			return nil, fmt.Errorf("failed to generate reference: %w", err)
		}
		session.Reference = &reference
	}
	reference := strings.TrimSpace(*session.Reference)
	session.ProcessorState["payer"] = account
	session.ProcessorState["subscriber_wallet"] = account
	if err := s.repo.BindSolanaTransactionRequest(ctx, session, account, s.now()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckoutSessionConflict, err)
	}

	terms := solanaPlanTerms{
		planID:     getUint64Field(session.ProcessorState, "plan_id"),
		mintSymbol: getStringField(session.ProcessorState, "mint_symbol"),
		amount:     getUint64Field(session.ProcessorState, "amount_base_units"),
		period:     getUint64Field(session.ProcessorState, "period_hours"),
	}
	if v := strings.TrimSpace(getStringField(session.ProcessorState, "plan_created_at")); v != "" {
		terms.createdAt, _ = strconv.ParseInt(v, 10, 64)
	}

	res, err := s.solanaPrepareSubscribe.Prepare(ctx, recurring.PrepareSubscribeInput{
		TenantID:         tenant.FromContextOrDefault(ctx),
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
		return nil, fmt.Errorf("%w: no subscribe transaction produced", ErrCheckoutSessionConflict)
	}

	// Track the step so the poller knows whether the landed tx was init (advance,
	// stay pending) or subscribe (enroll). A first-timer's second POST re-derives
	// the step from on-chain authority state (now "subscribe" once init landed).
	session.ProcessorState["subscribe_step"] = res.Step
	session.ProcessorState["subscription_pda"] = res.SubscriptionPDA
	session.UpdatedAt = s.now()
	if err := s.repo.Update(ctx, session); err != nil {
		return nil, err
	}

	message := "Sign to start your subscription"
	if res.Step == "init" {
		message = "Sign to initialize your subscription (one more signature follows)"
	}
	return &solanamodule.PayTransactionResponse{
		TransactionBase64: res.Transactions[0],
		Message:           message,
	}, nil
}

// buildSolanaCancelTransaction builds the unsigned cancel_subscription tx (with
// the Solana Pay reference attached) for the wallet to sign + send. The poller
// mirrors the confirmed cancel.
func (s *CheckoutSessionService) buildSolanaCancelTransaction(ctx context.Context, session *models.CheckoutSession, account string) (*solanamodule.PayTransactionResponse, error) {
	if s.solanaPrepareCancel == nil {
		return nil, fmt.Errorf("%w: solana cancel is not configured", ErrCheckoutSessionValidation)
	}
	subscriptionID, err := uuid.Parse(strings.TrimSpace(getStringField(session.ProcessorState, "subscription_id")))
	if err != nil {
		return nil, fmt.Errorf("%w: subscription_id missing from session", ErrCheckoutSessionValidation)
	}
	reference, err := s.bindSolanaLifecycleReference(ctx, session, account)
	if err != nil {
		return nil, err
	}
	res, err := s.solanaPrepareCancel.PrepareWithReference(ctx, subscriptionID, reference)
	if err != nil {
		return nil, err
	}
	return &solanamodule.PayTransactionResponse{
		TransactionBase64: res.Transaction,
		Message:           "Sign to cancel your subscription",
	}, nil
}

// buildSolanaTierChangeTransaction builds the atomic tier-change tx (with the
// Solana Pay reference attached) for the wallet to sign + send. For an upgrade
// the cranker has co-signed the prorated transfer slot. The poller mirrors the
// confirmed switch.
func (s *CheckoutSessionService) buildSolanaTierChangeTransaction(ctx context.Context, session *models.CheckoutSession, account string) (*solanamodule.PayTransactionResponse, error) {
	if s.solanaPrepareTierChange == nil {
		return nil, fmt.Errorf("%w: solana tier change is not configured", ErrCheckoutSessionValidation)
	}
	in, err := s.tierChangePrepareInput(ctx, session)
	if err != nil {
		return nil, err
	}
	reference, err := s.bindSolanaLifecycleReference(ctx, session, account)
	if err != nil {
		return nil, err
	}
	in.Reference = reference
	res, err := s.solanaPrepareTierChange.Prepare(ctx, in)
	if err != nil {
		return nil, err
	}
	return &solanamodule.PayTransactionResponse{
		TransactionBase64: res.Transaction,
		Message:           "Sign to change your subscription tier",
	}, nil
}

// tierChangePrepareInput reconstructs the prepare input from the persisted
// lifecycle state. The old on-chain identifiers are loaded from the stored row;
// the new terms are the canonical values stamped at create time.
func (s *CheckoutSessionService) tierChangePrepareInput(ctx context.Context, session *models.CheckoutSession) (recurring.PrepareTierChangeInput, error) {
	var in recurring.PrepareTierChangeInput
	if s.solanaSubscriptionRows == nil {
		return in, fmt.Errorf("%w: solana subscription lifecycle is not configured", ErrCheckoutSessionValidation)
	}
	subscriptionID, err := uuid.Parse(strings.TrimSpace(getStringField(session.ProcessorState, "subscription_id")))
	if err != nil {
		return in, fmt.Errorf("%w: subscription_id missing from session", ErrCheckoutSessionValidation)
	}
	oldRow, err := s.solanaSubscriptionRows.GetBySubscriptionID(ctx, subscriptionID)
	if err != nil || oldRow == nil {
		return in, fmt.Errorf("%w: no on-chain record for this subscription", ErrCheckoutSessionValidation)
	}
	newPlanCreatedAt, safeErr := safecast.Convert[int64](getUint64Field(session.ProcessorState, "tier_new_plan_created_at"))
	if safeErr != nil {
		return in, fmt.Errorf("%w: tier_new_plan_created_at overflows int64", ErrCheckoutSessionValidation)
	}
	in = recurring.PrepareTierChangeInput{
		TenantID:             tenant.FromContextOrDefault(ctx),
		SubscriberWallet:     oldRow.SubscriberWallet,
		MintSymbol:           getStringField(session.ProcessorState, "tier_new_mint_symbol"),
		OldPlanPDA:           oldRow.PlanPDA,
		OldSubscriptionPDA:   oldRow.SubscriptionPDA,
		NewPlanID:            getUint64Field(session.ProcessorState, "tier_new_plan_id"),
		NewAmountBaseUnits:   getUint64Field(session.ProcessorState, "tier_new_amount_base_units"),
		NewPeriodHours:       getUint64Field(session.ProcessorState, "tier_new_period_hours"),
		NewPlanCreatedAt:     newPlanCreatedAt,
		IsUpgrade:            getBoolField(session.ProcessorState, "tier_is_upgrade"),
		FirstChargeBaseUnits: getUint64Field(session.ProcessorState, "tier_first_charge_base_units"),
	}
	return in, nil
}

// ConfirmSolanaLifecycleSession mirrors a confirmed on-chain cancel / tier-change
// for a lifecycle Solana Pay session, then marks the session succeeded. Called by
// the reference poller when it detects the reference-tagged tx has landed.
// Idempotent: a re-confirm of an already-succeeded session is a no-op, and the
// underlying ConfirmCancel / ConfirmTierChange mirrors are themselves idempotent.
func (s *CheckoutSessionService) ConfirmSolanaLifecycleSession(ctx context.Context, sessionID uuid.UUID, signature string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if session.Status == models.CheckoutSessionStatusSucceeded {
		return nil
	}
	subscriptionID, err := uuid.Parse(strings.TrimSpace(getStringField(session.ProcessorState, "subscription_id")))
	if err != nil {
		return fmt.Errorf("%w: subscription_id missing from session", ErrCheckoutSessionValidation)
	}
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return fmt.Errorf("%w: signature is required", ErrCheckoutSessionValidation)
	}

	switch session.Mode {
	case models.CheckoutSessionModeSolanaCancel:
		if s.solanaConfirmCancel == nil {
			return fmt.Errorf("%w: solana cancel is not configured", ErrCheckoutSessionValidation)
		}
		if err := s.solanaConfirmCancel.Confirm(ctx, subscriptionID, signature); err != nil {
			return err
		}
		return s.MarkSucceededWithSubscription(ctx, session.ID, uuid.Nil, signature, subscriptionID)
	case models.CheckoutSessionModeSolanaTierChange:
		if s.solanaConfirmTierChange == nil {
			return fmt.Errorf("%w: solana tier change is not configured", ErrCheckoutSessionValidation)
		}
		in, err := s.tierChangeConfirmInput(ctx, session, subscriptionID, signature)
		if err != nil {
			return err
		}
		res, err := s.solanaConfirmTierChange.Confirm(ctx, in)
		if err != nil {
			return err
		}
		newSubID := uuid.Nil
		if res != nil && res.NewSubscription != nil {
			newSubID = res.NewSubscription.ID
		}
		return s.MarkSucceededWithSubscription(ctx, session.ID, uuid.Nil, signature, newSubID)
	default:
		return fmt.Errorf("%w: not a solana lifecycle session", ErrCheckoutSessionValidation)
	}
}

// ConfirmSolanaSubscribeSession completes a RECURRING subscribe Solana Pay
// session when the reference poller detects a confirmed reference-tagged tx. The
// session's init tx and atomic [subscribe+transfer] tx share the SAME reference,
// so this is step-aware:
//   - if the SubscriptionAuthority does not yet exist on-chain, only the init tx
//     (or nothing) has landed → return ErrSolanaSubscribePending so the poller
//     keeps the reference alive and re-polls for the subscribe tx;
//   - once the authority exists AND the subscription PDA is funded (the atomic
//     [subscribe+transfer] landed), ConfirmEnrollment creates the membership +
//     persists the on-chain row, and the session is marked succeeded.
//
// Idempotent: a re-confirm of an already-succeeded session is a no-op, and
// ConfirmEnrollment upserts on the processor subscription id.
func (s *CheckoutSessionService) ConfirmSolanaSubscribeSession(ctx context.Context, sessionID uuid.UUID, signature string) error {
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutSessionValidation)
	}
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if session.Status == models.CheckoutSessionStatusSucceeded {
		return nil
	}
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return fmt.Errorf("%w: signature is required", ErrCheckoutSessionValidation)
	}

	wallet := strings.TrimSpace(getStringField(session.ProcessorState, "subscriber_wallet"))
	if wallet == "" {
		// No wallet has POSTed yet — there is nothing to confirm.
		return solanamodule.ErrSolanaSubscribePending
	}
	terms := solanaPlanTerms{
		planID:     getUint64Field(session.ProcessorState, "plan_id"),
		mintSymbol: getStringField(session.ProcessorState, "mint_symbol"),
		amount:     getUint64Field(session.ProcessorState, "amount_base_units"),
		period:     getUint64Field(session.ProcessorState, "period_hours"),
	}
	if v := strings.TrimSpace(getStringField(session.ProcessorState, "plan_created_at")); v != "" {
		terms.createdAt, _ = strconv.ParseInt(v, 10, 64)
	}
	tenantID := tenant.FromContextOrDefault(ctx)

	// Gate on authority existence: if Prepare still returns the init step, the
	// authority is not on-chain yet (init has not landed / not visible) → the
	// subscribe tx cannot have landed either. Stay pending.
	prep, err := s.solanaPrepareSubscribe.Prepare(ctx, recurring.PrepareSubscribeInput{
		TenantID:         tenantID,
		SubscriberWallet: wallet,
		PlanID:           terms.planID,
		MintSymbol:       terms.mintSymbol,
		AmountBaseUnits:  terms.amount,
		PeriodHours:      terms.period,
		PlanCreatedAt:    terms.createdAt,
	})
	if err != nil {
		return err
	}
	if prep.Step != "subscribe" {
		// Authority not yet visible → only the init tx (at most) has landed.
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["subscribe_step"] = prep.Step
		session.UpdatedAt = s.now()
		_ = s.repo.Update(ctx, session)
		return solanamodule.ErrSolanaSubscribePending
	}

	// Authority exists. ConfirmEnrollment verifies the subscription PDA is funded
	// (the atomic [subscribe+transfer] landed) before creating the membership; if it
	// is not funded yet the subscribe tx has not landed → stay pending.
	sub, err := s.solanaEnroll.ConfirmEnrollment(ctx, recurring.EnrollInput{
		TenantID:         tenantID,
		UserID:           session.TenantSubjectID.String(),
		PriceID:          session.PriceID,
		SubscriberWallet: wallet,
		PlanID:           terms.planID,
		MintSymbol:       terms.mintSymbol,
		AmountBaseUnits:  terms.amount,
		PeriodHours:      terms.period,
		PlanCreatedAt:    terms.createdAt,
		FiatAmount:       session.Amount,
		Currency:         session.Currency,
		Signature:        signature,
	})
	if err != nil {
		// The subscription PDA is not funded yet (subscribe tx not landed/visible):
		// treat as still-pending so the poller re-checks rather than failing.
		if isSolanaSubscribeNotLandedErr(err) {
			return solanamodule.ErrSolanaSubscribePending
		}
		return err
	}

	return s.MarkSucceededWithSubscription(ctx, session.ID, uuid.Nil, signature, sub.ID)
}

// isSolanaSubscribeNotLandedErr reports whether a ConfirmEnrollment error means
// the atomic subscribe bundle has not landed / is not yet visible on-chain (the
// subscription PDA is unfunded), as opposed to a genuine failure. Such an error
// is an in-progress state for the Solana Pay subscribe poller, not a terminal
// failure.
func isSolanaSubscribeNotLandedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not found on-chain")
}

// tierChangeConfirmInput reconstructs the confirm input from the persisted
// lifecycle state + the prepare-derived new subscription PDA (re-derived
// server-side, never trusting client input).
func (s *CheckoutSessionService) tierChangeConfirmInput(ctx context.Context, session *models.CheckoutSession, subscriptionID uuid.UUID, signature string) (recurring.ConfirmTierChangeInput, error) {
	var out recurring.ConfirmTierChangeInput
	prepIn, err := s.tierChangePrepareInput(ctx, session)
	if err != nil {
		return out, err
	}
	prep, err := s.solanaPrepareTierChange.Prepare(ctx, prepIn)
	if err != nil {
		return out, err
	}
	newPriceID, err := uuid.Parse(strings.TrimSpace(getStringField(session.ProcessorState, "new_price_id")))
	if err != nil {
		return out, fmt.Errorf("%w: new_price_id missing from session", ErrCheckoutSessionValidation)
	}
	var oldPeriodEnds *time.Time
	if v := strings.TrimSpace(getStringField(session.ProcessorState, "tier_old_period_ends_at")); v != "" {
		if t, perr := time.Parse(time.RFC3339, v); perr == nil {
			tt := t.UTC()
			oldPeriodEnds = &tt
		}
	}

	out = recurring.ConfirmTierChangeInput{
		Signature:            signature,
		OldSubscriptionID:    subscriptionID,
		UserID:               session.TenantSubjectID.String(),
		NewPriceID:           newPriceID,
		NewSubscriptionPDA:   prep.NewSubscriptionPDA,
		NewPlanID:            prepIn.NewPlanID,
		NewMintSymbol:        prepIn.MintSymbol,
		NewAmountBaseUnits:   prepIn.NewAmountBaseUnits,
		NewPeriodHours:       prepIn.NewPeriodHours,
		NewPlanCreatedAt:     prepIn.NewPlanCreatedAt,
		NewFiatAmount:        session.Amount,
		NewCurrency:          session.Currency,
		IsUpgrade:            prepIn.IsUpgrade,
		FirstChargeBaseUnits: prepIn.FirstChargeBaseUnits,
		OldPeriodEndsAt:      oldPeriodEnds,
	}
	return out, nil
}

func getBoolField(fields map[string]any, key string) bool {
	if fields == nil {
		return false
	}
	switch v := fields[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}
