package checkout

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/settlement"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/cardholdername"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

const (
	checkoutAttemptIdempotencyOp  = "checkout_attempt_create"
	checkoutAttemptFingerprintKey = "_openrails_request_fingerprint"
	// checkoutAttemptPSPFieldKey persists the resolved PSP key on the session's
	// rail_fields when it differs from the rail kind, so execution (including
	// idempotent resume) lands on the requested PSP, not a re-resolved one (#848).
	checkoutAttemptPSPFieldKey = "psp"
	defaultCheckoutAttemptTTL  = 15 * time.Minute
	redirectCheckoutAttemptTTL = 24 * time.Hour
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

type checkoutAttemptIdempotencyResult struct {
	RequestFingerprint string                   `json:"request_fingerprint"`
	Response           *CheckoutAttemptResponse `json:"response"`
}

type checkoutAttemptExecutor interface {
	checkoutRailTargets
	Checkout(ctx context.Context, req *CheckoutRequest, user *UserIdentity) (*CheckoutResponse, error)
	RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error)
	// CheckSubscriptionConflict is the shared duplicate-billing guard (issue
	// #269): blocks a second non-terminal subscription in the same exact price or
	// tier-group. The Solana subscribe path runs it before preparing any
	// transaction.
	CheckSubscriptionConflict(ctx context.Context, userID string, price *models.Price, product *models.Product) (*SubscriptionConflict, error)
}

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
	GeneratePayment(ctx context.Context, userID string, priceID uuid.UUID, tokenSymbol string, sessionID *uuid.UUID) (*solanamodule.PayResult, error)
	// RegisterReference gives a transaction-request attempt its one reference
	// (the transfer-request flow registers inside GeneratePayment).
	RegisterReference(ctx context.Context, kind solanamodule.ReferenceKind, sessionID uuid.UUID, reference string, quoteExpiresAt time.Time) (gen.BillingSolanaPayReference, error)
}

type solanaTransactionService interface {
	BuildPaymentTransactionFromQuote(ctx context.Context, req *solanamodule.PaymentTransactionBuildRequest) (*solanamodule.TransactionBuildResponse, error)
	ObserveTransfer(ctx context.Context, req solana.ObserveTransferRequest) (*solana.TransferObservation, error)
	BlockHeight(ctx context.Context) (uint64, error)
	ReferenceHasOurTransfer(ctx context.Context, reference, recipient, mint string, sessionID uuid.UUID) (bool, error)
}

