package solana

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/identity"
	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

const (
	// Poll interval for checking pending payments
	solanaPayPollInterval = 500 * time.Millisecond
	// Per-reference backoff when no matching transaction is found yet.
	solanaNoTxRetryInterval = 3 * time.Second
	// Per-reference backoff when signatures exist but none can be processed yet.
	solanaUnmatchedTxRetryInterval = 5 * time.Second
)

type purchaseRegistrar interface {
	RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*RegisterPurchaseResult, error)
}

type RegisterPurchaseResult struct {
	PaymentID    uuid.UUID
	Entitlements []string
}

type paymentLookup interface {
	GetByTransactionID(ctx context.Context, processor models.Processor, transactionID string) (*models.Payment, error)
}

type checkoutSessionMarker interface {
	MarkSucceeded(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string) error
	// ConfirmSolanaLifecycleSession mirrors a confirmed on-chain cancel /
	// tier-change for a lifecycle Solana Pay session and marks it succeeded. It is
	// idempotent. Implemented by *checkout.CheckoutSessionService.
	ConfirmSolanaLifecycleSession(ctx context.Context, sessionID uuid.UUID, signature string) error
	// ConfirmSolanaSubscribeSession advances a RECURRING subscribe Solana Pay
	// session on a confirmed reference: if only init landed it stays pending; once
	// the subscription PDA is funded (the atomic [subscribe+transfer] landed) it
	// enrolls the membership and marks the session succeeded. Idempotent.
	// Implemented by *checkout.CheckoutSessionService.
	ConfirmSolanaSubscribeSession(ctx context.Context, sessionID uuid.UUID, signature string) error
}

// SolanaPayPoller polls the blockchain for confirmed Solana Pay payments
type SolanaPayPoller struct {
	db                     *db.DB
	redis                  *redis.Client
	cfg                    *config.Config
	rpc                    *solanarpc.RPCClient
	solanaPayService       *SolanaPayService
	solanaTransactionSvc   *SolanaTransactionService
	purchaseRegistrar      purchaseRegistrar
	paymentLookup          paymentLookup
	checkoutSessionService checkoutSessionMarker

	mu      sync.Mutex
	running bool
	stopCh  chan struct{}

	retryMu    sync.Mutex
	retryAfter map[string]time.Time
}

// NewSolanaPayPoller creates a new poller for Solana Pay payments
func NewSolanaPayPoller(
	db *db.DB,
	redis *redis.Client,
	cfg *config.Config,
	processors config.ProcessorSet,
	solanaPayService *SolanaPayService,
	solanaTransactionService *SolanaTransactionService,
	purchaseRegistrar purchaseRegistrar,
	paymentLookup paymentLookup,
	checkoutSessionService checkoutSessionMarker,
) *SolanaPayPoller {
	var rpc *solanarpc.RPCClient
	if solanaProc := processors.GetSolanaProcessor(); solanaProc != nil {
		solanaNetwork := strings.ToLower(strings.TrimSpace(solanaProc.Network))
		if solanaNetwork == "" {
			solanaNetwork = "mainnet"
			if cfg.IsTestEnv() {
				solanaNetwork = "devnet"
			}
		}
		rpc = solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{
			HeliusAPIKey: solanaProc.HeliusAPIKey,
			Network:      solanaNetwork,
			ReadOnly:     cfg.IsProviderReadOnly(),
		})
	}

	return &SolanaPayPoller{
		db:                     db,
		redis:                  redis,
		cfg:                    cfg,
		rpc:                    rpc,
		solanaPayService:       solanaPayService,
		solanaTransactionSvc:   solanaTransactionService,
		purchaseRegistrar:      purchaseRegistrar,
		paymentLookup:          paymentLookup,
		checkoutSessionService: checkoutSessionService,
		retryAfter:             make(map[string]time.Time),
	}
}

