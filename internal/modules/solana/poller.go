package solana

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	solanaPayPollInterval = 500 * time.Millisecond
	// pollBatch bounds one replica's claim per tick; pollLease is how long a
	// claimed reference stays with this replica if it dies mid-read.
	pollBatch = 100
	pollLease = time.Minute
	// pendingRecheck is the cadence while a transfer is awaited.
	pendingRecheck = 3 * time.Second
	// errorRecheck backs off a reference whose read failed.
	errorRecheck = time.Minute
	// signaturePage and maxSignatures bound one reference's history walk, so
	// a reference spammed with foreign transactions cannot hide a payment
	// behind a single page yet cannot make a pass unbounded either.
	signaturePage = 1000
	maxSignatures = 5 * signaturePage
)

// CheckoutSettler is the checkout side of a landed Solana Pay transaction.
// Implemented by *checkout.CheckoutSessionService.
type CheckoutSettler interface {
	// SettleSolanaTransfer records one transfer on a purchase reference
	// exactly once and credits the checkout when the transfer settles it.
	SettleSolanaTransfer(ctx context.Context, reference string, t ObservedTransfer) (*Receipt, error)
	// ConfirmSolanaLifecycleSession mirrors a confirmed on-chain cancel or
	// tier change. Idempotent.
	ConfirmSolanaLifecycleSession(ctx context.Context, sessionID uuid.UUID, signature string) error
	// ConfirmSolanaSubscribeSession enrolls a recurring subscribe once its
	// subscription is funded, returning ErrSolanaSubscribePending while only
	// the init transaction has landed. Idempotent.
	ConfirmSolanaSubscribeSession(ctx context.Context, sessionID uuid.UUID, signature string) error
}

// ErrSolanaSubscribePending: a recurring subscribe's init transaction landed
// but the funded subscribe has not. The reference stays pending.
var ErrSolanaSubscribePending = errors.New("solana: recurring subscribe pending subscribe step")

// SolanaPayPoller reads the chain for every watched reference. Replicas share
// the work through ClaimDue; all state is in PostgreSQL.
type SolanaPayPoller struct {
	db         *db.DB
	rpcBuilder *MerchantRPCBuilder
	checkout   CheckoutSettler
	clock      clockwork.Clock

	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
}

func NewSolanaPayPoller(database *db.DB, checkout CheckoutSettler, clocks ...clockwork.Clock) *SolanaPayPoller {
	return &SolanaPayPoller{db: database, checkout: checkout, rpcBuilder: &MerchantRPCBuilder{}, clock: timeutil.FirstClock(clocks...)}
}

// SetMerchantRPC installs the store-aware per-merchant RPC builder (#728).
func (p *SolanaPayPoller) SetMerchantRPC(b *MerchantRPCBuilder) {
	if b != nil {
		p.rpcBuilder = b
	}
}

func (p *SolanaPayPoller) Start(ctx context.Context) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.stopCh = make(chan struct{})
	stop := p.stopCh
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.stopCh == stop {
			p.running = false
		}
		p.mu.Unlock()
	}()

	ticker := time.NewTicker(solanaPayPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			if err := p.PollOnce(ctx); err != nil && ctx.Err() == nil {
				log.WithError(err).Warn("Solana Pay poll failed")
			}
		}
	}
}

func (p *SolanaPayPoller) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running && p.stopCh != nil {
		close(p.stopCh)
		p.running = false
	}
}

// PollOnce claims the due references and reads each once, per merchant.
func (p *SolanaPayPoller) PollOnce(ctx context.Context) error {
	now := p.clock.Now()
	refs, err := ClaimDue(ctx, p.db, now, now.Add(pollLease), pollBatch)
	if err != nil || len(refs) == 0 {
		return err
	}
	byMerchant := map[uuid.UUID][]gen.OpenrailsSolanaPayReference{}
	for _, r := range refs {
		byMerchant[r.MerchantID] = append(byMerchant[r.MerchantID], r)
	}
	for mid, group := range byMerchant {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		if err := p.db.RunInMerchantConn(mctx, func(ctx context.Context) error {
			p.pollMerchant(ctx, merchant.ID(mid), group)
			return nil
		}); err != nil {
			log.WithError(err).WithField("merchant_id", mid.String()).Warn("Solana Pay merchant pass failed")
		}
	}
	return nil
}

