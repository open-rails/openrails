package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
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

// SolanaPayPoller polls the blockchain for confirmed Solana Pay payments
type SolanaPayPoller struct {
	db                     *db.DB
	redis                  *redis.Client
	cfg                    *config.Config
	rpc                    *solana.RPCClient
	solanaPayService       *SolanaPayService
	solanaTransactionSvc   *SolanaTransactionService
	checkoutService        *CheckoutService
	checkoutSessionService *CheckoutSessionService

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
	solanaPayService *SolanaPayService,
	solanaTransactionService *SolanaTransactionService,
	checkoutService *CheckoutService,
	checkoutSessionService *CheckoutSessionService,
) *SolanaPayPoller {
	var rpc *solana.RPCClient
	if solanaProc := cfg.GetSolanaProcessor(); solanaProc != nil {
		rpc = solana.NewRPCClient(solanaProc.RPCEndpoint, solanaProc.Network)
	}

	return &SolanaPayPoller{
		db:                     db,
		redis:                  redis,
		cfg:                    cfg,
		rpc:                    rpc,
		solanaPayService:       solanaPayService,
		solanaTransactionSvc:   solanaTransactionService,
		checkoutService:        checkoutService,
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

	// Get the pending payment details from Redis
	pending, err := p.solanaPayService.GetPendingPayment(ctx, reference)
	if err != nil {
		log.WithError(err).WithField("reference", reference).Warn("Failed to get pending payment")
		p.deferRetry(reference, err)
		return
	}
	if pending == nil {
		// Payment expired, clean up the set
		p.solanaPayService.RemovePendingPayment(ctx, reference)
		p.clearRetry(reference)
		return
	}

	// Query blockchain for transactions with this reference
	if err := solana.ValidateAddress(reference); err != nil {
		log.WithError(err).WithField("reference", reference).Warn("Invalid Solana reference; removing pending payment")
		p.solanaPayService.RemovePendingPayment(ctx, reference)
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

		// Idempotency short-circuit: if this signature is already recorded, stop polling this reference.
		if p.checkoutService != nil && p.checkoutService.PaymentService != nil {
			existingPayment, getErr := p.checkoutService.PaymentService.GetByTransactionID(ctx, models.ProcessorSolana, sig.Signature)
			if getErr == nil && existingPayment != nil {
				log.WithFields(log.Fields{
					"reference":  reference,
					"signature":  sig.Signature,
					"payment_id": existingPayment.ID,
				}).Info("Solana reference already has a recorded payment; removing pending payment")
				p.markCheckoutSessionSucceeded(ctx, pending, existingPayment.ID, sig.Signature)
				p.solanaPayService.RemovePendingPayment(ctx, reference)
				p.clearRetry(reference)
				return
			}
			if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
				log.WithError(getErr).WithFields(log.Fields{
					"reference": reference,
					"signature": sig.Signature,
				}).Warn("Failed to check existing payment by signature")
			}
		}

		// Verify the transaction matches our expected payment
		if p.verifyPayment(ctx, reference, sig.Signature, pending) {
			log.WithFields(log.Fields{
				"reference": reference,
				"signature": sig.Signature,
				"user_id":   pending.UserID,
				"amount":    pending.Amount,
			}).Info("Solana payment confirmed")

			// Process the confirmed payment
			if err := p.processConfirmedPayment(ctx, reference, sig.Signature, pending); err != nil {
				log.WithError(err).WithField("reference", reference).Error("Failed to process confirmed payment")
				continue
			}

			// Remove from pending set
			p.solanaPayService.RemovePendingPayment(ctx, reference)
			p.clearRetry(reference)
			return
		}
	}

	// Signatures exist, but none were usable for this pending record yet; avoid hot-looping.
	p.deferRetryFor(reference, solanaUnmatchedTxRetryInterval)
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
	); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"reference": reference,
			"signature": signature,
			"user_id":   pending.UserID,
		}).Warn("solana pay verification failed")
		return false
	}

	return true
}

// processConfirmedPayment uses CheckoutService.RegisterPurchase to record payment and grant entitlements
func (p *SolanaPayPoller) processConfirmedPayment(ctx context.Context, reference, signature string, pending *PendingSolanaPayment) error {
	priceID, err := uuid.Parse(pending.PriceID)
	if err != nil {
		return err
	}

	if p.checkoutService == nil || p.checkoutService.PaymentService == nil {
		return fmt.Errorf("checkout payment service is not configured")
	}

	// Fast idempotency guard: skip processing if this signature is already recorded.
	existingPayment, err := p.checkoutService.PaymentService.GetByTransactionID(ctx, models.ProcessorSolana, signature)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("failed checking existing payment for signature %s: %w", signature, err)
	}
	if err == nil && existingPayment != nil {
		log.WithFields(log.Fields{
			"payment_id": existingPayment.ID,
			"reference":  reference,
			"signature":  signature,
		}).Info("Solana payment already processed, skipping duplicate")
		p.markCheckoutSessionSucceeded(ctx, pending, existingPayment.ID, signature)
		return nil
	}

	// Use the unified RegisterPurchase to record payment and grant entitlements
	result, err := p.checkoutService.RegisterPurchase(ctx, &RegisterPurchaseRequest{
		UserID:         pending.UserID,
		PriceID:        priceID,
		Processor:      "solana",
		TransactionID:  signature,
		Amount:         pending.Amount,
		Currency:       pending.Currency,
		WalletPurchase: true,
	})

	if err != nil {
		// Race-safe idempotency: if another worker inserted first, treat as success.
		if isDuplicatePaymentTransactionIDError(err) {
			existingPayment, getErr := p.checkoutService.PaymentService.GetByTransactionID(ctx, models.ProcessorSolana, signature)
			if getErr == nil && existingPayment != nil {
				log.WithFields(log.Fields{
					"payment_id": existingPayment.ID,
					"reference":  reference,
					"signature":  signature,
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
		"signature":    signature,
		"token":        pending.Token,
		"reference":    reference,
		"entitlements": result.Entitlements,
	}).Info("Processed Solana Pay payment via RegisterPurchase")

	p.markCheckoutSessionSucceeded(ctx, pending, result.PaymentID, signature)

	return nil
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
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "payments_processor_transaction_id_key") ||
		(strings.Contains(msg, "duplicate key value") && strings.Contains(msg, "sqlstate=23505"))
}