// Start begins polling for pending payments
func (p *SolanaPayPoller) Start(ctx context.Context) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.stopCh = make(chan struct{})
	p.mu.Unlock()

	log.Info("Starting Solana Pay poller (500ms interval)")

	ticker := time.NewTicker(solanaPayPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Solana Pay poller stopped (context cancelled)")
			return
		case <-p.stopCh:
			log.Info("Solana Pay poller stopped")
			return
		case <-ticker.C:
			p.pollPendingPayments(ctx)
		}
	}
}

// Stop stops the poller
func (p *SolanaPayPoller) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running && p.stopCh != nil {
		close(p.stopCh)
		p.running = false
	}
}

// pollPendingPayments checks all pending payments for confirmation
func (p *SolanaPayPoller) pollPendingPayments(ctx context.Context) {
	if p.rpc == nil || p.solanaPayService == nil {
		return
	}

	// Get all pending payment references from Redis
	refs, err := p.solanaPayService.GetAllPendingReferences(ctx)
	if err != nil {
		log.WithError(err).Warn("Failed to get pending payment references")
		return
	}

	if len(refs) == 0 {
		return // No pending payments
	}

	log.WithField("count", len(refs)).Debug("Polling pending Solana payments")

	// Check each pending payment
	for _, ref := range refs {
		select {
		case <-ctx.Done():
			return
		default:
		}

		p.checkPayment(ctx, ref)
	}
}