func (p *SolanaPayPoller) pollMerchant(ctx context.Context, mid merchant.ID, refs []gen.OpenrailsSolanaPayReference) {
	ledger := NewPayLedger(p.db)
	rpc, err := p.rpcBuilder.Resolve(ctx, mid)
	if rpc == nil {
		log.WithError(err).WithField("merchant_id", mid.String()).Warn("Solana Pay pass deferred: merchant RPC not armed")
		for _, r := range refs {
			_ = ledger.Schedule(ctx, r.Reference, p.clock.Now().Add(errorRecheck))
		}
		return
	}
	for _, r := range refs {
		if ctx.Err() != nil {
			return
		}
		next, err := p.check(ctx, rpc, ledger, r)
		if err != nil {
			log.WithError(err).WithFields(log.Fields{"merchant_id": mid.String(), "reference": r.Reference}).Warn("Solana Pay reference check failed")
			next = errorRecheck
		}
		if err := ledger.Schedule(ctx, r.Reference, p.clock.Now().Add(next)); err != nil {
			log.WithError(err).WithField("reference", r.Reference).Warn("Solana Pay reschedule failed")
		}
	}
}

// check reads one reference and returns when to read it again.
func (p *SolanaPayPoller) check(ctx context.Context, rpc *solanarpc.RPCClient, ledger *PayLedger, ref gen.OpenrailsSolanaPayReference) (time.Duration, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	row, err := p.db.Gen(ctx).GetCheckoutSessionByID(ctx, gen.GetCheckoutSessionByIDParams{MerchantID: mid.UUID(), ID: ref.CheckoutSessionID})
	if err != nil {
		return 0, err
	}
	session, err := models.CheckoutSessionFromGen(row)
	if err != nil {
		return 0, err
	}
	sigs, err := referenceSignatures(ctx, rpc, ref.Reference)
	if err != nil {
		return 0, err
	}
	if ReferenceKind(ref.Kind) == ReferencePurchase {
		err = p.settlePurchase(ctx, rpc, ledger, ref, session, sigs)
	} else {
		var awaiting bool
		awaiting, err = p.confirmMirror(ctx, ledger, ref, sigs)
		if awaiting {
			return pendingRecheck, nil
		}
	}
	if err != nil {
		return 0, err
	}
	now := p.clock.Now()
	if ref.Status == ReferencePending && now.After(ref.SettleUntil) {
		if _, err := ledger.Expire(ctx, ref.Reference, now); err != nil {
			return 0, err
		}
	}
	current, err := ledger.Get(ctx, ref.Reference)
	if err != nil {
		return 0, err
	}
	if current.Status == ReferencePending {
		return pendingRecheck, nil
	}
	return watchInterval(now.Sub(current.CreatedAt)), nil
}

