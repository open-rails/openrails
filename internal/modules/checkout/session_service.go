package checkout

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	checkoutSessionIdempotencyOp  = "checkout_session_create"
	checkoutSessionFingerprintKey = "_openrails_request_fingerprint"
	checkoutSessionPendingLease   = 30 * time.Second
	defaultCheckoutSessionTTL     = 15 * time.Minute
	redirectCheckoutSessionTTL    = 24 * time.Hour
)

// Idempotency types are the idempotency module's own (#666 — the copy that
// lived here, plus the payments adapter that translated between the two, is
// gone; checkout consumes the module's types directly).
type IdempotencyStatus = idempotency.IdempotencyStatus

const (
	IdempotencyStatusPending = idempotency.IdempotencyStatusPending
	IdempotencyStatusSuccess = idempotency.IdempotencyStatusSuccess
	IdempotencyStatusFailed  = idempotency.IdempotencyStatusFailed
)

type IdempotencyRecord = idempotency.IdempotencyRecord

type checkoutSessionIdempotencyResult struct {
	RequestFingerprint string                   `json:"request_fingerprint"`
	Response           *CheckoutSessionResponse `json:"response"`
}

type sessionIdempotencyStore interface {
	Begin(ctx context.Context, operation, key string) (*IdempotencyRecord, bool, error)
	TryTakeoverPending(ctx context.Context, operation, key string, olderThan time.Duration) (bool, error)
	Fail(ctx context.Context, operation, key string, operationErr error) error
	Complete(ctx context.Context, operation, key string, result json.RawMessage) error
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

// railMerchantAccountIDResolver is the OPTIONAL executor capability (#704):
// resolve the active provider account for new work on a rail, or nil.
// Satisfied by *CheckoutService; test fakes may omit it.
type railMerchantAccountIDResolver interface {
	ResolvePSPID(ctx context.Context, rail string) *uuid.UUID
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
	VerifyTransactionWithContent(ctx context.Context, signature string, expectedAmount uint64, expectedRecipient string, expectedTokenMint string, expectedPayer string, expectedReference *string, expectedMemoLocalID uuid.UUID, processedNotAfter *time.Time) error
}

type CheckoutSessionService struct {
	db                       *db.DB
	repo                     *CheckoutSessionRepo
	priceService             *catalog.PriceService
	productService           *catalog.ProductService
	paymentMethodService     *paymentmethods.PaymentMethodService
	idempotencyService       sessionIdempotencyStore
	checkoutService          checkoutSessionExecutor
	solanaPayService         solanaPaymentService
	solanaTransactionService solanaTransactionService
	fxProvider               fx.Provider
	priceProvider            solanamodule.TokenPriceProvider
	config                   *config.Config
	rails                    railresolve.Source
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
// subscription (satisfied by *solanasubs.SolanaSubscriptionRepo).
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
	paymentMethodService *paymentmethods.PaymentMethodService,
	idempotencyService sessionIdempotencyStore,
	checkoutService checkoutSessionExecutor,
	solanaPayService solanaPaymentService,
	solanaTransactionService solanaTransactionService,
	fxProvider fx.Provider,
	priceProvider solanamodule.TokenPriceProvider,
	cfg *config.Config,
	rails railresolve.Source,
	clocks ...clockwork.Clock,
) *CheckoutSessionService {
	return &CheckoutSessionService{
		db:                       db,
		repo:                     NewCheckoutSessionRepo(db),
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
		rails:                    rails,
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

func (s *CheckoutSessionService) requireProviderWrites() error {
	if s == nil || s.config == nil || s.config.IsProviderReadOnly() {
		return fmt.Errorf("%w: provider writes are disabled", ErrCheckoutSessionValidation)
	}
	return nil
}

func (s *CheckoutSessionService) CreateSession(ctx context.Context, req *CheckoutSessionCreateRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return nil, fmt.Errorf("%w: user is required", ErrCheckoutSessionValidation)
	}
	if req == nil {
		return nil, fmt.Errorf("%w: request is required", ErrCheckoutSessionValidation)
	}
	if err := s.requireProviderWrites(); err != nil {
		return nil, err
	}
	if looksLikePAN(req.IdempotencyKey) {
		return nil, fmt.Errorf("%w: idempotency key contains invalid card input", ErrCheckoutSessionValidation)
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
				if s.now().Sub(rec.CreatedAt) < checkoutSessionPendingLease {
					return nil, ErrCheckoutSessionPending
				}
				taken, err := s.idempotencyService.TryTakeoverPending(
					ctx,
					checkoutSessionIdempotencyOp,
					req.IdempotencyKey,
					checkoutSessionPendingLease,
				)
				if err != nil {
					return nil, fmt.Errorf("take over stale checkout session request: %w", err)
				}
				if !taken {
					return nil, ErrCheckoutSessionPending
				}
				claimed = true
			case IdempotencyStatusFailed:
				return nil, fmt.Errorf("%w: previous request failed: %s", ErrCheckoutSessionConflict, rec.Error)
			}
		}
		claimed = true
	}

	resp, err := s.createSessionWithValidation(ctx, req, user)
	if err != nil {
		if claimed && s.idempotencyService != nil && strings.TrimSpace(req.IdempotencyKey) != "" {
			isDeterministic := errors.Is(err, ErrCheckoutSessionValidation) || errors.Is(err, ErrCheckoutSessionConflict)
			if isDeterministic {
				_ = s.idempotencyService.Fail(ctx, checkoutSessionIdempotencyOp, req.IdempotencyKey, err)
			}
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
		PriceID    string
		Mode       string
		Payment    CheckoutSessionPaymentRequest
		Metadata   map[string]string
		SuccessURL string
		CancelURL  string
	}{
		PriceID:    strings.TrimSpace(req.PriceID),
		Mode:       strings.TrimSpace(req.Mode),
		Payment:    req.Payment,
		Metadata:   normalizeMetadata(req.Metadata),
		SuccessURL: strings.TrimSpace(req.SuccessURL),
		CancelURL:  strings.TrimSpace(req.CancelURL),
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
	if err := rejectCheckoutSessionPAN(req); err != nil {
		return nil, err
	}
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
	if strings.TrimSpace(req.Payment.Rail) == "" {
		return nil, fmt.Errorf("%w: payment.rail is required", ErrCheckoutSessionValidation)
	}

	// #774: price_id accepts a price_key too.
	price, err := catalog.ResolveReference(ctx, s.priceService, req.PriceID)
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

	rail := strings.ToLower(strings.TrimSpace(req.Payment.Rail))
	if rail == "stripe" && strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency key is required for stripe checkout", ErrCheckoutSessionValidation)
	}
	mode, err := s.resolveMode(req.Mode, rail, price)
	if err != nil {
		return nil, err
	}

	if err := s.validatePayment(ctx, rail, &req.Payment, user); err != nil {
		return nil, fmt.Errorf("error validating payment: %w", err)
	}

	// #704: resolve the active provider account for the selected rail and pin it
	// on the session + into ctx so payment/subscription/payment-method rows this
	// flow creates carry psp_id provenance. nil when no
	// resolver / no active account — never invented.
	var pspID *uuid.UUID
	if resolver, ok := s.checkoutService.(railMerchantAccountIDResolver); ok {
		pspID = resolver.ResolvePSPID(ctx, rail)
	}
	if pspID != nil {
		ctx = db.WithPSPID(ctx, *pspID)
	}

	now := s.now()
	ttl := defaultCheckoutSessionTTL
	if rail == "ccbill" || rail == "stripe" {
		ttl = redirectCheckoutSessionTTL
	}
	sessionID := uuidutil.NewV7()
	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	requestFingerprint := ""
	if idempotencyKey != "" {
		sessionID = idempotentCheckoutSessionID(price.MerchantID, idempotencyKey)
		requestFingerprint = checkoutSessionRequestFingerprint(req)
	}
	railState := map[string]any{}
	if requestFingerprint != "" {
		railState[checkoutSessionFingerprintKey] = requestFingerprint
	}
	session := &models.CheckoutSession{
		ID:         sessionID,
		CustomerID: identity.CustomerIDFromString(user.ID).UUID(),
		PriceID:    price.ID,
		Mode:       mode,
		Rail:       models.Rail(rail),
		Status:     models.CheckoutSessionStatusCreated,
		Amount:     price.Amount,
		Currency:   price.Currency,
		ExpiresAt:  timePtr(now.Add(ttl)),
		Metadata:   normalizeMetadata(req.Metadata),
		RailFields: s.buildRailFields(rail, &req.Payment),
		RailState:  railState,
		CreatedAt:  now,
		UpdatedAt:  now,
		PspID:      pspID,
		LastFour:   &req.Payment.LastFour,
		CardType:   &req.Payment.CardType,
		ExpiryDate: &req.Payment.ExpiryDate,
	}

	if idempotencyKey != "" {
		session.IdempotencyKey = normalize.OptionalString(req.IdempotencyKey)
		existing, err := s.repo.GetByID(ctx, session.ID)
		if err == nil {
			return s.resumeIdempotentSession(ctx, existing, session, &req.Payment, req.SuccessURL, req.CancelURL, user)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("failed to resolve idempotent checkout session: %w", err)
		}
	}

	if err := s.repo.Create(ctx, session); err != nil {
		if idempotencyKey != "" {
			existing, getErr := s.repo.GetByID(ctx, session.ID)
			if getErr == nil {
				return s.resumeIdempotentSession(ctx, existing, session, &req.Payment, req.SuccessURL, req.CancelURL, user)
			}
		}
		return nil, fmt.Errorf("failed to create checkout session: %w", err)
	}

	if err := s.initializeSession(ctx, session, &req.Payment, req.SuccessURL, req.CancelURL, user); err != nil {
		_ = s.MarkFailed(ctx, session.ID, err.Error(), "")
		return nil, err
	}

	session.UpdatedAt = s.now()
	if err := s.repo.Update(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to update checkout session: %w", err)
	}

	return s.sessionToResponse(session), nil
}

func idempotentCheckoutSessionID(merchantID uuid.UUID, key string) uuid.UUID {
	name := "openrails:checkout-session:" + merchantID.String() + ":" + strings.TrimSpace(key)
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name))
}