// checkPayment checks a single payment reference for confirmation
func (p *SolanaPayPoller) checkPayment(ctx context.Context, reference string) {
	if !p.shouldAttempt(reference) {
		return
	}

	consumed, err := p.solanaPayService.IsReferenceConsumed(ctx, reference)
	if err != nil {
		log.WithError(err).WithField("reference", reference).Warn("Failed to check consumed Solana reference")
		p.deferRetry(reference, err)
		return
	}
	if consumed {
		if err := p.clearConsumedPendingReference(ctx, reference); err != nil {
			log.WithError(err).WithField("reference", reference).Warn("Failed to clear consumed Solana pending reference")
			p.deferRetry(reference, err)
		}
		return
	}

	// Get the pending payment details from Redis
	pending, err := p.solanaPayService.GetPendingPayment(ctx, reference)
	if err != nil {
		log.WithError(err).WithField("reference", reference).Warn("Failed to get pending payment")
		p.deferRetry(reference, err)
		return
	}
	if pending == nil {
		pending, err = p.pendingPaymentFromCheckoutSession(ctx, reference)
		if err != nil {
			log.WithError(err).WithField("reference", reference).Warn("Failed to recover pending Solana payment from checkout session")
			p.deferRetry(reference, err)
			return
		}
		if pending == nil {
			// Payment expired and no durable checkout-session record can process it.
			if err := p.solanaPayService.RemovePendingPayment(ctx, reference); err != nil {
				log.WithError(err).WithField("reference", reference).Warn("Failed to remove expired pending payment")
				p.deferRetry(reference, err)
				return
			}
			p.clearRetry(reference)
			return
		}
	}
	if strings.TrimSpace(pending.SessionID) == "" {
		log.WithField("reference", reference).Warn("Removing non-checkout-backed Solana pending payment")
		if err := p.solanaPayService.RemovePendingPayment(ctx, reference); err != nil {
			log.WithError(err).WithField("reference", reference).Warn("Failed to remove non-checkout-backed Solana pending payment")
			p.deferRetry(reference, err)
			return
		}
		p.clearRetry(reference)
		return
	}
	if err := p.attachCheckoutExpiry(ctx, reference, pending); err != nil {
		log.WithError(err).WithField("reference", reference).Warn("Failed to load checkout expiry for Solana pending payment")
		p.deferRetry(reference, err)
		return
	}

	// Query blockchain for transactions with this reference
	if err := solanarpc.ValidateAddress(reference); err != nil {
		log.WithError(err).WithField("reference", reference).Warn("Invalid Solana reference; removing pending payment")
		if removeErr := p.solanaPayService.RemovePendingPayment(ctx, reference); removeErr != nil {
			log.WithError(removeErr).WithField("reference", reference).Warn("Failed to remove invalid Solana pending payment")
			p.deferRetry(reference, removeErr)
			return
		}
		return
	}

	limit := 10
	sigs, err := p.rpc.GetSignaturesForAddress(ctx, reference, limit)
	if err != nil {
		log.WithError(err).WithField("reference", reference).Debug("Failed to get signatures for reference")
		p.deferRetry(reference, err)
		return
	}
	p.clearRetry(reference)

	if len(sigs) == 0 {
		p.deferRetryFor(reference, solanaNoTxRetryInterval)
		return // No transactions yet
	}

	// Found a transaction - verify it matches our expected payment
	for _, sig := range sigs {
		if sig.HasError {
			continue // Skip failed transactions
		}

		consumed, consumedErr := p.solanaPayService.IsReferenceConsumed(ctx, reference)
		if consumedErr != nil {
			log.WithError(consumedErr).WithField("reference", reference).Warn("Failed to re-check consumed Solana reference")
			p.deferRetry(reference, consumedErr)
			return
		}
		if consumed {
			if err := p.clearConsumedPendingReference(ctx, reference); err != nil {
				log.WithError(err).WithField("reference", reference).Warn("Failed to clear consumed Solana pending reference")
				p.deferRetry(reference, err)
			}
			return
		}

		// Verify the transaction matches our expected payment
		if p.verifyPayment(ctx, reference, sig.Signature, pending) {
			log.WithFields(log.Fields{
				"reference": reference,
				"user_id":   pending.UserID,
				"amount":    pending.Amount,
			}).Info("Solana payment confirmed")

			// Process the confirmed payment
			if err := p.processConfirmedPayment(ctx, reference, sig.Signature, pending); err != nil {
				// Recurring subscribe whose init landed but subscribe has not yet: keep
				// the reference alive (the subscribe tx carries the SAME reference) and
				// re-poll. This is normal in-progress, not a failure — do NOT finalize.
				if errors.Is(err, ErrSolanaSubscribePending) {
					log.WithField("reference", reference).Debug("Solana subscribe init landed; awaiting subscribe step")
					p.deferRetryFor(reference, solanaNoTxRetryInterval)
					return
				}
				log.WithError(err).WithField("reference", reference).Error("Failed to process confirmed payment")
				continue
			}

			if err := p.finalizeProcessedReference(ctx, reference, sig.Signature); err != nil {
				log.WithError(err).WithField("reference", reference).Warn("Failed to finalize processed Solana payment reference")
				p.deferRetry(reference, err)
				return
			}
			return
		}
	}

	// Signatures exist, but none were usable for this pending record yet; avoid hot-looping.
	p.deferRetryFor(reference, solanaUnmatchedTxRetryInterval)
}