type CheckoutAttemptService struct {
	captureSecrets           merchants.MerchantSecretReader
	captureEncryption        captureEncryption
	db                       *db.DB
	repo                     *CheckoutAttemptRepo
	priceService             *catalog.PriceService
	productService           *catalog.ProductService
	paymentMethodService     *paymentmethods.PaymentMethodService
	idempotencyService       idempotencyStore
	checkoutService          checkoutAttemptExecutor
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
	idempotencyService idempotencyStore,
	checkoutService checkoutAttemptExecutor,
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

func (s *CheckoutAttemptService) requireProviderWrites() error {
	if s == nil || s.config == nil || config.IsProviderReadOnly(s.config) {
		return fmt.Errorf("%w: provider writes are disabled", ErrCheckoutAttemptValidation)
	}
	return nil
}

func (s *CheckoutAttemptService) CreateSession(ctx context.Context, req *CheckoutAttemptCreateRequest, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	// However the request ends, it is done with the card (#1129).
	defer describeCard(req)()
	if err := s.guardCardAttempt(ctx, user); err != nil {
		return nil, err
	}
	resp, err := s.createSession(ctx, req, user)
	s.noteCardAttempt(ctx, user, resp, err)
	return resp, err
}

func (s *CheckoutAttemptService) createSession(ctx context.Context, req *CheckoutAttemptCreateRequest, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return nil, fmt.Errorf("%w: user is required", ErrCheckoutAttemptValidation)
	}
	if req == nil {
		return nil, fmt.Errorf("%w: request is required", ErrCheckoutAttemptValidation)
	}
	if err := s.validateReturnURLs(req.SuccessURL, req.CancelURL); err != nil {
		return nil, err
	}
	if req.Mode == string(models.CheckoutAttemptModePaymentMethod) {
		return s.createPaymentMethodSetup(ctx, req, user)
	}
	if err := validateCheckoutPriceSelector(req.PriceID, req.ProductKey, req.PriceKey); err != nil {
		return nil, err
	}
	if err := s.requireProviderWrites(); err != nil {
		return nil, err
	}
	// The idempotency key is an opaque client token, but it is persisted and
	// replayed, so a card number pasted into it would land in our storage.
	if cardguard.ContainsPAN(req.IdempotencyKey) {
		return nil, fmt.Errorf("%w: idempotency key contains invalid card input", ErrCheckoutAttemptValidation)
	}
	canonicalizeCheckoutPaymentName(&req.Payment)

	req.IdempotencyKey = scopeIdempotencyKey(user.ID, req.IdempotencyKey)

	var claim *idempotency.Claim
	if s.idempotencyService != nil && strings.TrimSpace(req.IdempotencyKey) != "" {
		var rec *idempotency.Record
		var err error
		claim, rec, err = s.idempotencyService.Begin(ctx, checkoutAttemptIdempotencyOp, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if claim == nil {
			if rec.Status != idempotency.StatusSucceeded {
				return nil, ErrCheckoutAttemptPending
			}
			cached, err := decodeCheckoutAttemptIdempotencyResult(rec.Result, req, user)
			if err != nil {
				return nil, err
			}
			if cached.MembershipQuote != nil || cached.Mode == string(models.CheckoutAttemptModeOneOff) && s.db != nil {
				live, err := s.GetSession(ctx, cached.ID.UUID(), user)
				if err != nil {
					return nil, err
				}
				// The accepted quote's operation is keyed by the session, so
				// accepting again replays it.
				return s.acceptOnCreate(ctx, req, live, user)
			}
			return cached, nil
		}
		// A reclaimed claim reruns: the session row and its provider_intents are
		// keyed by this request key, so the rerun resumes them, never charges again.
	}

	work, stop := ctx, func() {}
	if claim != nil {
		// The owner works under its lease, and no transaction it opens
		// commits once the claim has passed on (#1099).
		work, stop = claim.Hold(ctx)
		work = db.WithCommitGuard(work, claim.InTx)
	}
	resp, err := s.createSessionWithValidation(work, req, user)
	if err == nil {
		resp, err = s.acceptOnCreate(work, req, resp, user)
	}
	stop() // before Complete/Fail: a renewal must never race the final state
	if claim != nil && (errors.Is(context.Cause(work), idempotency.ErrClaimLost) || errors.Is(err, idempotency.ErrClaimLost)) {
		// The lease lapsed under us: another request owns the key now.
		return nil, ErrCheckoutAttemptPending
	}
	if err != nil {
		if claim != nil {
			// Every outcome settles the claim. A replay reruns against the
			// durable session and its operation, which answer it the same way.
			failCheckoutIdempotency(ctx, claim, checkoutAttemptIdempotencyOp, req.IdempotencyKey, err)
		}
		return nil, err
	}

	if claim != nil {
		fingerprint := checkoutAttemptRequestFingerprintForRail(req, user, resp.Payment.Rail)
		payload, _ := json.Marshal(checkoutAttemptIdempotencyResult{RequestFingerprint: fingerprint, Response: resp})
		completeCheckoutIdempotency(ctx, claim, checkoutAttemptIdempotencyOp, req.IdempotencyKey, payload)
	}

	return resp, nil
}

// acceptOnCreate accepts a quoted membership in the same call when asked to.
func (s *CheckoutAttemptService) acceptOnCreate(ctx context.Context, req *CheckoutAttemptCreateRequest, resp *CheckoutAttemptResponse, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	if req.Acceptance == nil || resp == nil || resp.MembershipQuote == nil {
		return resp, nil
	}
	return s.acceptQuoteOnCreate(ctx, resp, user, *req.Acceptance)
}

func canonicalizeCheckoutPaymentName(payment *CheckoutAttemptPaymentRequest) {
	if payment == nil {
		return
	}
	full := strings.TrimSpace(payment.NameOnCard)
	if full != "" {
		payment.NameOnCard = full
		payment.FirstName, payment.LastName = cardholdername.Parts(full, "", "")
		return
	}
	payment.FirstName = strings.TrimSpace(payment.FirstName)
	payment.LastName = strings.TrimSpace(payment.LastName)
	payment.NameOnCard = cardholdername.Canonical("", payment.FirstName, payment.LastName)
}

func checkoutAttemptRequestFingerprint(req *CheckoutAttemptCreateRequest) string {
	return checkoutAttemptRequestFingerprintForRail(req, nil, "")
}

// checkoutAttemptRequestFingerprintForRail hashes the inputs the resolved rail
// actually executes. CCBill ignores browser payment.email and takes the verified
// account email from UserIdentity, so its idempotency projection must do the
// same. Other rails retain the request payload unchanged.
func checkoutAttemptRequestFingerprintForRail(req *CheckoutAttemptCreateRequest, user *UserIdentity, resolvedRail string) string {
	if req == nil {
		return ""
	}
	payment := req.Payment
	if strings.EqualFold(strings.TrimSpace(resolvedRail), string(models.RailCCBill)) {
		payment.Email = ""
		if user != nil && user.Email != nil {
			payment.Email = strings.TrimSpace(*user.Email)
		}
	}
	payload, _ := json.Marshal(struct {
		CancelAfterInitial bool `json:",omitempty"`
		PriceID            string
		PriceKey           string `json:",omitempty"`
		ProductKey         string `json:",omitempty"`
		Amount             *int64 `json:",omitempty"`
		Mode               string
		Payment            CheckoutAttemptPaymentRequest
		Metadata           map[string]string
		SuccessURL         string
		CancelURL          string
		Entitlement        string            `json:",omitempty"`
		OfferKind          billing.OfferKind `json:",omitempty"`
	}{
		CancelAfterInitial: req.AutoRenew != nil && !*req.AutoRenew,
		PriceID:            strings.TrimSpace(req.PriceID),
		PriceKey:           req.PriceKey, ProductKey: req.ProductKey, Amount: req.Amount,
		Mode:        strings.TrimSpace(req.Mode),
		Payment:     payment,
		Metadata:    normalizeMetadata(req.Metadata),
		SuccessURL:  strings.TrimSpace(req.SuccessURL),
		CancelURL:   strings.TrimSpace(req.CancelURL),
		Entitlement: req.Entitlement,
		OfferKind:   req.OfferKind,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func decodeCheckoutAttemptIdempotencyResult(payload json.RawMessage, req *CheckoutAttemptCreateRequest, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	var cached checkoutAttemptIdempotencyResult
	if err := json.Unmarshal(payload, &cached); err == nil && cached.Response != nil {
		fingerprint := checkoutAttemptRequestFingerprintForRail(req, user, cached.Response.Payment.Rail)
		if cached.RequestFingerprint != "" && fingerprint != "" && cached.RequestFingerprint != fingerprint {
			return nil, fmt.Errorf("%w: %w", ErrCheckoutAttemptConflict, billing.ErrIdempotencyKeyReused)
		}
		return cached.Response, nil
	}
	return nil, fmt.Errorf("failed to decode cached checkout attempt response")
}

func (s *CheckoutAttemptService) createSessionWithValidation(ctx context.Context, req *CheckoutAttemptCreateRequest, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	if err := rejectCheckoutAttemptPAN(req); err != nil {
		return nil, err
	}
	// The durable buyer-bound agreement wins over a moved price key, archived
	// product, changed routing policy, or a missing replay-cache entry.
	if s.db != nil && strings.TrimSpace(req.IdempotencyKey) != "" {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		existing, err := s.repo.GetByID(ctx, idempotentCheckoutAttemptID(mid.UUID(), req.IdempotencyKey))
		if err == nil {
			stored, _ := existing.RailState[checkoutAttemptFingerprintKey].(string)
			if existing.CustomerID.String() != user.ID || stored == "" || stored != checkoutAttemptRequestFingerprintForRail(req, user, string(existing.Rail)) {
				return nil, fmt.Errorf("%w: %w", ErrCheckoutAttemptConflict, billing.ErrIdempotencyKeyReused)
			}
			existing.IdempotencyKey = normalize.OptionalString(req.IdempotencyKey)
			return s.resumeIdempotentSession(db.WithPSPID(ctx, existing.PspID), existing, existing, &req.Payment, req.SuccessURL, req.CancelURL, user)
		}
		if !db.IsNotFound(err) {
			return nil, err
		}
	}

	price, err := resolveCheckoutPrice(ctx, s.priceService, req.PriceID, req.ProductKey, req.PriceKey)
	if err != nil {
		return nil, fmt.Errorf("%w: price not found", ErrCheckoutAttemptValidation)
	}
	if !price.IsPurchasable() {
		return nil, fmt.Errorf("%w: price is not active", ErrCheckoutAttemptValidation)
	}
	product, err := s.productService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("%w: product not found", ErrCheckoutAttemptValidation)
	}
	if !product.IsPurchasable() {
		return nil, fmt.Errorf("%w: product is not active", ErrCheckoutAttemptValidation)
	}
	price, err = CheckoutPriceForAmount(price, req.Amount)
	if err != nil {
		return nil, err
	}
	if err := validateOfferAssertion(price, product, req.Entitlement, req.OfferKind); err != nil {
		return nil, err
	}

	// or#288 + #848: resolve the processor ONCE, before the session exists.
	// A request that NAMES a PSP gets that PSP (the #848 wire value: PSP key
	// first, unambiguous rail-kind fallback); a request that names none is
	// routed by the merchant's policy, falling through unavailable candidates.
	// Either way this yields the rail KIND — used for dispatch and row
	// vocabulary — plus the PSP the charge must land on, and the trace that
	// explains the choice.
	rail := strings.ToLower(strings.TrimSpace(req.Payment.Rail))
	var pspID uuid.UUID
	decision, err := s.Route(ctx, RoutingInput{
		Price:    price,
		Product:  product,
		Mode:     checkoutModeForRail(price, ""),
		Country:  strings.TrimSpace(req.Payment.Country),
		Selector: rail,
	})
	if err != nil {
		var ambiguous *AmbiguousRailError
		var unknown *UnknownRailError
		if errors.As(err, &ambiguous) || errors.As(err, &unknown) || errors.Is(err, ErrNoRoutableProcessor) {
			return nil, fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
		}
		return nil, err
	}
	// payment.psp names a PSP by its key; a rail kind names none.
	if rail != "" && !strings.EqualFold(rail, decision.Target.PSP) {
		return nil, fmt.Errorf("%w: psp %q is not a PSP key; name one of the checkout options' psp", ErrCheckoutAttemptValidation, rail)
	}
	rail = decision.Target.Rail
	pspSelector := decision.Target.PSP
	routingReason := decision.Reason()
	// #704: pin provenance with the REAL resolved account — never invented.
	if decision.Target.Scope != nil {
		pspID = decision.Target.Scope.ID
	}
	// or#893: checkout_attempts.psp_id is NOT NULL. A session nobody can
	// attribute would be invisible to a PSP-scoped prune and would collide with
	// a sibling account's reference/transaction id under the nil-uuid lane the
	// 0063 uniques deleted. Refuse before anything is written.
	if pspID == uuid.Nil {
		return nil, fmt.Errorf("%w: no PSP is armed for rail %q", ErrCheckoutAttemptValidation, rail)
	}
	if req.Payment.PSPID != uuid.Nil && req.Payment.PSPID != pspID {
		return nil, fmt.Errorf("%w: PSP assertion does not match selected account", ErrCheckoutAttemptValidation)
	}
	if err := validateOrderRenewal(price, req.AutoRenew, models.Rail(rail)); err != nil {
		return nil, err
	}
	ctx = db.WithPSPID(ctx, pspID)
	price = priceForCheckoutTarget(price, decision.Target)

	if rail == "stripe" && strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency key is required for stripe checkout", ErrCheckoutAttemptValidation)
	}
	mode, err := s.resolveMode(req.Mode, rail, price)
	if err != nil {
		return nil, err
	}

	if err := s.validatePayment(ctx, rail, &req.Payment, user); err != nil {
		return nil, fmt.Errorf("error validating payment: %w", err)
	}
	// #1129: a card is admitted only by the PSP this session routed to.
	if req.Payment.Card != nil && cardEntryFor(decision.Target) != config.CardEntryServer {
		return nil, fmt.Errorf("%w: %w", ErrCheckoutAttemptValidation, paymentmethods.ErrCardEntryNotEnabled)
	}

	now := s.now()
	ttl := defaultCheckoutAttemptTTL
	if rail == "ccbill" || rail == "stripe" {
		ttl = redirectCheckoutAttemptTTL
	}
	sessionID := uuidutil.NewV7()
	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	requestFingerprint := ""
	if idempotencyKey != "" {
		sessionID = idempotentCheckoutAttemptID(price.MerchantID, idempotencyKey)
		requestFingerprint = checkoutAttemptRequestFingerprintForRail(req, user, rail)
	}
	railState := map[string]any{}
	if req.AutoRenew != nil {
		railState["auto_renew"] = *req.AutoRenew
	}
	if req.Amount != nil {
		railState["customer_selected"] = true
	}
	if req.OfferKind != "" {
		railState["requested_offer_kind"] = string(req.OfferKind)
	}
	if req.Entitlement != "" {
		railState["requested_entitlement"] = req.Entitlement
	}
	if requestFingerprint != "" {
		railState[checkoutAttemptFingerprintKey] = requestFingerprint
	}
	railFields := s.buildRailFields(rail, &req.Payment, user)
	if pspSelector != "" && pspSelector != rail {
		railFields[checkoutAttemptPSPFieldKey] = pspSelector
	}
	session := &models.CheckoutAttempt{
		ID:         sessionID,
		CustomerID: identity.CustomerIDFromString(user.ID).UUID(),
		PriceID:    new(price.ID),
		Mode:       mode,
		Rail:       models.Rail(rail),
		Status:     models.CheckoutAttemptStatusCreated,
		Amount:     new(price.Amount),
		Currency:   new(price.Currency),
		ExpiresAt:  timePtr(now.Add(ttl)),
		Metadata:   normalizeMetadata(req.Metadata),
		RailFields: railFields,
		RailState:  railState,
		// or#288: the decision trace is written with the row and never rewritten
		// (UpdateCheckoutAttempt does not name the column).
		RoutingReason: routingReason,
		CreatedAt:     now,
		UpdatedAt:     now,
		PspID:         pspID,
		LastFour:      &req.Payment.LastFour,
		CardType:      &req.Payment.CardType,
		ExpiryDate:    &req.Payment.ExpiryDate,
	}

	if idempotencyKey != "" {
		session.IdempotencyKey = normalize.OptionalString(req.IdempotencyKey)
		existing, err := s.repo.GetByID(ctx, session.ID)
		if err == nil {
			return s.resumeIdempotentSession(ctx, existing, session, &req.Payment, req.SuccessURL, req.CancelURL, user)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("failed to resolve idempotent checkout attempt: %w", err)
		}
	}

	// A saved custodian card creates a priced agreement for a later verified
	// customer action. Persist the quote with the row before returning it.
	engineEnrollment := mode == models.CheckoutAttemptModeSubscription && rails.NewSubscriptionFor(models.Rail(rail)) == rails.NewSubscriptionEngine
	// A new NMI card is vaulted first: engine memberships charge saved methods.
	var vaulted *models.PaymentMethod
	if engineEnrollment && req.Payment.PaymentMethodID == "" && (strings.TrimSpace(req.Payment.PaymentToken) != "" || req.Payment.Card != nil) && rails.IsNMI(models.Rail(rail)) {
		vaulted, err = s.vaultEnrollmentCard(ctx, &req.Payment, session, decision.Target, user)
		if err != nil {
			return nil, err
		}
		session.RailState[initialMembershipVaultedMethodKey] = vaulted.ID.String()
	}
	if engineEnrollment && (req.Payment.PaymentMethodID != "" || vaulted != nil) {
		var methodID uuid.UUID
		if vaulted != nil {
			methodID = vaulted.ID
		} else {
			parsed, err := billing.ParsePaymentMethodID(req.Payment.PaymentMethodID)
			if err != nil {
				return nil, ErrCheckoutAttemptValidation
			}
			methodID = parsed.UUID()
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			s.discardEnrollmentCard(ctx, vaulted)
			return nil, err
		}
		method, err := s.db.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: methodID})
		if err == nil {
			err = quoteInitialMembership(ctx, session, price, product, method, now)
		}
		if err != nil {
			s.discardEnrollmentCard(ctx, vaulted)
			return nil, err
		}
		session.Status = models.CheckoutAttemptStatusRequiresAction
	}

	// Engine-collected rails enroll only on a quoted saved method; an on-chain
	// plan enrolls through the subscriber's wallet below.
	if engineEnrollment {
		if _, quoted := session.RailState[initialMembershipQuoteKey]; !quoted {
			return nil, fmt.Errorf("%w: engine enrollment requires an NMI card token or a saved NMI or Stripe method", ErrPaymentMethodRequired)
		}
	}

	// Created in a transaction, so a request whose claim passed on cannot
	// commit it (#1099).
	create := func(ctx context.Context, session *models.CheckoutAttempt) error {
		return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return NewCheckoutAttemptRepo(s.db.NewWithPgxTx(tx)).Create(ctx, session)
		})
	}
	if mode == models.CheckoutAttemptModeOneOff {
		create = s.admitPurchaseSession
	}
	if err := create(ctx, session); err != nil {
		s.discardEnrollmentCard(ctx, vaulted)
		if idempotencyKey != "" {
			existing, getErr := s.repo.GetByID(ctx, session.ID)
			if getErr == nil {
				return s.resumeIdempotentSession(ctx, existing, session, &req.Payment, req.SuccessURL, req.CancelURL, user)
			}
		}
		return nil, fmt.Errorf("failed to create checkout attempt: %w", err)
	}

	if _, quoted := session.RailState[initialMembershipQuoteKey]; quoted {
		return s.sessionToResponse(session), nil
	}

	if err := s.initializeSession(ctx, session, &req.Payment, req.SuccessURL, req.CancelURL, user); err != nil {
		_ = s.markInitializationFailed(ctx, session, err)
		// An accepted card operation that awaits authentication or is still
		// unresolved answers with its state; a definite decline stays a 402.
		if response, found, readErr := s.acceptedOperationSessionResponse(ctx, session); readErr == nil && found && response.Status != string(models.CheckoutAttemptStatusFailed) {
			return response, nil
		}
		return nil, err
	}

	session.UpdatedAt = s.now()
	return s.saveInitializedSession(ctx, session)
}

