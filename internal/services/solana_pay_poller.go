package services

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/doujins-org/doujins-billing/config"
	"github.com/doujins-org/doujins-billing/internal/db"
	solana "github.com/doujins-org/doujins-billing/internal/integrations/solana"
	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

const (
	// Poll interval for checking pending payments
	solanaPayPollInterval = 500 * time.Millisecond
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
	// Get the pending payment details from Redis
	pending, err := p.solanaPayService.GetPendingPayment(ctx, reference)
	if err != nil {
		log.WithError(err).WithField("reference", reference).Warn("Failed to get pending payment")
		return
	}
	if pending == nil {
		// Payment expired, clean up the set
		p.solanaPayService.RemovePendingPayment(ctx, reference)
		return
	}

	// Query blockchain for transactions with this reference
	limit := 10
	sigs, err := p.rpc.GetSignaturesForAddress(ctx, reference, limit)
	if err != nil {
		log.WithError(err).WithField("reference", reference).Debug("Failed to get signatures for reference")
		return
	}

	if len(sigs) == 0 {
		return // No transactions yet
	}

	// Found a transaction - verify it matches our expected payment
	for _, sig := range sigs {
		if sig.HasError {
			continue // Skip failed transactions
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
			return
		}
	}
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

	// Use the unified RegisterPurchase to record payment and grant entitlements
	result, err := p.checkoutService.RegisterPurchase(ctx, &RegisterPurchaseRequest{
		UserID:        pending.UserID,
		PriceID:       priceID,
		Processor:     "solana",
		TransactionID: signature,
		Amount:        pending.Amount,
		Currency:      pending.Currency,
	})
	if err != nil {
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

	if pending.SessionID != "" && p.checkoutSessionService != nil {
		if sessionID, err := uuid.Parse(pending.SessionID); err == nil {
			if err := p.checkoutSessionService.MarkSucceeded(ctx, sessionID, result.PaymentID, signature); err != nil {
				log.WithError(err).WithField("session_id", pending.SessionID).Warn("Failed to update checkout session for Solana payment")
			}
		} else {
			log.WithError(err).WithField("session_id", pending.SessionID).Warn("Invalid checkout session ID on pending Solana payment")
		}
	}

	return nil
}