func (p *SolanaPayPoller) attachCheckoutExpiry(ctx context.Context, reference string, pending *PendingSolanaPayment) error {
	if p.db == nil || pending == nil || !pending.ExpiresAt.IsZero() {
		return nil
	}
	sessionID, err := uuid.Parse(strings.TrimSpace(pending.SessionID))
	if err != nil {
		return fmt.Errorf("invalid checkout session id %q: %w", pending.SessionID, err)
	}
	session, err := dbrepo.NewCheckoutSessionRepo(p.db).GetByID(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.Processor != models.ProcessorSolana {
		return fmt.Errorf("checkout session %s is not a solana session", session.ID)
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) != strings.TrimSpace(reference) {
		return fmt.Errorf("checkout session %s reference does not match pending reference", session.ID)
	}
	if session.ExpiresAt != nil {
		pending.ExpiresAt = session.ExpiresAt.UTC()
	}
	return nil
}

func (p *SolanaPayPoller) pendingPaymentFromCheckoutSession(ctx context.Context, reference string) (*PendingSolanaPayment, error) {
	if p.db == nil {
		return nil, nil
	}
	session, err := dbrepo.NewCheckoutSessionRepo(p.db).GetByReference(ctx, reference)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if session.Processor != models.ProcessorSolana {
		return nil, nil
	}
	switch session.Status {
	case models.CheckoutSessionStatusRequiresAction, models.CheckoutSessionStatusExpired, models.CheckoutSessionStatusCreated:
	case models.CheckoutSessionStatusSucceeded:
		return nil, nil
	default:
		return nil, nil
	}

	// Lifecycle sessions (cancel / tier-change) carry no token quote — their
	// confirmation mirrors an on-chain action, not a transfer. Mark the pending
	// record as lifecycle so the poller skips token verification and routes the
	// confirmed signature to ConfirmSolanaLifecycleSession.
	switch session.Mode {
	case models.CheckoutSessionModeSolanaCancel, models.CheckoutSessionModeSolanaTierChange:
		pending := &PendingSolanaPayment{
			UserID:    session.CustomerID.String(),
			PriceID:   session.PriceID.String(),
			SessionID: session.ID.String(),
			Amount:    session.Amount,
			Currency:  session.Currency,
			CreatedAt: session.CreatedAt,
			Lifecycle: true,
		}
		if session.ExpiresAt != nil {
			pending.ExpiresAt = session.ExpiresAt.UTC()
		}
		return pending, nil
	case models.CheckoutSessionModeSubscription:
		// RECURRING subscribe over Solana Pay (flow=transaction_request, no token
		// quote on state — the on-chain amount lives in the subscribe instruction).
		// The confirmed reference is routed to ConfirmSolanaSubscribeSession, which
		// advances init→subscribe and enrolls once the subscription PDA is funded.
		pending := &PendingSolanaPayment{
			UserID:    session.CustomerID.String(),
			PriceID:   session.PriceID.String(),
			SessionID: session.ID.String(),
			Amount:    session.Amount,
			Currency:  session.Currency,
			CreatedAt: session.CreatedAt,
			Subscribe: true,
		}
		if session.ExpiresAt != nil {
			pending.ExpiresAt = session.ExpiresAt.UTC()
		}
		return pending, nil
	}

	token := strings.ToUpper(strings.TrimSpace(solanaStateString(session.ProcessorState, "token_symbol")))
	tokenMint := strings.TrimSpace(solanaStateString(session.ProcessorState, "token_mint"))
	recipient := strings.TrimSpace(solanaStateString(session.ProcessorState, "recipient"))
	tokenAmount := solanaStateUint64(session.ProcessorState, "token_amount")
	if token == "" || tokenMint == "" || recipient == "" || tokenAmount == 0 {
		return nil, fmt.Errorf("checkout session %s is missing solana payment state", session.ID)
	}
	pending := &PendingSolanaPayment{
		UserID:      session.CustomerID.String(),
		PriceID:     session.PriceID.String(),
		SessionID:   session.ID.String(),
		Amount:      session.Amount,
		Currency:    session.Currency,
		Token:       token,
		TokenMint:   tokenMint,
		TokenAmount: tokenAmount,
		Recipient:   recipient,
		CreatedAt:   session.CreatedAt,
	}
	if session.ExpiresAt != nil {
		pending.ExpiresAt = session.ExpiresAt.UTC()
	}
	return pending, nil
}

func solanaStateString(state map[string]any, key string) string {
	if state == nil {
		return ""
	}
	value, ok := state[key]
	if !ok || value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func solanaStateUint64(state map[string]any, key string) uint64 {
	if state == nil {
		return 0
	}
	value, ok := state[key]
	if !ok || value == nil {
		return 0
	}
	switch v := value.(type) {
	case uint64:
		return v
	case uint32:
		return uint64(v)
	case uint:
		return uint64(v)
	case int64:
		if v > 0 {
			return uint64(v)
		}
	case int:
		if v > 0 {
			return uint64(v)
		}
	case float64:
		if v > 0 {
			return uint64(v)
		}
	case string:
		if parsed, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

func (p *SolanaPayPoller) shouldAttempt(reference string) bool {
	p.retryMu.Lock()
	defer p.retryMu.Unlock()

	next, ok := p.retryAfter[reference]
	if !ok {
		return true
	}
	if time.Now().Before(next) {
		return false
	}
	return true
}

func (p *SolanaPayPoller) clearRetry(reference string) {
	p.retryMu.Lock()
	defer p.retryMu.Unlock()
	delete(p.retryAfter, reference)
}

func (p *SolanaPayPoller) deferRetry(reference string, err error) {
	if reference == "" {
		return
	}

	backoff := time.Minute
	if err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "too many requests") || strings.Contains(lower, "429") {
			backoff = 2 * time.Minute
		}
	}

	p.retryMu.Lock()
	p.retryAfter[reference] = time.Now().Add(backoff)
	p.retryMu.Unlock()
}

func (p *SolanaPayPoller) deferRetryFor(reference string, backoff time.Duration) {
	if reference == "" {
		return
	}
	if backoff <= 0 {
		backoff = time.Second
	}
	p.retryMu.Lock()
	p.retryAfter[reference] = time.Now().Add(backoff)
	p.retryMu.Unlock()
}

// verifyPayment validates that a transaction matches our expected payment
func (p *SolanaPayPoller) verifyPayment(ctx context.Context, reference string, signature string, pending *PendingSolanaPayment) bool {
	if pending != nil && (pending.Lifecycle || pending.Subscribe) {
		// Lifecycle (cancel / tier-change) and recurring-subscribe sessions have no
		// transfer to verify here; the random reference appearing on this signature
		// already proves it is a tx built for THIS session. The on-chain SUCCESS check
		// happens in the mirror invoked by processConfirmedPayment (ConfirmCancel /
		// ConfirmTierChange for lifecycle; ConfirmEnrollment — gated on the funded
		// subscription PDA — for subscribe).
		return true
	}
	if p.solanaTransactionSvc == nil {
		// Reference key is cryptographically random (32 bytes); fallback to reference-only checks.
		return true
	}
	expectedRecipient := strings.TrimSpace(pending.Recipient)
	expectedMint := strings.TrimSpace(pending.TokenMint)
	expectedAmount := pending.TokenAmount
	if expectedRecipient == "" || expectedMint == "" || expectedAmount == 0 {
		log.WithFields(log.Fields{
			"reference": reference,
			"user_id":   pending.UserID,
		}).Warn("missing expected solana payment fields; skipping verification")
		return false
	}

	var refPtr *string
	ref := strings.TrimSpace(reference)
	if ref != "" {
		refPtr = &ref
	}
	if err := p.solanaTransactionSvc.VerifyTransactionWithContent(
		ctx,
		strings.TrimSpace(signature),
		expectedAmount,
		expectedRecipient,
		expectedMint,
		"",
		refPtr,
		solanaPaymentExpiryDeadline(pending),
	); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"reference": reference,
			"user_id":   pending.UserID,
		}).Warn("solana pay verification failed")
		return false
	}

	return true
}