func idempotentCheckoutAttemptID(merchantID uuid.UUID, key string) uuid.UUID {
	name := "openrails:checkout-session:" + merchantID.String() + ":" + strings.TrimSpace(key)
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name))
}

func (s *CheckoutAttemptService) resumeIdempotentSession(
	ctx context.Context,
	existing, requested *models.CheckoutAttempt,
	payment *CheckoutAttemptPaymentRequest,
	successURL, cancelURL string,
	user *UserIdentity,
) (*CheckoutAttemptResponse, error) {
	if existing == nil || requested == nil {
		return nil, fmt.Errorf("%w: idempotent checkout attempt unavailable", ErrCheckoutAttemptConflict)
	}
	// or#288: Rail and PspID are part of the match, so a retry whose routing
	// would now resolve elsewhere (arming or policy moved between attempts)
	// CONFLICTS rather than quietly switching processors mid-idempotency.
	storedFingerprint, _ := existing.RailState[checkoutAttemptFingerprintKey].(string)
	requestedFingerprint, _ := requested.RailState[checkoutAttemptFingerprintKey].(string)
	parametersMatch := existing.CustomerID == requested.CustomerID &&
		existing.PriceID != nil && requested.PriceID != nil && *existing.PriceID == *requested.PriceID &&
		existing.Mode == requested.Mode &&
		existing.Rail == requested.Rail &&
		existing.PspID == requested.PspID &&
		storedFingerprint != "" &&
		storedFingerprint == requestedFingerprint
	if !parametersMatch {
		return nil, fmt.Errorf("%w: %w", ErrCheckoutAttemptConflict, billing.ErrIdempotencyKeyReused)
	}

	// A replay answers a declined sale with the same refusal as the request
	// that was declined.
	if err := s.saleRefusal(ctx, existing); err != nil {
		return nil, err
	}
	if response, found, err := s.acceptedOperationSessionResponse(ctx, existing); found || err != nil {
		return response, err
	}
	switch existing.Status {
	case models.CheckoutAttemptStatusRequiresAction, models.CheckoutAttemptStatusSucceeded:
		return s.sessionToResponse(existing), nil
	case models.CheckoutAttemptStatusFailed:
		// Final for its key: a stale request for an abandoned attempt never
		// runs it again after the host moved on (#1099).
		return nil, failedSessionError(existing)
	case models.CheckoutAttemptStatusCreated:
		existing.IdempotencyKey = requested.IdempotencyKey
		if err := s.initializeSession(ctx, existing, payment, successURL, cancelURL, user); err != nil {
			_ = s.markInitializationFailed(ctx, existing, err)
			if response, found, readErr := s.acceptedOperationSessionResponse(ctx, existing); readErr == nil && found && response.Status != string(models.CheckoutAttemptStatusFailed) {
				return response, nil
			}
			return nil, err
		}
		existing.UpdatedAt = s.now()
		return s.saveInitializedSession(ctx, existing)
	default:
		return nil, fmt.Errorf("%w: previous checkout attempt is %s", ErrCheckoutAttemptConflict, existing.Status)
	}
}

func equalOptionalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (s *CheckoutAttemptService) GetSession(ctx context.Context, sessionID uuid.UUID, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, ErrCheckoutAttemptNotFound
		}
		return nil, err
	}
	if user == nil || strings.TrimSpace(user.ID) == "" || session.CustomerID.String() != user.ID {
		return nil, ErrCheckoutAttemptForbidden
	}

	if response, found, err := s.acceptedOperationSessionResponse(ctx, session); found || err != nil {
		return response, err
	}
	if session.Mode == models.CheckoutAttemptModePaymentMethod {
		if session.Rail != models.RailStripe {
			return s.renderPaymentMethodSetup(ctx, session)
		}
		// A Stripe card setup: its own routes drive it; a read reports it.
		response := s.sessionToResponse(session)
		if ref, _ := session.RailState["payment_method_id"].(string); session.Status == models.CheckoutAttemptStatusSucceeded {
			if id, err := uuid.Parse(ref); err == nil {
				method := billing.PaymentMethodID(id)
				response.PaymentMethodID = &method
			}
		}
		return response, nil
	}
	if s.isExpired(session) && !s.isTerminal(session.Status) {
		session.Status = models.CheckoutAttemptStatusExpired
		session.UpdatedAt = s.now()
		if updateErr := s.repo.Update(ctx, session); updateErr != nil {
			return nil, fmt.Errorf("failed to update expired session: %w", updateErr)
		}
	}

	return s.sessionToResponse(session), nil
}

// The offer uses the shared commercial terms type, not a second membership
// payload. A JSON string preserves integer benefit caps through RailState's
// map[string]any database roundtrip. No quote is financial authority.
const initialMembershipQuoteKey = "initial_membership_quote"

func quoteInitialMembership(ctx context.Context, session *models.CheckoutAttempt, price *models.Price, product *models.Product, method gen.BillingPaymentMethod, now time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil || session == nil || price == nil || product == nil || session.ID == uuid.Nil || session.CustomerID == uuid.Nil || session.Mode != models.CheckoutAttemptModeSubscription || (session.Rail != models.RailNMI && session.Rail != models.RailStripe) || session.PriceID == nil || *session.PriceID != price.ID || price.ProductID != product.ID || method.MerchantID != mid.UUID() || method.CustomerID != session.CustomerID || !charge.ChargeableOn(method, session.PspID) || !((method.Custodian == models.CustodianHyperSwitch && method.CustodianID != nil && method.Rail == "nmi") || (method.Custodian == models.CustodianPSP && method.CustodianID == nil)) || method.RailCustomerRef == nil || method.RailMethodRef == nil || method.ParkReason != nil || method.Rail != string(session.Rail) {
		return ErrCheckoutAttemptConflict
	}
	if _, exists := session.RailState[initialMembershipQuoteKey]; exists {
		return ErrCheckoutAttemptConflict
	}
	if !price.IsPurchasable() || !product.IsPurchasable() || price.Amount <= 0 || price.TrialUnitAmount != nil || price.TrialDurationHours != nil || session.Amount == nil || *session.Amount != price.Amount || session.Currency == nil || *session.Currency != price.Currency {
		return ErrCheckoutAttemptValidation
	}
	hours := price.RecurringCycleHours()
	if hours == nil || *hours <= 0 || int64(*hours) > math.MaxInt64/int64(time.Hour) || now.IsZero() {
		return ErrCheckoutAttemptValidation
	}
	now = now.UTC().Truncate(time.Microsecond)
	if session.ExpiresAt == nil || !session.ExpiresAt.After(now) {
		return ErrCheckoutAttemptExpired
	}
	benefits := models.CloneEntitlements(product.Entitlements)
	if benefits == nil {
		benefits = []string{}
	}
	terms := subscriptions.InitialMembershipTerms{CollectionPolicy: models.CollectionPolicyEngine, CancelAfterInitial: !sessionAutoRenew(session), SubscriptionID: uuidutil.NewV7(), PaymentID: uuidutil.NewV7(), CustomerID: session.CustomerID, PSPID: session.PspID, ProductID: product.ID, PriceID: price.ID, PaymentMethodID: method.ID, ProductName: product.DisplayName, Amount: price.Amount, RecurringAmount: price.Amount, Currency: price.Currency, AccessDurationHours: price.AccessDurationHours, AcceptedAt: now, PeriodStart: now, PeriodEnd: now.Add(time.Duration(*hours) * time.Hour), Entitlements: benefits}
	if err := terms.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(terms)
	if err != nil {
		return err
	}
	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	session.RailState[initialMembershipQuoteKey] = string(raw)
	return nil
}

func readInitialMembershipQuote(session *models.CheckoutAttempt) (subscriptions.InitialMembershipTerms, error) {
	var terms subscriptions.InitialMembershipTerms
	if session == nil {
		return terms, ErrCheckoutAttemptNotFound
	}
	raw, ok := session.RailState[initialMembershipQuoteKey].(string)
	if !ok || raw == "" {
		return terms, ErrCheckoutAttemptConflict
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&terms); err != nil {
		return terms, ErrCheckoutAttemptConflict
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return terms, ErrCheckoutAttemptConflict
	}
	if err := terms.Validate(); err != nil {
		return terms, err
	}
	if session.Mode != models.CheckoutAttemptModeSubscription || (session.Rail != models.RailNMI && session.Rail != models.RailStripe) || terms.CollectionPolicy != models.CollectionPolicyEngine || terms.Pending || terms.Amount <= 0 || terms.Amount != terms.RecurringAmount || terms.CustomerID != session.CustomerID || terms.PSPID != session.PspID || session.PriceID == nil || terms.PriceID != *session.PriceID || session.Amount == nil || terms.Amount != *session.Amount || session.Currency == nil || terms.Currency != *session.Currency {
		return terms, ErrCheckoutAttemptConflict
	}
	duration := terms.PeriodEnd.Sub(terms.PeriodStart)
	if !terms.AcceptedAt.Equal(terms.PeriodStart) || duration%time.Hour != 0 || !terms.PeriodStart.Add(duration).Equal(terms.PeriodEnd) {
		return terms, ErrCheckoutAttemptConflict
	}
	return terms, nil
}