func (s *CheckoutSessionService) resumeIdempotentSession(
	ctx context.Context,
	existing, requested *models.CheckoutSession,
	payment *CheckoutSessionPaymentRequest,
	successURL, cancelURL string,
	user *UserIdentity,
) (*CheckoutSessionResponse, error) {
	if existing == nil || requested == nil {
		return nil, fmt.Errorf("%w: idempotent checkout session unavailable", ErrCheckoutSessionConflict)
	}
	storedFingerprint, _ := existing.RailState[checkoutSessionFingerprintKey].(string)
	requestedFingerprint, _ := requested.RailState[checkoutSessionFingerprintKey].(string)
	parametersMatch := existing.CustomerID == requested.CustomerID &&
		existing.PriceID == requested.PriceID &&
		existing.Mode == requested.Mode &&
		existing.Rail == requested.Rail &&
		equalOptionalUUID(existing.PspID, requested.PspID) &&
		storedFingerprint != "" &&
		storedFingerprint == requestedFingerprint
	if !parametersMatch {
		return nil, fmt.Errorf("%w: idempotency key reused with different checkout session parameters", ErrCheckoutSessionConflict)
	}

	switch existing.Status {
	case models.CheckoutSessionStatusRequiresAction, models.CheckoutSessionStatusSucceeded:
		return s.sessionToResponse(existing), nil
	case models.CheckoutSessionStatusCreated, models.CheckoutSessionStatusFailed:
		existing.IdempotencyKey = requested.IdempotencyKey
		if err := s.initializeSession(ctx, existing, payment, successURL, cancelURL, user); err != nil {
			_ = s.MarkFailed(ctx, existing.ID, err.Error(), "")
			return nil, err
		}
		existing.UpdatedAt = s.now()
		if err := s.repo.Update(ctx, existing); err != nil {
			return nil, fmt.Errorf("failed to update idempotent checkout session: %w", err)
		}
		return s.sessionToResponse(existing), nil
	default:
		return nil, fmt.Errorf("%w: previous checkout session is %s", ErrCheckoutSessionConflict, existing.Status)
	}
}

func equalOptionalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (s *CheckoutSessionService) GetSession(ctx context.Context, sessionID uuid.UUID, user *UserIdentity) (*CheckoutSessionResponse, error) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return nil, ErrCheckoutSessionNotFound
	}
	if user == nil || strings.TrimSpace(user.ID) == "" || session.CustomerID.String() != user.ID {
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
	if user == nil || strings.TrimSpace(user.ID) == "" || session.CustomerID.String() != user.ID {
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
	rail := strings.ToLower(strings.TrimSpace(req.Payment.Rail))
	if rail == "" {
		return nil, fmt.Errorf("%w: payment.rail is required", ErrCheckoutSessionValidation)
	}
	if rail != strings.ToLower(string(session.Rail)) {
		return nil, fmt.Errorf("%w: rail mismatch", ErrCheckoutSessionValidation)
	}
	if s.isExpired(session) && rail != string(models.RailSolana) {
		if !s.isTerminal(session.Status) {
			_ = s.MarkExpired(ctx, session.ID, "checkout session expired")
		}
		return nil, ErrCheckoutSessionExpired
	}

	// #704: carry the session's pinned provider-account provenance into the
	// confirm flow (falling back to a fresh resolution for older sessions).
	stampID := session.PspID
	if stampID == nil {
		if resolver, ok := s.checkoutService.(railMerchantAccountIDResolver); ok {
			stampID = resolver.ResolvePSPID(ctx, rail)
		}
	}
	if stampID != nil {
		ctx = db.WithPSPID(ctx, *stampID)
	}

	switch rail {
	case "solana":
		if session.Mode == models.CheckoutSessionModeSubscription {
			return s.confirmSolanaSubscriptionSession(ctx, session, req, user)
		}
		return s.confirmSolanaSession(ctx, session, req, user)
	default:
		return nil, fmt.Errorf("%w: confirmation not implemented for rail %s", ErrCheckoutSessionConflict, rail)
	}
}

func (s *CheckoutSessionService) resolveMode(mode string, rail string, price *models.Price) (models.CheckoutSessionMode, error) {
	if rail == "" {
		return "", fmt.Errorf("%w: rail is required", ErrCheckoutSessionValidation)
	}

	trimmedMode := strings.TrimSpace(mode)
	if rail == "solana" {
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
	if price.AutoRenew {
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

// validatePayment dispatches checkout-input validation to the per-rail
// validator that owns that rail's required-input contract. Keeping each
// rail's rules in its own method (rather than a shared switch body) is what
// keeps the validation contract from drifting out of sync with what the
// rail's executor actually consumes — the drift that previously made Stripe
// demand billing fields its hosted-checkout path never reads.
func (s *CheckoutSessionService) validatePayment(ctx context.Context, rail string, payment *CheckoutSessionPaymentRequest, user *UserIdentity) error {
	switch {
	case rails.IsNMI(models.Rail(rail)):
		return s.validateNMIInput(ctx, payment, user)
	case rail == "stripe":
		return s.validateStripeInput(payment)
	case rail == "solana":
		return s.validateSolanaInput(payment)
	case rail == "ccbill":
		return s.validateCCBillInput(payment)
	default:
		return fmt.Errorf("%w: unsupported rail", ErrCheckoutSessionValidation)
	}
}

func rejectCheckoutSessionPAN(req *CheckoutSessionCreateRequest) error {
	if req == nil {
		return nil
	}
	payment := req.Payment
	if err := RejectPANShapedFields(&CheckoutRequest{
		PaymentMethodID: payment.PaymentMethodID,
		PaymentToken:    payment.PaymentToken,
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
		Metadata:        req.Metadata,
	}); err != nil {
		return fmt.Errorf("%w: invalid checkout input: %v", ErrCheckoutSessionValidation, err)
	}
	extraFields := map[string]string{
		"payment.rail":         payment.Rail,
		"payment.token_symbol": payment.TokenSymbol,
		"payment.flow":         payment.Flow,
		"payment.wallet":       payment.Wallet,
		"success_url":          req.SuccessURL,
		"cancel_url":           req.CancelURL,
		"subscription_id":      req.SubscriptionID,
		"new_price_id":         req.NewPriceID,
		"price_id":             req.PriceID,
		"mode":                 req.Mode,
	}
	if err := RejectPANShapedFields(&CheckoutRequest{Metadata: extraFields}); err != nil {
		return fmt.Errorf("%w: invalid checkout input: %v", ErrCheckoutSessionValidation, err)
	}
	return nil
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

func (s *CheckoutSessionService) initializeSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest, successURL, cancelURL string, user *UserIdentity) error {
	if session == nil {
		return fmt.Errorf("%w: session is required", ErrCheckoutSessionValidation)
	}
	if payment == nil {
		return fmt.Errorf("%w: payment is required", ErrCheckoutSessionValidation)
	}

	rail := strings.ToLower(string(session.Rail))
	// Route to rail-specific initialization based on config type detection
	// This allows adding new NMI providers via config without code changes
	switch {
	case rail == "solana":
		return s.initializeSolanaSession(ctx, session, payment)
	case rails.IsNMI(models.Rail(rail)):
		return s.initializeCheckoutSession(ctx, session, payment, successURL, cancelURL, user)
	case rail == "ccbill" || rail == "stripe":
		return s.initializeCheckoutSession(ctx, session, payment, successURL, cancelURL, user)
	default:
		return fmt.Errorf("%w: unsupported rail", ErrCheckoutSessionValidation)
	}
}

func (s *CheckoutSessionService) initializeSolanaSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest) error {
	// Recurring Solana subscription (#261): distinct from the one-off Solana Pay
	// flow — the subscriber signs init_subscription_authority + subscribe in their
	// wallet, so we return UNSIGNED transactions to sign rather than a Pay URL.
	if session.Mode == models.CheckoutSessionModeSubscription {
		return s.initializeSolanaSubscriptionSession(ctx, session, payment)
	}

	solanaProc, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails)
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

	if solanaProc.Solana == nil {
		return fmt.Errorf("%w: solana rail is not configured", ErrCheckoutSessionValidation)
	}
	tokenCfg, ok := solanaProc.Solana.Tokens[tokenSymbol]
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
		result, err := s.solanaPayService.GeneratePayment(ctx, session.CustomerID.String(), session.PriceID, tokenSymbol, &session.ID)
		if err != nil {
			return err
		}
		session.Status = models.CheckoutSessionStatusRequiresAction
		session.Reference = &result.Reference
		session.ExpiresAt = &result.ExpiresAt
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState["transaction_url"] = result.URL
		session.RailState["flow"] = flow
		session.RailState["token_symbol"] = tokenSymbol
		tokenMintValue := strings.TrimSpace(result.TokenMint)
		if tokenMintValue == "" {
			tokenMintValue = tokenMint
		}
		recipient := strings.TrimSpace(result.Recipient)
		if recipient == "" {
			return fmt.Errorf("%w: recipient missing from payment quote", ErrCheckoutSessionValidation)
		}
		session.RailState["token_mint"] = tokenMintValue
		session.RailState["recipient"] = recipient
		if err := setSolanaQuoteState(session.RailState, result.TokenUnits, result.TokenPriceUSD, result.FXRate, result.FXCurrency, result.QuotedAt, result.QuoteExpiresAt); err != nil {
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
		quote, err := solanamodule.CalculateTokenQuote(ctx, tokenSymbol, tokenCfg, moneyutil.Micros(session.Amount), session.Currency, s.fxProvider, s.priceProvider)
		if err != nil {
			return fmt.Errorf("%w: failed to calculate solana token quote: %v", ErrCheckoutSessionValidation, err)
		}
		session.Status = models.CheckoutSessionStatusRequiresAction
		expiresAt := s.now().Add(defaultCheckoutSessionTTL)
		session.ExpiresAt = &expiresAt
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState["flow"] = flow
		session.RailState["token_symbol"] = tokenSymbol
		session.RailState["token_mint"] = tokenMint
		if err := setSolanaQuoteState(session.RailState, quote.Units, quote.TokenPriceUSD, quote.FXRate, quote.FXCurrency, quote.QuotedAt, expiresAt); err != nil {
			return err
		}
		recipient, err := solanamodule.ResolveRecipientWallet(ctx, s.db, s.config)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCheckoutSessionValidation, err)
		}
		session.RailState["recipient"] = recipient
	default:
		return fmt.Errorf("%w: unsupported solana flow", ErrCheckoutSessionValidation)
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

// getStringSliceField reads a []string persisted in RailState (JSONB decodes
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
		conflict, err := s.checkoutService.CheckSubscriptionConflict(ctx, session.CustomerID.String(), price, product)
		if err != nil {
			return fmt.Errorf("%w: failed to check existing subscriptions: %v", ErrCheckoutSessionValidation, err)
		}
		if conflict != nil && conflict.Blocked {
			return fmt.Errorf("%w: %s", ErrCheckoutSessionConflict, conflict.Message)
		}
	}

	terms, err := parseSolanaPlanTerms(price.PSPLinkForRail(models.RailSolana))
	if err != nil {
		return err
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}

	res, err := s.solanaPrepareSubscribe.Prepare(ctx, recurring.PrepareSubscribeInput{
		MerchantID:       tid,
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
	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	session.RailState["flow"] = "subscription"
	session.RailState["subscriber_wallet"] = wallet
	session.RailState["plan_id"] = strconv.FormatUint(terms.planID, 10)
	session.RailState["mint_symbol"] = terms.mintSymbol
	session.RailState["amount_base_units"] = strconv.FormatUint(terms.amount, 10)
	session.RailState["period_hours"] = strconv.FormatUint(terms.period, 10)
	session.RailState["plan_created_at"] = strconv.FormatInt(terms.createdAt, 10)
	session.RailState["subscription_pda"] = res.SubscriptionPDA
	session.RailState["subscribe_step"] = res.Step
	session.RailState["sign_transactions"] = toAnySlice(res.Transactions)
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
		conflict, cerr := s.checkoutService.CheckSubscriptionConflict(ctx, session.CustomerID.String(), price, product)
		if cerr != nil {
			return fmt.Errorf("%w: failed to check existing subscriptions: %v", ErrCheckoutSessionValidation, cerr)
		}
		if conflict != nil && conflict.Blocked {
			return fmt.Errorf("%w: %s", ErrCheckoutSessionConflict, conflict.Message)
		}
	}

	terms, err := parseSolanaPlanTerms(price.PSPLinkForRail(models.RailSolana))
	if err != nil {
		return err
	}

	expiresAt := s.now().Add(defaultCheckoutSessionTTL)
	session.Status = models.CheckoutSessionStatusRequiresAction
	session.ExpiresAt = &expiresAt
	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	// flow=transaction_request is what makes sessionToResponse build the
	// solana_pay_url; the subscribe terms travel on RailState exactly like the
	// wallet path so BuildSolanaPayTransaction can re-derive the PrepareSubscribe
	// input without trusting client input. subscribe_step starts empty — the first
	// POST resolves it to "init" or "subscribe" from on-chain authority state.
	session.RailState["flow"] = "transaction_request"
	session.RailState["plan_id"] = strconv.FormatUint(terms.planID, 10)
	session.RailState["mint_symbol"] = terms.mintSymbol
	session.RailState["amount_base_units"] = strconv.FormatUint(terms.amount, 10)
	session.RailState["period_hours"] = strconv.FormatUint(terms.period, 10)
	session.RailState["plan_created_at"] = strconv.FormatInt(terms.createdAt, 10)
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
	wallet := strings.TrimSpace(getStringField(session.RailState, "subscriber_wallet"))
	if reqWallet := strings.TrimSpace(req.Payment.Wallet); reqWallet != "" && wallet != "" && reqWallet != wallet {
		return nil, fmt.Errorf("%w: wallet does not match session", ErrCheckoutSessionValidation)
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
	tenantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}

	// Step 1: the signed transaction was init_subscription_authority. Re-prepare
	// the subscribe transaction (the authority now exists) and stay in
	// requires_action so the wallet signs the second transaction.
	if getStringField(session.RailState, "subscribe_step") == "init" {
		res, err := s.solanaPrepareSubscribe.Prepare(ctx, recurring.PrepareSubscribeInput{
			MerchantID:       tenantID,
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
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState["subscribe_step"] = "subscribe"
		session.RailState["sign_transactions"] = toAnySlice(res.Transactions)
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
		MerchantID:       tenantID,
		UserID:           session.CustomerID.String(),
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
// persisted (mode + RailState carry it forward); the build/confirm paths
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
// in RailState; the public Solana Pay endpoint then builds the reference-
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

	rail := strings.ToLower(strings.TrimSpace(req.Payment.Rail))
	if rail != string(models.RailSolana) {
		return nil, fmt.Errorf("%w: %s mode requires the solana rail", ErrCheckoutSessionValidation, mode)
	}

	subscriptionID, err := api.ParseSubscriptionID(strings.TrimSpace(req.SubscriptionID))
	if err != nil {
		return nil, fmt.Errorf("%w: subscription_id is required", ErrCheckoutSessionValidation)
	}

	// Authorize: the acting user must OWN the target subscription, and it must be
	// an active Solana subscription.
	sub, err := s.subscriptionReader.GetByID(ctx, subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, fmt.Errorf("%w: subscription not found", ErrCheckoutSessionValidation)
		}
		return nil, fmt.Errorf("failed to load subscription: %w", err)
	}
	if sub == nil || sub.CustomerID.String() != user.ID {
		// Do not leak existence of someone else's subscription.
		return nil, fmt.Errorf("%w: subscription not found", ErrCheckoutSessionValidation)
	}
	if sub.Rail != models.RailSolana {
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
	session := &models.CheckoutSession{
		ID:         uuidutil.NewV7(),
		CustomerID: identity.CustomerIDFromString(user.ID).UUID(),
		PriceID:    sessionPriceID,
		Mode:       mode,
		Rail:       models.RailSolana,
		Status:     models.CheckoutSessionStatusRequiresAction,
		Amount:     sessionAmount,
		Currency:   sessionCurrency,
		ExpiresAt:  &expiresAt,
		Metadata:   normalizeMetadata(req.Metadata),
		RailFields: map[string]any{"rail": string(models.RailSolana)},
		RailState:  s.buildLifecycleState(lifecycle),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if strings.TrimSpace(req.IdempotencyKey) != "" {
		session.IdempotencyKey = normalize.OptionalString(req.IdempotencyKey)
	}

	if err := s.repo.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create checkout session: %w", err)
	}
	return s.sessionToResponse(session), nil
}

// buildLifecycleState serializes the resolved lifecycle facts onto RailState
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
	// #774: new_price_id accepts a price_key too.
	newPrice, err := catalog.ResolveReference(ctx, s.priceService, newPriceIDStr)
	if err != nil || newPrice == nil {
		return nil, fmt.Errorf("%w: target price not found", ErrCheckoutSessionValidation)
	}
	if !newPrice.IsPurchasable() {
		return nil, fmt.Errorf("%w: target price is not available", ErrCheckoutSessionValidation)
	}
	newTerms, err := parseSolanaPlanTerms(newPrice.PSPLinkForRail(models.RailSolana))
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
		newPriceID:      newPrice.ID,
		oldRow:          oldRow,
		newTerms:        newTerms,
		isUpgrade:       isUpgrade,
		oldPeriodEndsAt: oldSub.CurrentPeriodEndsAt,
	}
	if isUpgrade {
		firstChargeMicros, _ := CalculateModelBUpgradeCharge(
			oldPrice.Amount,
			newPrice.Amount,
			oldSub.CurrentPeriodEndsAt,
			newPrice.RecurringCycleHours(),
			s.now(),
		)
		firstChargeBaseUnits := solanamodule.FiatMicrosToStablecoinBaseUnits(ctx, moneyutil.Micros(firstChargeMicros), newTerms.mintSymbol, s.priceProvider)
		if firstChargeBaseUnits == 0 {
			firstChargeBaseUnits = 1
		}
		out.firstChargeBaseUnits = firstChargeBaseUnits
	}
	return out, nil
}

func (s *CheckoutSessionService) initializeCheckoutSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest, successURL, cancelURL string, user *UserIdentity) error {
	if s.checkoutService == nil {
		return fmt.Errorf("%w: checkout service unavailable", ErrCheckoutSessionValidation)
	}

	req := &CheckoutRequest{
		PriceID:           api.FormatPriceID(session.PriceID),
		PaymentMethodID:   payment.PaymentMethodID,
		PaymentToken:      payment.PaymentToken,
		Rail:              string(session.Rail),
		SuccessURL:        successURL,
		CancelURL:         cancelURL,
		Metadata:          session.Metadata,
		CheckoutStartedAt: session.CreatedAt,
		Email:             payment.Email,
		FirstName:         payment.FirstName,
		LastName:          payment.LastName,
		Address1:          payment.Address1,
		City:              payment.City,
		State:             payment.State,
		Zip:               payment.Zip,
		Country:           payment.Country,
		LastFour:          payment.LastFour,
		CardType:          payment.CardType,
		ExpiryDate:        payment.ExpiryDate,
	}

	if session.IdempotencyKey != nil {
		if key := strings.TrimSpace(*session.IdempotencyKey); key != "" {
			req.IdempotencyKey = fmt.Sprintf("checkout_session:%s", key)
		}
	}
	if session.Rail == models.RailStripe || session.Rail == models.RailCCBill {
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
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState["redirect_url"] = redirectURL
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

func (s *CheckoutSessionService) buildRailFields(rail string, payment *CheckoutSessionPaymentRequest) map[string]any {
	fields := map[string]any{
		"rail": rail,
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
			Rail: string(session.Rail),
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

	if session.RailState != nil {
		if val, ok := session.RailState["transaction_url"].(string); ok && strings.TrimSpace(val) != "" {
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
		if val, ok := session.RailState["redirect_url"].(string); ok && strings.TrimSpace(val) != "" {
			resp.URL = strings.TrimSpace(val)
			resp.Payment.RedirectURL = resp.URL
		}
		if val, ok := session.RailState["message"].(string); ok && strings.TrimSpace(val) != "" {
			resp.Message = strings.TrimSpace(val)
		} else if val, ok := session.RailState["failure_reason"].(string); ok && strings.TrimSpace(val) != "" {
			resp.Message = strings.TrimSpace(val)
		}
	}

	if action := s.buildNextAction(resp); action != nil {
		resp.NextAction = action
	}

	// Recurring Solana subscribe (#261): surface the unsigned transaction(s) to
	// sign. Takes precedence over other next_actions for a subscription session.
	if resp.Status == string(models.CheckoutSessionStatusRequiresAction) {
		if txns := getStringSliceField(session.RailState, "sign_transactions"); len(txns) > 0 {
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
	sum := sha256.Sum256([]byte(trimmedKey))
	return fmt.Sprintf("%s:%s", strings.TrimSpace(userID), hex.EncodeToString(sum[:]))
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
	if session == nil || session.Rail != models.RailSolana {
		return false
	}
	switch session.Mode {
	case models.CheckoutSessionModeSolanaCancel, models.CheckoutSessionModeSolanaTierChange:
		return true
	}
	if flow, ok := session.RailState["flow"].(string); ok && flow == "transaction_request" {
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
	solanaProc, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckoutSessionValidation, err)
	}

	// Get token symbol from RailState (where initializeSolanaSession stores it)
	tokenSymbol := strings.ToUpper(strings.TrimSpace(getStringField(session.RailState, "token_symbol")))
	if tokenSymbol == "" {
		return nil, fmt.Errorf("%w: token_symbol missing", ErrCheckoutSessionValidation)
	}

	if solanaProc.Solana == nil {
		return nil, fmt.Errorf("%w: solana rail is not configured", ErrCheckoutSessionValidation)
	}
	tokenCfg, ok := solanaProc.Solana.Tokens[tokenSymbol]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported token", ErrCheckoutSessionValidation)
	}
	tokenMint := tokenCfg.Mint
	storedTokenMint := getStringField(session.RailState, "token_mint")
	if storedTokenMint == "" {
		return nil, fmt.Errorf("%w: token_mint missing", ErrCheckoutSessionValidation)
	}
	if !strings.EqualFold(tokenSymbol, "SOL") && solanamodule.IsNativeSOLMint(storedTokenMint) {
		return nil, fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrCheckoutSessionValidation)
	}
	if !strings.EqualFold(storedTokenMint, tokenMint) {
		return nil, fmt.Errorf("%w: token_mint mismatch", ErrCheckoutSessionValidation)
	}

	expectedAmount := getUint64Field(session.RailState, "token_amount")
	if expectedAmount == 0 {
		return nil, fmt.Errorf("%w: token_amount missing or invalid", ErrCheckoutSessionValidation)
	}
	expectedRecipient := getStringField(session.RailState, "recipient")
	if expectedRecipient == "" {
		return nil, fmt.Errorf("%w: recipient missing", ErrCheckoutSessionValidation)
	}
	// Get payer from RailState (set by BuildSolanaPayTransaction)
	expectedPayer := strings.TrimSpace(getStringField(session.RailState, "payer"))
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
		session.ID, // #713: a present purchase memo must name THIS session; absence passes
		session.ExpiresAt,
	); err != nil {
		return nil, err
	}

	signature := strings.TrimSpace(req.Payment.Signature)
	if s.db != nil {
		if existingPayment, err := payments.NewPaymentRepo(s.db).GetByTransactionID(ctx, models.RailSolana, signature); err == nil {
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
		} else if !db.IsNotFound(err) {
			return nil, fmt.Errorf("failed checking existing solana payment: %w", err)
		}
	}

	result, err := s.checkoutService.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        session.CustomerID.String(),
		PriceID:       session.PriceID,
		Rail:          "solana",
		TransactionID: signature,
		Amount:        session.Amount,
		Currency:      session.Currency,
		Metadata: map[string]any{
			"solana_reference":    referenceValue,
			"checkout_session_id": session.ID.String(),
			"solana_payer_wallet": checkoutStateString(session.RailState, "payer"),
			"solana_token_symbol": checkoutStateString(session.RailState, "token_symbol"),
			"solana_token_mint":   checkoutStateString(session.RailState, "token_mint"),
			"solana_token_amount": checkoutStateUint64(session.RailState, "token_amount"),
			"solana_recipient":    checkoutStateString(session.RailState, "recipient"),
		},
		AttemptKind: payments.AttemptInitial,
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
	payment, err := payments.NewPaymentRepo(s.db).GetByID(ctx, paymentID)
	if err != nil {
		return fmt.Errorf("failed to verify registered solana payment: %w", err)
	}
	return validateSolanaPaymentMatchesSession(payment, session, reference)
}

func validateSolanaPaymentMatchesSession(payment *models.Payment, session *models.CheckoutSession, reference string) error {
	if payment == nil || session == nil {
		return fmt.Errorf("%w: solana payment does not match checkout session", ErrCheckoutSessionConflict)
	}
	if payment.CustomerID.String() != session.CustomerID.String() || payment.PriceID != session.PriceID || payment.Amount != session.Amount || !strings.EqualFold(payment.Currency, session.Currency) {
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
		if session.Status == models.CheckoutSessionStatusExpired && session.Rail == models.RailSolana && paymentID != uuid.Nil && strings.TrimSpace(transactionID) != "" {
			// A wallet may broadcast before expiry but the app may confirm after expiry.
			// The caller has already verified the signature against the session-bound quote.
		} else {
			return ErrCheckoutSessionConflict
		}
	}
	if s.isExpired(session) && session.Rail != models.RailSolana {
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
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState["message"] = msg
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

func (s *CheckoutSessionService) FindOpenByUserPriceRail(ctx context.Context, userID string, priceID uuid.UUID, rail models.Rail) (*models.CheckoutSession, error) {
	if s.repo == nil {
		return nil, ErrCheckoutSessionNotFound
	}
	session, err := s.repo.GetLatestOpenByUserPriceRail(ctx, userID, priceID, rail)
	if err != nil {
		if db.IsNotFound(err) {
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
	if session.CustomerID.String() != userID || session.PriceID != priceID || session.Rail != models.RailCCBill {
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
	flow := strings.ToLower(strings.TrimSpace(getStringField(session.RailState, "flow")))
	if flow == "" {
		// Legacy default for Solana checkout sessions.
		return true
	}
	return flow == "transfer_request"
}

func (s *CheckoutSessionService) finalizeSolanaTransferReference(ctx context.Context, session *models.CheckoutSession, transactionID string) error {
	if session == nil || session.Rail != models.RailSolana {
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

func setSolanaQuoteState(railState map[string]any, tokenAmount uint64, tokenPriceUSD, fxRate float64, fxCurrency string, quotedAt, quoteExpiresAt time.Time) error {
	if railState == nil {
		return fmt.Errorf("%w: rail_state unavailable", ErrCheckoutSessionValidation)
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

	railState["token_amount"] = tokenAmount
	railState["token_price_usd"] = tokenPriceUSD
	railState["fx_rate"] = fxRate
	railState["fx_currency"] = strings.TrimSpace(fxCurrency)
	railState["quoted_at"] = quotedAt.UTC().Format(time.RFC3339)
	railState["quote_expires_at"] = quoteExpiresAt.UTC().Format(time.RFC3339)

	return nil
}

func checkoutStateString(state map[string]any, key string) string {
	if state == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(state[key]))
}

func checkoutStateUint64(state map[string]any, key string) uint64 {
	if state == nil {
		return 0
	}
	switch v := state[key].(type) {
	case uint64:
		return v
	case uint:
		return uint64(v)
	case int:
		if v > 0 {
			return uint64(v)
		}
	case int64:
		if v > 0 {
			return uint64(v)
		}
	case float64:
		if v > 0 {
			return uint64(v)
		}
	}
	return 0
}

// GetSessionForSolanaPay retrieves and validates a checkout session for Solana Pay spec endpoints.
// Returns session info needed for GET endpoint or an error if the session is invalid.
func (s *CheckoutSessionService) GetSessionForSolanaPay(ctx context.Context, sessionID uuid.UUID) (*solanamodule.PaySessionInfo, error) {
	if s.repo == nil {
		return nil, ErrCheckoutSessionNotFound
	}

	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, ErrCheckoutSessionNotFound
		}
		return nil, err
	}
	// Validate it's a Solana session
	if session.Rail != models.RailSolana {
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
		productName = solanaLifecycleLabel("Cancel", productName, session.RailState)
	case models.CheckoutSessionModeSolanaTierChange:
		productName = solanaLifecycleLabel("Change to", productName, session.RailState)
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
	if err := s.requireProviderWrites(); err != nil {
		return nil, err
	}
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
		if db.IsNotFound(err) {
			return nil, ErrCheckoutSessionNotFound
		}
		return nil, err
	}
	// Validate it's a Solana session
	if session.Rail != models.RailSolana {
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

	// Get token symbol from rail state
	tokenSymbol := getStringField(session.RailState, "token_symbol")
	if tokenSymbol == "" {
		return nil, fmt.Errorf("%w: token_symbol missing from session", ErrCheckoutSessionValidation)
	}

	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	if existingPayer := strings.TrimSpace(getStringField(session.RailState, "payer")); existingPayer != "" && existingPayer != account {
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
	session.RailState["payer"] = account
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
	tokenAmount := getUint64Field(session.RailState, "token_amount")
	if tokenAmount == 0 {
		return nil, fmt.Errorf("%w: token_amount missing from session", ErrCheckoutSessionValidation)
	}
	tokenMint := getStringField(session.RailState, "token_mint")
	if tokenMint == "" {
		return nil, fmt.Errorf("%w: token_mint missing from session", ErrCheckoutSessionValidation)
	}
	if !strings.EqualFold(tokenSymbol, "SOL") && solanamodule.IsNativeSOLMint(tokenMint) {
		return nil, fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrCheckoutSessionValidation)
	}
	recipient := getStringField(session.RailState, "recipient")
	if recipient == "" {
		return nil, fmt.Errorf("%w: recipient missing from session", ErrCheckoutSessionValidation)
	}

	return &solanamodule.PaymentTransactionBuildRequest{
		UserID:      session.CustomerID.String(),
		PriceID:     session.PriceID,
		TokenSymbol: tokenSymbol,
		UserWallet:  account,
		Reference:   session.Reference,
		TokenAmount: tokenAmount,
		TokenMint:   tokenMint,
		Recipient:   recipient,
		Amount:      session.Amount,
		Currency:    session.Currency,
		SessionID:   session.ID, // #713 memo local-id
	}, nil
}

// bindSolanaLifecycleReference ensures the session has a durable Solana Pay
// reference + bound payer before a lifecycle tx is returned, so the reference
// poller can recover the session via GetByReference and the build is bound to
// one wallet. The bound wallet must equal the subscription's on-chain subscriber
// wallet — only that wallet can sign the cancel / tier-change. Returns the
// reference string to inject into the tx.
func (s *CheckoutSessionService) bindSolanaLifecycleReference(ctx context.Context, session *models.CheckoutSession, account string) (string, error) {
	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	// The signer must be the subscription's subscriber wallet (it owns the
	// SubscriptionAuthority / fee payer slot). Reject a mismatched wallet up front.
	if subscriber := strings.TrimSpace(getStringField(session.RailState, "subscriber_wallet")); subscriber != "" && subscriber != account {
		return "", fmt.Errorf("%w: account is not the subscriber wallet for this subscription", ErrCheckoutSessionConflict)
	}
	if existingPayer := strings.TrimSpace(getStringField(session.RailState, "payer")); existingPayer != "" && existingPayer != account {
		return "", fmt.Errorf("%w: solana checkout session is already bound to a different payer", ErrCheckoutSessionConflict)
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		reference, err := solana.GenerateReference()
		if err != nil {
			return "", fmt.Errorf("failed to generate reference: %w", err)
		}
		session.Reference = &reference
	}
	session.RailState["payer"] = account
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
// RailState so a first-timer's second POST (after init lands) returns the
// subscribe tx. The poller mirrors the confirmed subscribe via ConfirmEnrollment.
func (s *CheckoutSessionService) buildSolanaSubscribeTransaction(ctx context.Context, session *models.CheckoutSession, account string) (*solanamodule.PayTransactionResponse, error) {
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return nil, fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutSessionValidation)
	}
	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	// The subscribe Solana Pay session is bound to the first wallet that POSTs its
	// account (it becomes the subscriber + fee payer). A second, different wallet is
	// rejected so the QR can't be hijacked mid-flow.
	if existingPayer := strings.TrimSpace(getStringField(session.RailState, "payer")); existingPayer != "" && existingPayer != account {
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
	session.RailState["payer"] = account
	session.RailState["subscriber_wallet"] = account
	if err := s.repo.BindSolanaTransactionRequest(ctx, session, account, s.now()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckoutSessionConflict, err)
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
		return nil, fmt.Errorf("%w: no subscribe transaction produced", ErrCheckoutSessionConflict)
	}

	// Track the step so the poller knows whether the landed tx was init (advance,
	// stay pending) or subscribe (enroll). A first-timer's second POST re-derives
	// the step from on-chain authority state (now "subscribe" once init landed).
	session.RailState["subscribe_step"] = res.Step
	session.RailState["subscription_pda"] = res.SubscriptionPDA
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
	subscriptionID, err := uuid.Parse(strings.TrimSpace(getStringField(session.RailState, "subscription_id")))
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
	subscriptionID, err := uuid.Parse(strings.TrimSpace(getStringField(session.RailState, "subscription_id")))
	if err != nil {
		return in, fmt.Errorf("%w: subscription_id missing from session", ErrCheckoutSessionValidation)
	}
	oldRow, err := s.solanaSubscriptionRows.GetBySubscriptionID(ctx, subscriptionID)
	if err != nil || oldRow == nil {
		return in, fmt.Errorf("%w: no on-chain record for this subscription", ErrCheckoutSessionValidation)
	}
	newPlanCreatedAt, safeErr := safecast.Convert[int64](getUint64Field(session.RailState, "tier_new_plan_created_at"))
	if safeErr != nil {
		return in, fmt.Errorf("%w: tier_new_plan_created_at overflows int64", ErrCheckoutSessionValidation)
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return in, err
	}
	in = recurring.PrepareTierChangeInput{
		MerchantID:           tid,
		SubscriberWallet:     oldRow.SubscriberWallet,
		MintSymbol:           getStringField(session.RailState, "tier_new_mint_symbol"),
		OldPlanPDA:           oldRow.PlanPDA,
		OldSubscriptionPDA:   oldRow.SubscriptionPDA,
		NewPlanID:            getUint64Field(session.RailState, "tier_new_plan_id"),
		NewAmountBaseUnits:   getUint64Field(session.RailState, "tier_new_amount_base_units"),
		NewPeriodHours:       getUint64Field(session.RailState, "tier_new_period_hours"),
		NewPlanCreatedAt:     newPlanCreatedAt,
		IsUpgrade:            getBoolField(session.RailState, "tier_is_upgrade"),
		FirstChargeBaseUnits: getUint64Field(session.RailState, "tier_first_charge_base_units"),
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
	subscriptionID, err := uuid.Parse(strings.TrimSpace(getStringField(session.RailState, "subscription_id")))
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
// ConfirmEnrollment upserts on the rail subscription id.
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

	wallet := strings.TrimSpace(getStringField(session.RailState, "subscriber_wallet"))
	if wallet == "" {
		// No wallet has POSTed yet — there is nothing to confirm.
		return solanamodule.ErrSolanaSubscribePending
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
	tenantID, err := merchant.Require(ctx)
	if err != nil {
		return err
	}

	// Gate on authority existence: if Prepare still returns the init step, the
	// authority is not on-chain yet (init has not landed / not visible) → the
	// subscribe tx cannot have landed either. Stay pending.
	prep, err := s.solanaPrepareSubscribe.Prepare(ctx, recurring.PrepareSubscribeInput{
		MerchantID:       tenantID,
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
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState["subscribe_step"] = prep.Step
		session.UpdatedAt = s.now()
		_ = s.repo.Update(ctx, session)
		return solanamodule.ErrSolanaSubscribePending
	}

	// Authority exists. ConfirmEnrollment verifies the subscription PDA is funded
	// (the atomic [subscribe+transfer] landed) before creating the membership; if it
	// is not funded yet the subscribe tx has not landed → stay pending.
	sub, err := s.solanaEnroll.ConfirmEnrollment(ctx, recurring.EnrollInput{
		MerchantID:       tenantID,
		UserID:           session.CustomerID.String(),
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
	newPriceID, err := uuid.Parse(strings.TrimSpace(getStringField(session.RailState, "new_price_id")))
	if err != nil {
		return out, fmt.Errorf("%w: new_price_id missing from session", ErrCheckoutSessionValidation)
	}
	var oldPeriodEnds *time.Time
	if v := strings.TrimSpace(getStringField(session.RailState, "tier_old_period_ends_at")); v != "" {
		if t, perr := time.Parse(time.RFC3339, v); perr == nil {
			tt := t.UTC()
			oldPeriodEnds = &tt
		}
	}

	out = recurring.ConfirmTierChangeInput{
		Signature:            signature,
		OldSubscriptionID:    subscriptionID,
		UserID:               session.CustomerID.String(),
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