func solanaPaymentExpiryDeadline(pending *PendingSolanaPayment) *time.Time {
	if pending == nil || pending.ExpiresAt.IsZero() {
		return nil
	}
	expiresAt := pending.ExpiresAt.UTC()
	return &expiresAt
}

// processConfirmedPayment uses CheckoutService.RegisterPurchase to record payment and grant entitlements
func (p *SolanaPayPoller) processConfirmedPayment(ctx context.Context, reference, signature string, pending *PendingSolanaPayment) error {
	// Lifecycle (cancel / tier-change): mirror the confirmed on-chain action via
	// the checkout session service instead of registering a purchase. The mirror
	// re-confirms the signature LANDED + SUCCEEDED on-chain (source of truth) and
	// is idempotent.
	if pending.Lifecycle {
		if p.checkoutSessionService == nil {
			return fmt.Errorf("checkout session service is not configured")
		}
		sessionID, err := uuid.Parse(strings.TrimSpace(pending.SessionID))
		if err != nil {
			return fmt.Errorf("invalid checkout session id %q: %w", pending.SessionID, err)
		}
		if err := p.checkoutSessionService.ConfirmSolanaLifecycleSession(ctx, sessionID, signature); err != nil {
			return err
		}
		log.WithFields(log.Fields{
			"session_id": pending.SessionID,
			"reference":  reference,
		}).Info("Mirrored confirmed Solana lifecycle (cancel/tier-change) checkout session")
		return nil
	}

	// Recurring subscribe over Solana Pay: advance the init→subscribe step and, once
	// the subscription PDA is funded (the atomic [subscribe+transfer] landed), enroll
	// the membership + mark the session succeeded. ConfirmSolanaSubscribeSession is
	// idempotent and returns ErrSolanaSubscribePending while only init has landed —
	// the caller keeps the reference alive (does NOT consume it) and re-polls for the
	// subscribe tx, which carries the SAME reference.
	if pending.Subscribe {
		if p.checkoutSessionService == nil {
			return fmt.Errorf("checkout session service is not configured")
		}
		sessionID, err := uuid.Parse(strings.TrimSpace(pending.SessionID))
		if err != nil {
			return fmt.Errorf("invalid checkout session id %q: %w", pending.SessionID, err)
		}
		if err := p.checkoutSessionService.ConfirmSolanaSubscribeSession(ctx, sessionID, signature); err != nil {
			return err
		}
		log.WithFields(log.Fields{
			"session_id": pending.SessionID,
			"reference":  reference,
		}).Info("Enrolled confirmed Solana recurring subscribe checkout session")
		return nil
	}

	priceID, err := uuid.Parse(pending.PriceID)
	if err != nil {
		return err
	}

	if p.purchaseRegistrar == nil || p.paymentLookup == nil {
		return fmt.Errorf("checkout payment service is not configured")
	}

	// Fast idempotency guard: skip processing if this signature is already recorded.
	existingPayment, err := p.paymentLookup.GetByTransactionID(ctx, models.ProcessorSolana, signature)
	if err != nil && !repo.IsNotFound(err) {
		return fmt.Errorf("failed checking existing payment by transaction id: %w", err)
	}
	if err == nil {
		if !solanaPaymentMatchesPending(existingPayment, reference, pending) {
			log.WithFields(log.Fields{
				"payment_id": existingPayment.ID,
				"reference":  reference,
			}).Warn("Solana signature already belongs to a different pending payment; not marking checkout succeeded")
			return nil
		}
		log.WithFields(log.Fields{
			"payment_id": existingPayment.ID,
			"reference":  reference,
		}).Info("Solana payment already processed, skipping duplicate")
		p.markCheckoutSessionSucceeded(ctx, pending, existingPayment.ID, signature)
		return nil
	}

	// Use the unified RegisterPurchase to record payment and grant entitlements
	result, err := p.purchaseRegistrar.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:         pending.UserID,
		PriceID:        priceID,
		Processor:      "solana",
		TransactionID:  signature,
		Amount:         pending.Amount,
		Currency:       pending.Currency,
		WalletPurchase: true,
		Metadata: map[string]any{
			"solana_reference":    reference,
			"checkout_session_id": strings.TrimSpace(pending.SessionID),
			"solana_token_symbol": pending.Token,
			"solana_token_mint":   pending.TokenMint,
			"solana_token_amount": pending.TokenAmount,
			"solana_recipient":    pending.Recipient,
		},
	})

	if err != nil {
		// Race-safe idempotency: if another worker inserted first, treat as success.
		if isDuplicatePaymentTransactionIDError(err) {
			existingPayment, getErr := p.paymentLookup.GetByTransactionID(ctx, models.ProcessorSolana, signature)
			if getErr == nil {
				if !solanaPaymentMatchesPending(existingPayment, reference, pending) {
					log.WithFields(log.Fields{
						"payment_id": existingPayment.ID,
						"reference":  reference,
					}).Warn("Concurrent Solana duplicate belongs to a different pending payment; not marking checkout succeeded")
					return nil
				}
				log.WithFields(log.Fields{
					"payment_id": existingPayment.ID,
					"reference":  reference,
				}).Info("Solana payment already processed by concurrent worker, skipping duplicate")
				p.markCheckoutSessionSucceeded(ctx, pending, existingPayment.ID, signature)
				return nil
			}
		}
		return err
	}

	log.WithFields(log.Fields{
		"payment_id":   result.PaymentID,
		"user_id":      pending.UserID,
		"price_id":     priceID,
		"token":        pending.Token,
		"reference":    reference,
		"entitlements": result.Entitlements,
	}).Info("Processed Solana Pay payment via RegisterPurchase")

	p.markCheckoutSessionSucceeded(ctx, pending, result.PaymentID, signature)

	return nil
}