func validateInitialMembershipPrincipal(ctx context.Context, session *models.CheckoutAttempt, principal billingauth.DelegatedPrincipal) error {
	mid, err := merchant.Require(ctx)
	if err != nil || session == nil || session.ID == uuid.Nil || session.CustomerID == uuid.Nil || principal.CredentialClass != billingauth.CredentialClassUserSession || principal.Invoker != "" || principal.MerchantID != mid || principal.SubjectID != session.CustomerID.String() {
		return ErrCheckoutAttemptForbidden
	}
	return nil
}

// acceptedInitialMembershipQuote prepares the FIRST confirmation only. The
// real integration resolves an existing canonical operation before calling it,
// so a repeated/uncertain confirmation cannot shift accepted period bounds.
// It does not enqueue, charge, create membership, or return a checkout success.
func acceptedInitialMembershipQuote(ctx context.Context, session *models.CheckoutAttempt, principal billingauth.DelegatedPrincipal, now time.Time) (subscriptions.InitialMembershipTerms, error) {
	if err := validateInitialMembershipPrincipal(ctx, session, principal); err != nil {
		return subscriptions.InitialMembershipTerms{}, err
	}
	terms, err := readInitialMembershipQuote(session)
	if err != nil {
		return terms, err
	}
	if now.IsZero() || session.Status != models.CheckoutAttemptStatusRequiresAction || session.ExpiresAt == nil || !session.ExpiresAt.After(now) {
		return terms, ErrCheckoutAttemptExpired
	}
	duration := terms.PeriodEnd.Sub(terms.PeriodStart)
	terms.AcceptedAt = now.UTC().Truncate(time.Microsecond)
	terms.PeriodStart = terms.AcceptedAt
	terms.PeriodEnd = terms.PeriodStart.Add(duration)
	return terms, terms.Validate()
}

// initialMembershipSessionResponse is a read-only projection of the accepted
// operation. Session expiry is an offer deadline, never a payment outcome.
func (s *CheckoutAttemptService) initialMembershipSessionResponse(ctx context.Context, session *models.CheckoutAttempt) (*CheckoutAttemptResponse, bool, error) {
	if _, quoted := session.RailState[initialMembershipQuoteKey]; !quoted {
		return nil, false, nil
	}
	terms, err := readInitialMembershipQuote(session)
	if err != nil {
		return nil, false, err
	}
	operation, err := intents.NewStore(s.db).GetByIdempotencyKey(ctx, InitialMembershipIdempotencyKey("checkout_attempt:"+session.ID.String()))
	if db.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := ownsInitialMembership(operation, session.CustomerID.String(), terms.PriceID, initialMembershipQuoteFingerprint(terms), &session.ID); err != nil {
		return nil, true, err
	}
	projection := *session
	projection.PaymentID, projection.SubscriptionID, projection.TransactionID = nil, nil, nil
	projection.ExpiresAt = nil
	switch operation.Status {
	case intents.StatusSucceeded:
		result, err := initialMembershipResponseFromIntent(operation)
		if err != nil {
			return nil, true, err
		}
		if err := s.applyCheckoutResponse(&projection, result); err != nil {
			return nil, true, err
		}
	case intents.StatusFailedTerminal:
		if err := intents.ValidateInitialMembershipTerminal(operation); err != nil {
			return nil, true, err
		}
		projection.Status = models.CheckoutAttemptStatusFailed
	case intents.StatusExpired:
		projection.Status = models.CheckoutAttemptStatusExpired
	case intents.StatusSuperseded:
		projection.Status = models.CheckoutAttemptStatusCanceled
	case intents.StatusPending, intents.StatusInFlight, intents.StatusFailedRetryable, intents.StatusUnknownNeedsVerify:
		// This response-only state is not a second persisted operation status.
		projection.Status = models.CheckoutAttemptStatus("processing")
		if authenticationRequired(operation) {
			projection.Status = models.CheckoutAttemptStatusRequiresAction
		}
	default:
		return nil, true, fmt.Errorf("unrecognized initial membership operation status %q", operation.Status)
	}
	response := s.sessionToResponse(&projection)
	response.Operation = &billing.PaymentOperation{ID: billing.PaymentOperationID(operation.ID), Status: operation.Status}
	response.NextAction = nil
	if operation.Status == intents.StatusFailedTerminal {
		response.Failure = operationFailure(operation)
	}
	return response, true, nil
}

// ConfirmCustomerSession is the self-service boundary. The operation ledger,
// rather than the session projection or quote expiry, owns accepted replay.
// AttemptOwner is who an attempt charges and on which rail.
type AttemptOwner struct {
	CustomerID uuid.UUID
	Rail       string
}

// Owner reads an attempt's customer and rail.
func (s *CheckoutAttemptService) Owner(ctx context.Context, id uuid.UUID) (AttemptOwner, error) {
	attempt, err := s.repo.GetByID(ctx, id)
	if db.IsNotFound(err) {
		return AttemptOwner{}, ErrCheckoutAttemptNotFound
	}
	if err != nil {
		return AttemptOwner{}, err
	}
	return AttemptOwner{CustomerID: attempt.CustomerID, Rail: string(attempt.Rail)}, nil
}

// acceptQuote accepts a quoted membership for the present payer, or
// confirms an attempt that carries no quote.
func (s *CheckoutAttemptService) acceptQuote(ctx context.Context, sessionID uuid.UUID, req *CheckoutAttemptConfirmRequest, user *UserIdentity, principal billingauth.DelegatedPrincipal) (*CheckoutAttemptResponse, error) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, ErrCheckoutAttemptNotFound
		}
		return nil, err
	}
	if user == nil || user.ID != session.CustomerID.String() {
		return nil, ErrCheckoutAttemptForbidden
	}
	if _, quoted := session.RailState[initialMembershipQuoteKey]; !quoted {
		return s.ConfirmSession(ctx, sessionID, req, user)
	}
	if err := validateInitialMembershipPrincipal(ctx, session, principal); err != nil {
		return nil, err
	}
	if req == nil || req.Payment.Rail != string(session.Rail) || req.Payment.Capture != nil || req.Payment.Signature != "" || req.Payment.Wallet != "" {
		return nil, ErrCheckoutAttemptValidation
	}
	terms, err := readInitialMembershipQuote(session)
	if err != nil {
		return nil, err
	}
	key := "checkout_attempt:" + session.ID.String()
	ctx = db.WithPSPID(ctx, session.PspID)
	_, err = intents.NewStore(s.db).GetByIdempotencyKey(ctx, InitialMembershipIdempotencyKey(key))
	if db.IsNotFound(err) {
		terms, err = acceptedInitialMembershipQuote(ctx, session, principal, s.now())
	}
	if err != nil {
		return nil, err
	}
	confirmer, ok := s.checkoutService.(interface {
		ConfirmInitialMembership(context.Context, subscriptions.InitialMembershipTerms, string, billingauth.DelegatedPrincipal, *uuid.UUID) (*CheckoutResponse, error)
	})
	if !ok {
		return nil, errors.New("initial membership service unavailable")
	}
	_, err = confirmer.ConfirmInitialMembership(ctx, terms, key, principal, &session.ID)
	if err != nil && !errors.Is(err, ErrCheckoutProcessing) {
		return nil, err
	}
	response, found, readErr := s.acceptedOperationSessionResponse(ctx, session)
	if readErr != nil {
		return nil, readErr
	}
	if !found {
		return nil, errors.New("accepted membership operation unavailable")
	}
	return response, nil
}

func (s *CheckoutAttemptService) ConfirmSession(ctx context.Context, sessionID uuid.UUID, req *CheckoutAttemptConfirmRequest, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, ErrCheckoutAttemptNotFound
		}
		return nil, err
	}
	if user == nil || strings.TrimSpace(user.ID) == "" || session.CustomerID.String() != user.ID {
		return nil, ErrCheckoutAttemptForbidden
	}

	if _, quoted := session.RailState[initialMembershipQuoteKey]; quoted {
		return nil, ErrCheckoutAttemptForbidden
	}

	if session.Mode == models.CheckoutAttemptModePaymentMethod {
		return s.confirmPaymentMethodSetup(ctx, session, req)
	}
	// A paid Solana checkout still records any other signature a client
	// submits for it: that money is flagged for refund, never dropped.
	otherSolanaSignature := session.Rail == models.RailSolana && req != nil && strings.TrimSpace(req.Payment.Signature) != "" &&
		(session.TransactionID == nil || strings.TrimSpace(*session.TransactionID) != strings.TrimSpace(req.Payment.Signature))
	if s.isTerminal(session.Status) && !(otherSolanaSignature && session.Mode == models.CheckoutAttemptModeOneOff) {
		if session.Status == models.CheckoutAttemptStatusSucceeded {
			if response, found, err := s.acceptedOperationSessionResponse(ctx, session); found || err != nil {
				return response, err
			}
			return s.sessionToResponse(session), nil
		}
		if session.Status != models.CheckoutAttemptStatusExpired {
			return nil, ErrCheckoutAttemptConflict
		}
	}
	rail := strings.ToLower(strings.TrimSpace(req.Payment.Rail))
	if rail == "" {
		return nil, fmt.Errorf("%w: payment.rail is required", ErrCheckoutAttemptValidation)
	}
	if rail != strings.ToLower(string(session.Rail)) {
		return nil, fmt.Errorf("%w: rail mismatch", ErrCheckoutAttemptValidation)
	}
	if s.isExpired(session) && rail != string(models.RailSolana) {
		if !s.isTerminal(session.Status) {
			_ = s.MarkExpired(ctx, session.ID, "checkout attempt expired")
		}
		return nil, ErrCheckoutAttemptExpired
	}

	// #704: carry the session's pinned PSP provenance into the
	// confirm flow (falling back to a fresh resolution for older sessions).
	ctx = db.WithPSPID(ctx, session.PspID)

	switch rail {
	case "solana":
		if session.Mode == models.CheckoutAttemptModeSubscription {
			return s.confirmSolanaSubscriptionSession(ctx, session, req, user)
		}
		return s.confirmSolanaSession(ctx, session, req, user)
	default:
		return nil, fmt.Errorf("%w: confirmation not implemented for rail %s", ErrCheckoutAttemptConflict, rail)
	}
}