// settlePurchase hands every signature not yet recorded, oldest first, to the
// checkout: the first transfer that settles the reference is credited and
// every later one is recorded for review.
func (p *SolanaPayPoller) settlePurchase(ctx context.Context, rpc *solanarpc.RPCClient, ledger *PayLedger, ref gen.OpenrailsSolanaPayReference, session *models.CheckoutSession, sigs []solanarpc.SignatureInfo) error {
	known, err := ledger.KnownSignatures(ctx, ref.Reference)
	if err != nil {
		return err
	}
	recipient := stateString(session.RailState, "recipient")
	mint := stateString(session.RailState, "token_mint")
	expected, _ := strconv.ParseUint(stateString(session.RailState, "token_amount"), 10, 64)
	policy := solanarpc.MemoRequired
	if stateString(session.RailState, "flow") == "transfer_request" {
		policy = solanarpc.MemoPresenceOptional
	}
	for i := len(sigs) - 1; i >= 0; i-- {
		sig := sigs[i]
		if sig.HasError || known[sig.Signature] {
			continue
		}
		obs, err := rpc.ObserveTransfer(ctx, solanarpc.ObserveTransferRequest{
			Signature: sig.Signature, Recipient: recipient, TokenMint: mint, Reference: ref.Reference,
			MemoLocalID: session.ID, MemoPolicy: policy,
		})
		if errors.Is(err, solanarpc.ErrForeignTransfer) {
			if err := ledger.Record(ctx, Receipt{Reference: ref.Reference, Signature: sig.Signature, SessionID: session.ID, Disposition: Ignored, TokenMint: mint, ExpectedAmount: expected}, p.clock.Now()); err != nil && !errors.Is(err, ErrSignatureClaimed) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		receipt, err := p.checkout.SettleSolanaTransfer(ctx, ref.Reference, ObservedTransfer{Signature: sig.Signature, Amount: obs.Amount, Payer: obs.Payer, LandedAt: obs.LandedAt})
		if errors.Is(err, ErrSignatureClaimed) {
			log.WithFields(log.Fields{"reference": ref.Reference, "signature": sig.Signature}).Warn("Solana signature already settled another reference")
			continue
		}
		if err != nil {
			return err
		}
		if receipt.ReviewReason != "" {
			log.WithFields(log.Fields{"reference": ref.Reference, "signature": sig.Signature, "disposition": receipt.Disposition, "reason": receipt.ReviewReason}).
				Warn("Solana transfer recorded for review")
		}
	}
	return nil
}

// confirmMirror routes a landed cancel, tier change or subscribe to its
// session mirror. awaiting reports a subscribe whose funded step has not landed.
func (p *SolanaPayPoller) confirmMirror(ctx context.Context, ledger *PayLedger, ref gen.OpenrailsSolanaPayReference, sigs []solanarpc.SignatureInfo) (bool, error) {
	if ref.Status != ReferencePending {
		return false, nil
	}
	for _, sig := range sigs {
		if sig.HasError {
			continue
		}
		var err error
		if ReferenceKind(ref.Kind) == ReferenceSubscribe {
			err = p.checkout.ConfirmSolanaSubscribeSession(ctx, ref.CheckoutSessionID, sig.Signature)
		} else {
			err = p.checkout.ConfirmSolanaLifecycleSession(ctx, ref.CheckoutSessionID, sig.Signature)
		}
		if errors.Is(err, ErrSolanaSubscribePending) {
			return true, nil
		}
		if err != nil {
			log.WithError(err).WithFields(log.Fields{"reference": ref.Reference, "signature": sig.Signature}).Warn("Solana session mirror failed")
			continue
		}
		_, err = ledger.Confirm(ctx, ref.Reference, sig.Signature, p.clock.Now())
		return false, err
	}
	return false, nil
}

// referenceSignatures lists the finalized signatures naming the reference,
// newest first.
func referenceSignatures(ctx context.Context, rpc *solanarpc.RPCClient, reference string) ([]solanarpc.SignatureInfo, error) {
	var out []solanarpc.SignatureInfo
	before := ""
	for len(out) < maxSignatures {
		page, err := rpc.GetSignaturesForAddressPage(ctx, reference, before, signaturePage)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < signaturePage {
			break
		}
		before = page[len(page)-1].Signature
	}
	return out, nil
}

// watchInterval spaces reads of a settled reference out as it ages: minutes
// at first, six hours near the end of its watch window.
func watchInterval(age time.Duration) time.Duration {
	return min(max(age/4, time.Minute), 6*time.Hour)
}

func stateString(state map[string]any, key string) string {
	v, _ := state[key].(string)
	return strings.TrimSpace(v)
}