func solanaPaymentMatchesPending(payment *models.Payment, reference string, pending *PendingSolanaPayment) bool {
	if payment == nil || pending == nil {
		return false
	}
	if strings.TrimSpace(pending.SessionID) == "" {
		return false
	}
	priceID, err := uuid.Parse(strings.TrimSpace(pending.PriceID))
	if err != nil {
		return false
	}
	if payment.CustomerID != identity.CustomerIDFromString(pending.UserID).UUID() || payment.PriceID != priceID || payment.Amount != pending.Amount || !strings.EqualFold(payment.Currency, pending.Currency) {
		return false
	}
	if strings.TrimSpace(fmt.Sprint(payment.Metadata["solana_reference"])) != strings.TrimSpace(reference) {
		return false
	}
	if strings.TrimSpace(fmt.Sprint(payment.Metadata["checkout_session_id"])) != strings.TrimSpace(pending.SessionID) {
		return false
	}
	return true
}

func (p *SolanaPayPoller) markCheckoutSessionSucceeded(ctx context.Context, pending *PendingSolanaPayment, paymentID uuid.UUID, signature string) {
	if pending == nil || pending.SessionID == "" || p.checkoutSessionService == nil {
		return
	}
	sessionID, err := uuid.Parse(pending.SessionID)
	if err != nil {
		log.WithError(err).WithField("session_id", pending.SessionID).Warn("Invalid checkout session ID on pending Solana payment")
		return
	}
	if err := p.checkoutSessionService.MarkSucceeded(ctx, sessionID, paymentID, signature); err != nil {
		log.WithError(err).WithField("session_id", pending.SessionID).Warn("Failed to update checkout session for Solana payment")
	}
}

func isDuplicatePaymentTransactionIDError(err error) bool {
	if err == nil {
		return false
	}
	// SQLSTATE 23505 = unique_violation. Checked structurally so it works
	// across drivers (pgx *pgconn.PgError, bun's pgdriver, lib/pq all expose
	// SQLState()); the message check remains as a fallback.
	var stateErr interface{ SQLState() string }
	if errors.As(err, &stateErr) && stateErr.SQLState() == "23505" {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "payments_processor_transaction_id_key") ||
		(strings.Contains(msg, "duplicate key value") && strings.Contains(msg, "sqlstate=23505"))
}

func (p *SolanaPayPoller) finalizeProcessedReference(ctx context.Context, reference, transactionID string) error {
	if p.solanaPayService == nil {
		return nil
	}
	if err := p.solanaPayService.ConsumeAndRemovePending(ctx, reference, transactionID); err != nil {
		return err
	}
	p.clearRetry(reference)
	return nil
}

func (p *SolanaPayPoller) clearConsumedPendingReference(ctx context.Context, reference string) error {
	if p.solanaPayService == nil {
		return nil
	}
	if err := p.solanaPayService.RemovePendingPayment(ctx, reference); err != nil {
		return err
	}
	p.clearRetry(reference)
	return nil
}