func (s *CheckoutAttemptService) resolveMode(mode string, rail string, price *models.Price) (models.CheckoutAttemptMode, error) {
	if rail == "" {
		return "", fmt.Errorf("%w: rail is required", ErrCheckoutAttemptValidation)
	}

	trimmedMode := strings.TrimSpace(mode)
	if rail == "solana" {
		hasRecurring := priceHasSolanaRecurring(price)
		if trimmedMode == string(models.CheckoutAttemptModeSubscription) {
			if !hasRecurring {
				return "", fmt.Errorf("%w: price is not configured for Solana recurring billing", ErrCheckoutAttemptValidation)
			}
			return models.CheckoutAttemptModeSubscription, nil
		}
		if trimmedMode == string(models.CheckoutAttemptModeOneOff) {
			return models.CheckoutAttemptModeOneOff, nil
		}
		// Mode unspecified: a price with a published Solana recurring plan defaults
		// to subscription (Solana = subscription by default); otherwise one-off.
		if hasRecurring {
			return models.CheckoutAttemptModeSubscription, nil
		}
		return models.CheckoutAttemptModeOneOff, nil
	}

	expected := models.CheckoutAttemptModeOneOff
	if price.IsRecurring() {
		expected = models.CheckoutAttemptModeSubscription
	}
	if trimmedMode == "" {
		return expected, nil
	}
	if trimmedMode != string(expected) {
		return "", fmt.Errorf("%w: mode does not match price configuration", ErrCheckoutAttemptValidation)
	}
	return models.CheckoutAttemptMode(trimmedMode), nil
}

// validatePayment dispatches checkout-input validation to the per-rail
// validator that owns that rail's required-input contract. Keeping each
// rail's rules in its own method (rather than a shared switch body) is what
// keeps the validation contract from drifting out of sync with what the
// rail's executor actually consumes — the drift that previously made Stripe
// demand billing fields its hosted-checkout path never reads.
func (s *CheckoutAttemptService) validatePayment(ctx context.Context, rail string, payment *CheckoutAttemptPaymentRequest, user *UserIdentity) error {
	switch {
	case rails.IsNMI(models.Rail(rail)):
		return s.validateNMIInput(ctx, payment, user)
	case rail == "stripe":
		if strings.TrimSpace(payment.PaymentMethodID) != "" {
			return s.validateNMIInput(ctx, payment, user)
		}
		return s.validateStripeInput(payment)
	case rail == "solana":
		return s.validateSolanaInput(payment)
	case rail == "ccbill":
		return s.validateCCBillInput(payment, user)
	default:
		return fmt.Errorf("%w: unsupported rail", ErrCheckoutAttemptValidation)
	}
}

func rejectCheckoutAttemptPAN(req *CheckoutAttemptCreateRequest) error {
	if req == nil {
		return nil
	}
	payment := req.Payment
	if err := RejectPANShapedFields(&CheckoutRequest{
		PaymentMethodID: payment.PaymentMethodID,
		PaymentToken:    payment.PaymentToken,
		Email:           payment.Email,
		NameOnCard:      payment.NameOnCard,
		FirstName:       payment.FirstName,
		LastName:        payment.LastName,
		Address1:        payment.Address1,
		Address2:        payment.Address2,
		Phone:           payment.Phone,
		City:            payment.City,
		State:           payment.State,
		Zip:             payment.Zip,
		Country:         payment.Country,
		LastFour:        payment.LastFour,
		CardType:        payment.CardType,
		ExpiryDate:      payment.ExpiryDate,
		Metadata:        req.Metadata,
	}); err != nil {
		return fmt.Errorf("%w: invalid checkout input: %v", ErrCheckoutAttemptValidation, err)
	}
	extraFields := map[string]string{
		"payment.rail":         payment.Rail,
		"payment.token_symbol": payment.TokenSymbol,
		"payment.flow":         payment.Flow,
		"payment.wallet":       payment.Wallet,
		"success_url":          req.SuccessURL,
		"cancel_url":           req.CancelURL,
		"mode":                 req.Mode,
		"price_id":             req.PriceID,
		"price_key":            req.PriceKey,
		"product_key":          req.ProductKey,
	}
	if err := RejectPANShapedFields(&CheckoutRequest{Metadata: extraFields}); err != nil {
		return fmt.Errorf("%w: invalid checkout input: %v", ErrCheckoutAttemptValidation, err)
	}
	return nil
}

// validateNMIInput requires exactly one of payment_token, card or
// payment_method_id; a saved method must belong to the caller. The NMI
// executor charges with the token, card or vaulted method, so each input is
// genuinely consumed downstream.
func (s *CheckoutAttemptService) validateNMIInput(ctx context.Context, payment *CheckoutAttemptPaymentRequest, user *UserIdentity) error {
	hasToken := strings.TrimSpace(payment.PaymentToken) != ""
	hasMethod := strings.TrimSpace(payment.PaymentMethodID) != ""
	hasCard := payment.Card != nil
	if !hasToken && !hasMethod && !hasCard {
		return ErrPaymentMethodRequired
	}
	if hasCard && hasToken {
		return fmt.Errorf("%w: %w", ErrCheckoutAttemptValidation, paymentmethods.ErrCardWithToken)
	}
	if hasCard && hasMethod {
		return fmt.Errorf("%w: provide either card or payment_method_id, not both", ErrCheckoutAttemptValidation)
	}
	if hasToken && hasMethod {
		return fmt.Errorf("%w: provide either payment_token or payment_method_id, not both", ErrCheckoutAttemptValidation)
	}
	if hasMethod {
		pmID, err := billing.ParsePaymentMethodID(payment.PaymentMethodID)
		if err != nil || pmID.IsZero() {
			return fmt.Errorf("%w: invalid payment_method_id", ErrCheckoutAttemptValidation)
		}
		if s.paymentMethodService == nil {
			return fmt.Errorf("%w: payment method service unavailable", ErrCheckoutAttemptValidation)
		}
		if err := s.paymentMethodService.ValidateOwnership(ctx, pmID.UUID(), user.ID); err != nil {
			if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) || errors.Is(err, paymentmethods.ErrPaymentMethodAccessDenied) {
				return fmt.Errorf("%w: %w", ErrPaymentMethodStale, err)
			}
			return fmt.Errorf("validate payment method ownership: %w", err)
		}
	}
	return nil
}

// validateStripeInput validates inputs for Stripe hosted Checkout. Stripe's
// hosted page collects the customer's email and billing address itself, and
// createStripeCheckoutSession sends none of those fields, so they are NOT
// required here. Saved payment methods are not supported in the redirect flow.
func (s *CheckoutAttemptService) validateStripeInput(payment *CheckoutAttemptPaymentRequest) error {
	if strings.TrimSpace(payment.PaymentMethodID) != "" {
		return fmt.Errorf("%w: saved payment methods are not supported for stripe checkout", ErrCheckoutAttemptValidation)
	}
	return nil
}

// validateSolanaInput requires a token symbol so the executor can resolve which
// SPL mint to charge.
func (s *CheckoutAttemptService) validateSolanaInput(payment *CheckoutAttemptPaymentRequest) error {
	if strings.TrimSpace(payment.TokenSymbol) == "" {
		return fmt.Errorf("%w: token_symbol is required", ErrCheckoutAttemptValidation)
	}
	return nil
}

// validateCCBillInput requires the canonical name, country, and postal code
// consumed by CCBill. The email comes from the authenticated, verified customer
// identity rather than the browser request. Street, city, and state are
// optional in CCBill's hosted-card contract.
func (s *CheckoutAttemptService) validateCCBillInput(payment *CheckoutAttemptPaymentRequest, user *UserIdentity) error {
	if payment == nil {
		return fmt.Errorf("%w: payment is required", ErrCheckoutAttemptValidation)
	}
	if err := validateCCBillBillingIdentity(payment.NameOnCard, payment.Zip, payment.Country, user); err != nil {
		return fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
	}
	payment.Zip = strings.TrimSpace(payment.Zip)
	payment.Country = strings.ToUpper(strings.TrimSpace(payment.Country))
	return nil
}

func (s *CheckoutAttemptService) initializeSession(ctx context.Context, session *models.CheckoutAttempt, payment *CheckoutAttemptPaymentRequest, successURL, cancelURL string, user *UserIdentity) error {
	if session == nil {
		return fmt.Errorf("%w: session is required", ErrCheckoutAttemptValidation)
	}
	if payment == nil {
		return fmt.Errorf("%w: payment is required", ErrCheckoutAttemptValidation)
	}

	rail := strings.ToLower(string(session.Rail))
	// Route to rail-specific initialization based on config type detection
	// This allows adding new NMI providers via config without code changes
	switch {
	case rail == "solana":
		return s.initializeSolanaSession(ctx, session, payment)
	case rails.IsNMI(models.Rail(rail)):
		return s.initializeCheckoutAttempt(ctx, session, payment, successURL, cancelURL, user)
	case rail == "ccbill" || rail == "stripe":
		return s.initializeCheckoutAttempt(ctx, session, payment, successURL, cancelURL, user)
	default:
		return fmt.Errorf("%w: unsupported rail", ErrCheckoutAttemptValidation)
	}
}

func (s *CheckoutAttemptService) initializeSolanaSession(ctx context.Context, session *models.CheckoutAttempt, payment *CheckoutAttemptPaymentRequest) error {
	// Recurring Solana subscription (#261): distinct from the one-off Solana Pay
	// flow — the subscriber signs init_subscription_authority + subscribe in their
	// wallet, so we return UNSIGNED transactions to sign rather than a Pay URL.
	if session.Mode == models.CheckoutAttemptModeSubscription {
		return s.initializeSolanaSubscriptionSession(ctx, session, payment)
	}

	solanaProc, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCheckoutAttemptValidation, err)
	}

	tokenSymbol := strings.ToUpper(strings.TrimSpace(payment.TokenSymbol))
	if tokenSymbol == "" {
		return fmt.Errorf("%w: token_symbol is required", ErrCheckoutAttemptValidation)
	}

	// or#893: the flow is DECLARED, never defaulted. transfer_request (wallet
	// builds the tx from a Solana Pay URL) and transaction_request (OpenRails
	// builds and returns an unsigned tx) diverge in what is written, what is
	// verified and who signs — a session whose flow was inferred is a session
	// whose confirm path was guessed.
	flow := strings.TrimSpace(payment.Flow)
	if flow == "" {
		return fmt.Errorf("%w: payment.flow is required for solana (transfer_request | transaction_request)", ErrCheckoutAttemptValidation)
	}

	if solanaProc.Solana == nil {
		return fmt.Errorf("%w: solana rail is not configured", ErrCheckoutAttemptValidation)
	}
	tokenCfg, ok := solanaProc.Solana.Tokens[tokenSymbol]
	if !ok {
		return fmt.Errorf("%w: unsupported token", ErrCheckoutAttemptValidation)
	}
	tokenMint := tokenCfg.Mint
	if err := solanamodule.ValidateQuoteCurrency(*session.Currency, tokenSymbol, tokenMint); err != nil {
		return fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
	}
	if !strings.EqualFold(tokenSymbol, "SOL") && solanamodule.IsNativeSOLMint(tokenMint) {
		return fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrCheckoutAttemptValidation)
	}

	switch flow {
	case "transfer_request":
		if s.solanaPayService == nil {
			return fmt.Errorf("%w: solana pay service unavailable", ErrCheckoutAttemptValidation)
		}
		result, err := s.solanaPayService.GeneratePayment(ctx, session.CustomerID.String(), *session.PriceID, tokenSymbol, &session.ID)
		if err != nil {
			return err
		}
		session.Status = models.CheckoutAttemptStatusRequiresAction
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
			return fmt.Errorf("%w: recipient missing from payment quote", ErrCheckoutAttemptValidation)
		}
		session.RailState["token_mint"] = tokenMintValue
		session.RailState["recipient"] = recipient
		if err := setSolanaQuoteState(session.RailState, result.TokenUnits, result.TokenPriceUSD, result.FXRate, result.FXCurrency, result.QuotedAt, result.QuoteExpiresAt); err != nil {
			return err
		}
	case "transaction_request":
		// Transaction Request flow per Solana Pay spec:
		// - Wallet address is NOT required at session creation
		// - Transaction is built later when wallet calls POST /v1/checkout-attempts/:id/solana-pay
		// - Session just stores flow and token info, returns solana_pay_url for wallet
		if s.solanaTransactionService == nil {
			return fmt.Errorf("%w: solana transaction service unavailable", ErrCheckoutAttemptValidation)
		}
		decimals, err := solanamodule.RequireMintDecimals(ctx, s.solanaMints, tokenCfg.Mint)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
		}
		// OpenRails builds this transfer itself and does not resolve a
		// Token-2022 transfer hook's extra accounts: such a mint is refused
		// here rather than offered a transaction that cannot land.
		if !strings.EqualFold(tokenSymbol, "SOL") {
			info, err := solanamodule.RequireMintInfo(ctx, s.solanaMintInfo, tokenMint)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
			}
			if !info.Hook.IsZero() {
				return fmt.Errorf("%w: %s has a transfer hook; pay it by transfer request", ErrCheckoutAttemptValidation, tokenSymbol)
			}
		}
		quotedAt := s.now().UTC()
		quote, err := solanamodule.CalculateTokenQuote(ctx, tokenSymbol, tokenCfg.Mint, decimals, moneyutil.Micros(*session.Amount), *session.Currency, s.fxProvider, s.priceProvider, quotedAt)
		if err != nil {
			return fmt.Errorf("%w: failed to calculate solana token quote: %v", ErrCheckoutAttemptValidation, err)
		}
		session.Status = models.CheckoutAttemptStatusRequiresAction
		expiresAt := quotedAt.Add(defaultCheckoutAttemptTTL)
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
			return fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
		}
		session.RailState["recipient"] = recipient
	default:
		return fmt.Errorf("%w: unsupported solana flow", ErrCheckoutAttemptValidation)
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
func (s *CheckoutAttemptService) initializeSolanaSubscriptionSession(ctx context.Context, session *models.CheckoutAttempt, payment *CheckoutAttemptPaymentRequest) error {
	if err := s.solanaSignerAvailable(ctx); err != nil {
		return err
	}
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutAttemptValidation)
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
	price, err := s.priceService.GetByID(ctx, *session.PriceID)
	if err != nil || price == nil {
		return fmt.Errorf("%w: price not found", ErrCheckoutAttemptValidation)
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
			return fmt.Errorf("%w: product not found", ErrCheckoutAttemptValidation)
		}
		conflict, err := s.checkoutService.CheckSubscriptionConflict(ctx, session.CustomerID.String(), price, product)
		if err != nil {
			return fmt.Errorf("%w: failed to check existing subscriptions: %v", ErrCheckoutAttemptValidation, err)
		}
		if conflict != nil && conflict.Blocked {
			return fmt.Errorf("%w: %s", ErrCheckoutAttemptConflict, conflict.Message)
		}
	}

	terms, err := parseSolanaPlanTerms(price.ForPSP(session.PspID).PSPLinkForRail(models.RailSolana))
	if err != nil {
		return err
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}

	// The reference binds the prepared first payment to this checkout: the
	// merchant co-signs the bundle carrying it, and confirm requires it.
	reference, err := solana.GenerateReference()
	if err != nil {
		return fmt.Errorf("failed to generate reference: %w", err)
	}
	res, err := s.solanaPrepareSubscribe.Prepare(ctx, recurring.PrepareSubscribeInput{
		MerchantID:       tid,
		SubscriberWallet: wallet,
		PlanID:           terms.planID,
		MintSymbol:       terms.mintSymbol,
		AmountBaseUnits:  terms.amount,
		PeriodHours:      terms.period,
		PlanCreatedAt:    terms.createdAt,
		Reference:        reference,
	})
	if err != nil {
		return err
	}

	session.Reference = &reference
	expiresAt := s.now().Add(defaultCheckoutAttemptTTL)
	session.Status = models.CheckoutAttemptStatusRequiresAction
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
// solanaSignerAvailable refuses (503) while the Solana rail's signer is
// unavailable or its identity change awaits approval (#1101).
func (s *CheckoutAttemptService) solanaSignerAvailable(ctx context.Context) error {
	if _, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails); err != nil && errors.Is(err, vault.ErrUnavailable) {
		return err
	}
	return nil
}

func (s *CheckoutAttemptService) initializeSolanaSubscriptionPayRequest(ctx context.Context, session *models.CheckoutAttempt) error {
	if err := s.solanaSignerAvailable(ctx); err != nil {
		return err
	}
	price, err := s.priceService.GetByID(ctx, *session.PriceID)
	if err != nil || price == nil {
		return fmt.Errorf("%w: price not found", ErrCheckoutAttemptValidation)
	}

	// Duplicate-billing guard (issue #269) — same as the wallet subscribe path:
	// refuse to issue a Solana Pay QR that would create a second concurrent
	// non-terminal subscription in the same product/tier-group.
	if s.checkoutService != nil {
		product, perr := s.productService.GetByID(ctx, price.ProductID)
		if perr != nil || product == nil {
			return fmt.Errorf("%w: product not found", ErrCheckoutAttemptValidation)
		}
		conflict, cerr := s.checkoutService.CheckSubscriptionConflict(ctx, session.CustomerID.String(), price, product)
		if cerr != nil {
			return fmt.Errorf("%w: failed to check existing subscriptions: %v", ErrCheckoutAttemptValidation, cerr)
		}
		if conflict != nil && conflict.Blocked {
			return fmt.Errorf("%w: %s", ErrCheckoutAttemptConflict, conflict.Message)
		}
	}

	terms, err := parseSolanaPlanTerms(price.ForPSP(session.PspID).PSPLinkForRail(models.RailSolana))
	if err != nil {
		return err
	}

	expiresAt := s.now().Add(defaultCheckoutAttemptTTL)
	session.Status = models.CheckoutAttemptStatusRequiresAction
	session.ExpiresAt = &expiresAt
	if session.RailState == nil {
		session.RailState = map[string]any{}
	}
	// flow=transaction_request is what makes sessionToResponse build the
	// solana_pay_url; the subscribe terms travel on RailState exactly like the
	// wallet path so BuildSolanaPayTransaction can re-derive the PrepareSubscribe
	// input without trusting client input.
	session.RailState["flow"] = "transaction_request"
	session.RailState["plan_id"] = strconv.FormatUint(terms.planID, 10)
	session.RailState["mint_symbol"] = terms.mintSymbol
	session.RailState["amount_base_units"] = strconv.FormatUint(terms.amount, 10)
	session.RailState["period_hours"] = strconv.FormatUint(terms.period, 10)
	session.RailState["plan_created_at"] = strconv.FormatInt(terms.createdAt, 10)
	return nil
}

// confirmSolanaSubscriptionSession completes a subscribe checkout from the
// signature of its landed first payment. The payment is read from the chain and
// must be the bundle this checkout prepared: signed by the bound wallet,
// carrying the checkout's reference and pulling the full first period to the
// merchant. Nothing the client sends besides the signature is trusted.
func (s *CheckoutAttemptService) confirmSolanaSubscriptionSession(ctx context.Context, session *models.CheckoutAttempt, req *CheckoutAttemptConfirmRequest, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return nil, fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutAttemptValidation)
	}
	wallet := strings.TrimSpace(getStringField(session.RailState, "subscriber_wallet"))
	if reqWallet := strings.TrimSpace(req.Payment.Wallet); reqWallet != "" && wallet != "" && reqWallet != wallet {
		return nil, fmt.Errorf("%w: wallet does not match session", ErrCheckoutAttemptValidation)
	}
	sig := strings.TrimSpace(req.Payment.Signature)
	if wallet == "" || sig == "" {
		return nil, fmt.Errorf("%w: the signed first payment is required", ErrCheckoutAttemptValidation)
	}
	var email string
	if user != nil && user.Email != nil {
		email = *user.Email
	}
	sub, err := s.enrollSolanaSubscription(ctx, session, sig, email)
	switch {
	case errors.Is(err, recurring.ErrPaymentNotLanded):
		// Confirmation lag: the wallet just sent it. Retryable; the poller also
		// settles a Solana Pay subscribe asynchronously.
		return nil, fmt.Errorf("%w: subscription payment not yet confirmed on-chain; retry", ErrCheckoutAttemptConflict)
	case errors.Is(err, recurring.ErrPaymentLate):
		return nil, fmt.Errorf("%w: %v", ErrCheckoutAttemptExpired, err)
	case errors.Is(err, recurring.ErrPaymentUnverified):
		return nil, fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
	case errors.Is(err, settlement.ErrClaimed):
		return nil, fmt.Errorf("%w: %v", ErrCheckoutAttemptConflict, err)
	case err != nil:
		return nil, err
	}

	// Verified on-chain within the late window: settled even if the session's
	// window has since elapsed.
	if err := s.markSucceededWithSubscription(ctx, session.ID, uuid.Nil, sig, sub.ID, true); err != nil {
		return nil, err
	}
	updated, err := s.repo.GetByID(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	return s.sessionToResponse(updated), nil
}

// enrollSolanaSubscription activates a subscribe checkout from its landed first
// payment, with the terms, wallet, reference and validity the checkout stored.
func (s *CheckoutAttemptService) enrollSolanaSubscription(ctx context.Context, session *models.CheckoutAttempt, signature, email string) (*models.Subscription, error) {
	tenantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
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
	in := recurring.EnrollInput{
		MerchantID:        tenantID,
		CheckoutAttemptID: session.ID,
		UserID:            session.CustomerID.String(),
		CustomerEmail:     email,
		PriceID:           *session.PriceID,
		SubscriberWallet:  strings.TrimSpace(getStringField(session.RailState, "subscriber_wallet")),
		PlanID:            terms.planID,
		MintSymbol:        terms.mintSymbol,
		AmountBaseUnits:   terms.amount,
		PeriodHours:       terms.period,
		PlanCreatedAt:     terms.createdAt,
		FiatAmount:        *session.Amount,
		Currency:          *session.Currency,
		Signature:         signature,
	}
	if session.Reference != nil {
		in.Reference = *session.Reference
	}
	if session.ExpiresAt != nil {
		in.ValidUntil = *session.ExpiresAt
	}
	sub, err := s.solanaEnroll.ConfirmEnrollment(ctx, in)
	var late *recurring.LatePaymentError
	if errors.As(err, &late) && s.db != nil {
		if aerr := solanamodule.RecordLateSettlement(ctx, s.db, late.LandedAt, in.Reference, signature, solanamodule.LateSettlement{
			UserID: in.UserID, PriceID: in.PriceID.String(), SessionID: session.ID.String(), Amount: in.FiatAmount, Currency: in.Currency,
			Token: in.MintSymbol, TokenAmount: in.AmountBaseUnits, ExpiresAt: in.ValidUntil,
		}); aerr != nil {
			return nil, aerr
		}
	}
	return sub, err
}

func (s *CheckoutAttemptService) initializeCheckoutAttempt(ctx context.Context, session *models.CheckoutAttempt, payment *CheckoutAttemptPaymentRequest, successURL, cancelURL string, user *UserIdentity) error {
	if s.checkoutService == nil {
		return fmt.Errorf("%w: checkout service unavailable", ErrCheckoutAttemptValidation)
	}

	// #848: execute against the PSP the session pinned (falling back to the rail
	// kind for sessions without a distinct PSP key), so the executor never
	// re-resolves a kind onto a different account.
	railSelector := string(session.Rail)
	if psp, ok := session.RailFields[checkoutAttemptPSPFieldKey].(string); ok && strings.TrimSpace(psp) != "" {
		railSelector = strings.TrimSpace(psp)
	}
	req := &CheckoutRequest{
		AutoRenew:         orderAutoRenewInput(session),
		PriceID:           billing.PriceID(*session.PriceID).String(),
		PaymentMethodID:   payment.PaymentMethodID,
		PaymentToken:      payment.PaymentToken,
		Card:              payment.Card,
		Rail:              railSelector,
		SuccessURL:        successURL,
		CancelURL:         cancelURL,
		Metadata:          session.Metadata,
		CheckoutStartedAt: session.CreatedAt,
		Email:             payment.Email,
		NameOnCard:        payment.NameOnCard,
		FirstName:         payment.FirstName,
		LastName:          payment.LastName,
		Address1:          payment.Address1,
		Address2:          payment.Address2,
		Phone:             payment.Phone,
		City:              payment.City,
		State:             payment.State,
		Zip:               payment.Zip,
		Country:           payment.Country,
		LastFour:          payment.LastFour,
		CardType:          payment.CardType,
		ExpiryDate:        payment.ExpiryDate,
	}

	if selected, _ := session.RailState["customer_selected"].(bool); selected {
		req.Amount = session.Amount
	}

	// The accepted operation belongs to this persisted session. The caller's
	// replay key resolves the session; it is not a session identity.
	req.IdempotencyKey = "checkout_native_session:" + session.ID.String()
	req.CheckoutAttemptID = billing.CheckoutAttemptID(session.ID).String()
	terms, err := purchaseTerms(session)
	if err != nil {
		return err
	}
	req.acceptedPurchase = terms

	resp, err := s.checkoutService.Checkout(ctx, req, user)
	if err != nil {
		return err
	}

	return s.applyCheckoutResponse(session, resp)
}

func (s *CheckoutAttemptService) applyCheckoutResponse(session *models.CheckoutAttempt, resp *CheckoutResponse) error {
	if session == nil {
		return fmt.Errorf("%w: session is required", ErrCheckoutAttemptValidation)
	}
	if resp == nil {
		return fmt.Errorf("%w: checkout response is required", ErrCheckoutAttemptValidation)
	}

	switch resp.Status {
	case "success", "pending":
		session.Status = models.CheckoutAttemptStatusSucceeded
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
			return fmt.Errorf("%w: redirect url missing", ErrCheckoutAttemptValidation)
		}
		session.Status = models.CheckoutAttemptStatusRequiresAction
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState["redirect_url"] = redirectURL
	case "blocked":
		msg := strings.TrimSpace(resp.Message)
		if msg == "" {
			msg = "checkout blocked"
		}
		return fmt.Errorf("%w: %s", ErrCheckoutAttemptConflict, msg)
	default:
		return fmt.Errorf("%w: unsupported checkout status", ErrCheckoutAttemptConflict)
	}

	return nil
}

func (s *CheckoutAttemptService) buildRailFields(rail string, payment *CheckoutAttemptPaymentRequest, user *UserIdentity) map[string]any {
	fields := map[string]any{
		"rail": rail,
	}
	if payment == nil {
		return fields
	}
	email := payment.Email
	if rail == string(models.RailCCBill) {
		email = ""
		if user != nil && user.Email != nil {
			email = *user.Email
		}
	}

	addField(fields, "payment_method_id", payment.PaymentMethodID)
	addField(fields, "token_symbol", payment.TokenSymbol)
	addField(fields, "flow", payment.Flow)
	addField(fields, "wallet", payment.Wallet)
	addField(fields, "email", email)
	addField(fields, "name_on_card", payment.NameOnCard)
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

func (s *CheckoutAttemptService) sessionToResponse(session *models.CheckoutAttempt) *CheckoutAttemptResponse {
	resp := &CheckoutAttemptResponse{
		Object:   "checkout_attempt",
		ID:       billing.CheckoutAttemptID(session.ID),
		Status:   string(session.Status),
		Mode:     string(session.Mode),
		Amount:   session.Amount,
		Currency: session.Currency,
		Payment: CheckoutAttemptPaymentResponse{
			Rail: string(session.Rail),
		},
		ExpiresAt: session.ExpiresAt,
		CreatedAt: session.CreatedAt,
	}
	if session.PriceID != nil {
		id := billing.PriceID(*session.PriceID)
		resp.PriceID = &id
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
		paymentID := billing.PaymentID(*session.PaymentID)
		resp.PaymentID = &paymentID
	}
	if session.SubscriptionID != nil {
		subID := billing.SubscriptionID(*session.SubscriptionID)
		resp.SubscriptionID = &subID
	}

	payable := session.Status == models.CheckoutAttemptStatusCreated || session.Status == models.CheckoutAttemptStatusRequiresAction
	if session.RailState != nil {
		if val, ok := session.RailState["transaction_url"].(string); ok && strings.TrimSpace(val) != "" && payable {
			resp.Payment.TransactionURL = val
		}
		// Build solana_pay_url for every Solana-Pay-capable session. The wallet POSTs
		// its account to this URL and the matching BuildSolanaPayTransaction branch
		// returns the right tx:
		//   - transaction_request flow → one-off transfer OR recurring subscribe
		//     (price-driven: BuildSolanaPayTransaction reads the session mode).
		if payable && solanaSessionUsesPayURL(session) {
			// Construct the Solana Pay URL:
			// - standalone: solana:{public_billing_base_url}/v1/checkout-attempts/:id/solana-pay
			// - embedded:   solana:{public_billing_base_url}/v1/checkout-attempts/:id/solana-pay (public_billing_base_url typically ends with /billing)
			baseURL := s.getAPIBaseURL()
			if baseURL != "" {
				resp.Payment.SolanaPayURL = fmt.Sprintf(
					"solana:%s/v1/checkout-attempts/%s/solana-pay",
					baseURL,
					billing.CheckoutAttemptID(session.ID),
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

	if terms, err := readInitialMembershipQuote(session); err == nil {
		resp.MembershipQuote = &CheckoutAttemptMembershipQuote{AutoRenew: !terms.CancelAfterInitial, ProductName: terms.ProductName, CycleHours: int64(terms.PeriodEnd.Sub(terms.PeriodStart) / time.Hour), AccessDurationHours: terms.AccessDurationHours, Entitlements: models.CloneEntitlements(terms.Entitlements)}
	}
	// Local HTTP failure and TTL expiry cannot declare a submitted Stripe
	// purchase financially failed. Keep callers polling the accepted attempt
	// until payment or authoritative provider closure resolves it.
	if _, accepted := session.RailState[acceptedPurchaseTermsKey]; accepted && session.Rail == models.RailStripe && session.RailState["purchase_submitted"] == true && session.RailState["provider_closed"] != true {
		if session.Status == models.CheckoutAttemptStatusCreated || session.Status == models.CheckoutAttemptStatusFailed || session.Status == models.CheckoutAttemptStatusExpired || session.Status == models.CheckoutAttemptStatusCanceled || session.Status == models.CheckoutAttemptStatusRequiresAction && s.isExpired(session) {
			resp.Status = "processing"
			resp.ExpiresAt = nil
			resp.Message = "The original payment outcome is being verified. Keep this checkout attempt."
		}
	}

	if action := s.buildNextAction(resp); action != nil {
		resp.NextAction = action
	}

	// Recurring Solana subscribe (#261): surface the unsigned transaction(s) to
	// sign. Takes precedence over other next_actions for a subscription session.
	if resp.Status == string(models.CheckoutAttemptStatusRequiresAction) {
		if txns := getStringSliceField(session.RailState, "sign_transactions"); len(txns) > 0 {
			resp.NextAction = &CheckoutAttemptNextAction{
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

func (s *CheckoutAttemptService) buildNextAction(resp *CheckoutAttemptResponse) *CheckoutAttemptNextAction {
	if resp == nil {
		return nil
	}
	if resp.Status != string(models.CheckoutAttemptStatusRequiresAction) {
		return nil
	}
	if resp.Payment.RedirectURL != "" {
		return &CheckoutAttemptNextAction{
			Type: "redirect_to_url",
			RedirectToURL: &CheckoutAttemptRedirectToURL{
				URL: resp.Payment.RedirectURL,
			},
		}
	}
	if resp.Payment.TransactionURL != "" {
		return &CheckoutAttemptNextAction{
			Type: "solana_qr",
		}
	}
	if resp.Payment.SolanaPayURL != "" {
		return &CheckoutAttemptNextAction{
			Type: "solana_pay",
		}
	}
	return nil
}

// solanaSessionUsesPayURL reports whether a session should surface a
// solana_pay_url (i.e. the wallet completes it by scanning a QR / POSTing its
// account to the solana-pay endpoint). True for the transaction_request flow
// (one-off transfer OR recurring subscribe — the build endpoint picks based on
// the session mode/price).
func solanaSessionUsesPayURL(session *models.CheckoutAttempt) bool {
	if session == nil || session.Rail != models.RailSolana {
		return false
	}
	if flow, ok := session.RailState["flow"].(string); ok && flow == "transaction_request" {
		return true
	}
	return false
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

func (s *CheckoutAttemptService) confirmSolanaSession(ctx context.Context, session *models.CheckoutAttempt, req *CheckoutAttemptConfirmRequest, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	if strings.TrimSpace(req.Payment.Signature) == "" {
		return nil, fmt.Errorf("%w: signature is required", ErrCheckoutAttemptValidation)
	}
	if s.solanaTransactionService == nil {
		return nil, fmt.Errorf("%w: solana transaction service unavailable", ErrCheckoutAttemptValidation)
	}
	solanaProc, err := solanamodule.RequireSolanaRailConfig(ctx, s.rails)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
	}

	// Get token symbol from RailState (where initializeSolanaSession stores it)
	tokenSymbol := strings.ToUpper(strings.TrimSpace(getStringField(session.RailState, "token_symbol")))
	if tokenSymbol == "" {
		return nil, fmt.Errorf("%w: token_symbol missing", ErrCheckoutAttemptValidation)
	}

	if solanaProc.Solana == nil {
		return nil, fmt.Errorf("%w: solana rail is not configured", ErrCheckoutAttemptValidation)
	}
	tokenCfg, ok := solanaProc.Solana.Tokens[tokenSymbol]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported token", ErrCheckoutAttemptValidation)
	}
	tokenMint := tokenCfg.Mint
	storedTokenMint := getStringField(session.RailState, "token_mint")
	if storedTokenMint == "" {
		return nil, fmt.Errorf("%w: token_mint missing", ErrCheckoutAttemptValidation)
	}
	if !strings.EqualFold(tokenSymbol, "SOL") && solanamodule.IsNativeSOLMint(storedTokenMint) {
		return nil, fmt.Errorf("%w: non-SOL token cannot use native SOL mint", ErrCheckoutAttemptValidation)
	}
	if strings.TrimSpace(storedTokenMint) != strings.TrimSpace(tokenMint) {
		return nil, fmt.Errorf("%w: token_mint mismatch", ErrCheckoutAttemptValidation)
	}

	expectedAmount := getUint64Field(session.RailState, "token_amount")
	if expectedAmount == 0 {
		return nil, fmt.Errorf("%w: token_amount missing or invalid", ErrCheckoutAttemptValidation)
	}
	expectedRecipient := getStringField(session.RailState, "recipient")
	if expectedRecipient == "" {
		return nil, fmt.Errorf("%w: recipient missing", ErrCheckoutAttemptValidation)
	}
	// A client naming a wallet must name the one the session is bound to.
	if payer, wallet := strings.TrimSpace(getStringField(session.RailState, "payer")), strings.TrimSpace(req.Payment.Wallet); payer != "" && wallet != "" && payer != wallet {
		return nil, fmt.Errorf("%w: wallet does not match session", ErrCheckoutAttemptValidation)
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		return nil, fmt.Errorf("%w: reference missing", ErrCheckoutAttemptValidation)
	}
	referenceValue := strings.TrimSpace(*session.Reference)

	// or#893: the memo policy follows WHO BUILT the transaction. In the
	// transaction-request flow OpenRails builds and stamps it, so absence means
	// the signature is not our transaction. In the transfer-request flow the
	// buyer's wallet builds it from the Solana Pay URL and may drop the memo.
	memoPolicy := solana.MemoRequired
	if isSolanaTransferRequestFlow(session) {
		memoPolicy = solana.MemoPresenceOptional
	}
	signature := strings.TrimSpace(req.Payment.Signature)
	// The page waits a bounded time for finalization; past it the transfer is
	// still being confirmed and the poller credits it, so the page is told to
	// wait (it polls the session) rather than handed an error.
	wctx, cancel := context.WithTimeout(ctx, solanaFinalizeWait)
	defer cancel()
	obs, err := s.solanaTransactionService.ObserveTransfer(wctx, solana.ObserveTransferRequest{
		Signature: signature, Recipient: expectedRecipient, TokenMint: storedTokenMint, Reference: referenceValue,
		MemoLocalID: session.ID, MemoPolicy: memoPolicy,
	})
	// The page learns the outcome once the transfer is finalized, the only
	// commitment money is credited at; the poller records anything it refuses.
	switch {
	case errors.Is(err, solana.ErrForeignTransfer), errors.Is(err, solana.ErrFailedOnChain):
		return nil, fmt.Errorf("%w: %v", ErrCheckoutAttemptValidation, err)
	case errors.Is(err, solana.ErrUnreadableTransfer):
		return nil, fmt.Errorf("%w: the transaction cannot be read; it is recorded for review", ErrCheckoutAttemptConflict)
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		resp := s.sessionToResponse(session)
		resp.Status = "processing"
		resp.Message = "Confirming your payment on Solana. You can close this page; it is credited once final."
		return resp, nil
	case err != nil:
		return nil, err
	}
	receipt, err := s.SettleSolanaTransfer(ctx, referenceValue, solanamodule.ObservedTransfer{Signature: signature, Amount: obs.Amount, Other: obs.Other, Payer: obs.Payer, LandedAt: obs.LandedAt})
	if err != nil {
		return nil, err
	}
	switch receipt.Disposition {
	case solanamodule.Ignored:
		return nil, fmt.Errorf("%w: transaction paid nothing to the merchant", ErrCheckoutAttemptValidation)
	case solanamodule.Duplicate:
		return nil, fmt.Errorf("%w: this transfer already settled a different checkout", ErrCheckoutAttemptConflict)
	case solanamodule.Review:
		if receipt.ReviewReason == solanamodule.ReasonLate {
			return nil, fmt.Errorf("%w: payment landed after the quote expired; recorded for refund review", ErrCheckoutAttemptExpired)
		}
		return nil, fmt.Errorf("%w: transfer recorded for refund review (%s)", ErrCheckoutAttemptConflict, receipt.ReviewReason)
	}
	updated, err := s.repo.GetByID(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	return s.sessionToResponse(updated), nil
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

	if session.Mode == models.CheckoutAttemptModePaymentMethod {
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
	if session.CustomerID.String() != userID || session.Rail != models.RailCCBill || session.PriceID == nil || *session.PriceID != priceID {
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

// or#893: no missing-flow default. Every Solana session records its flow in
// rail_state at creation, so an absent one is not "the old kind of session" —
// it is a session whose rail_state was not written by this code path, and
// treating it as transfer_request would run the wrong finalize.
func isSolanaTransferRequestFlow(session *models.CheckoutAttempt) bool {
	if session == nil {
		return false
	}
	return strings.ToLower(strings.TrimSpace(getStringField(session.RailState, "flow"))) == "transfer_request"
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
	case string:
		// The canonical JSONB shape for a base-unit amount (MONEY-3): a
		// decimal string, so the value survives the round-trip exactly.
		if parsed, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

// GetSessionForSolanaPay retrieves and validates a checkout attempt for Solana Pay spec endpoints.
// Returns session info needed for GET endpoint or an error if the session is invalid.
func (s *CheckoutAttemptService) GetSessionForSolanaPay(ctx context.Context, sessionID uuid.UUID) (*solanamodule.PaySessionInfo, error) {
	if s.repo == nil {
		return nil, ErrCheckoutAttemptNotFound
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

	// Check if expired
	if session.ExpiresAt != nil && s.now().After(*session.ExpiresAt) {
		return nil, ErrCheckoutAttemptExpired
	}

	// Check if already completed
	if session.Status == models.CheckoutAttemptStatusSucceeded ||
		session.Status == models.CheckoutAttemptStatusCanceled {
		return nil, ErrCheckoutAttemptAlreadyCompleted
	}

	// Get product name for label (via price)
	var productName string
	if s.priceService != nil {
		price, err := s.priceService.GetByID(ctx, *session.PriceID)
		if err == nil && s.productService != nil {
			product, err := s.productService.GetByID(ctx, price.ProductID)
			if err == nil {
				productName = product.DisplayName
			}
		}
	}

	return &solanamodule.PaySessionInfo{
		ProductName: productName,
	}, nil
}

// BuildSolanaPayTransaction builds a Solana transaction for the given checkout attempt and wallet account.
// This implements the POST endpoint of the Solana Pay Transaction Request spec.
func (s *CheckoutAttemptService) BuildSolanaPayTransaction(ctx context.Context, sessionID uuid.UUID, account string) (*solanamodule.PayTransactionResponse, error) {
	if err := s.requireProviderWrites(); err != nil {
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

	// Check if expired
	if session.ExpiresAt != nil && s.now().After(*session.ExpiresAt) {
		return nil, ErrCheckoutAttemptExpired
	}

	// Check if already completed
	if session.Status == models.CheckoutAttemptStatusSucceeded ||
		session.Status == models.CheckoutAttemptStatusCanceled {
		return nil, ErrCheckoutAttemptAlreadyCompleted
	}

	// A RECURRING subscribe over Solana Pay (price-driven) builds the init or
	// the atomic [subscribe+transfer] tx for the POSTed account, with the
	// reference injected and put under the poller's watch.
	if session.Mode == models.CheckoutAttemptModeSubscription {
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
	if session.ExpiresAt != nil {
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
		PriceID:     *session.PriceID,
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

// ConfirmSolanaSubscribeSession completes a RECURRING subscribe Solana Pay
// session when the reference poller finds a confirmed transaction carrying its
// reference. Anyone can put a public reference in a transaction, so the
// signature only names a candidate: enrollment verifies it is this checkout's
// first payment (see confirmSolanaSubscriptionSession). A transaction not yet
// readable stays pending; one that is not the payment is refused and the
// poller moves on to the reference's other signatures.
//
// Idempotent: a re-confirm of an already-succeeded session is a no-op, and
// ConfirmEnrollment upserts on the rail subscription id.
func (s *CheckoutAttemptService) ConfirmSolanaSubscribeSession(ctx context.Context, sessionID uuid.UUID, signature string) error {
	if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
		return fmt.Errorf("%w: solana recurring billing is not configured", ErrCheckoutAttemptValidation)
	}
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutAttemptNotFound
	}
	if session.Status == models.CheckoutAttemptStatusSucceeded {
		return nil
	}
	ctx = s.pollerConfirmContext(ctx, session)
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return fmt.Errorf("%w: signature is required", ErrCheckoutAttemptValidation)
	}
	if strings.TrimSpace(getStringField(session.RailState, "subscriber_wallet")) == "" {
		// No wallet has POSTed yet — there is nothing to confirm.
		return solanamodule.ErrSolanaSubscribePending
	}
	sub, err := s.enrollSolanaSubscription(ctx, session, signature, "")
	if errors.Is(err, recurring.ErrPaymentNotLanded) {
		return solanamodule.ErrSolanaSubscribePending
	}
	if err != nil {
		return err
	}
	return s.markSucceededWithSubscription(ctx, session.ID, uuid.Nil, signature, sub.ID, true)
}
