package solana

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/timeutil"
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
	// signaturePage is one history read; pagesPerPass bounds one reference's
	// share of a pass. The walk resumes from its stored cursor, so the whole
	// history is always read however many transactions name the reference.
	signaturePage = 1000
	pagesPerPass  = 10
	// observeTimeout bounds one transaction read; a node that has not caught
	// up is asked again on a later pass.
	observeTimeout = 30 * time.Second
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
	// ConfirmSolanaSubscribeSession enrolls a recurring subscribe once the
	// signature is verified as its first payment, returning
	// ErrSolanaSubscribePending while the transaction is not yet readable.
	// Idempotent.
	ConfirmSolanaSubscribeSession(ctx context.Context, sessionID uuid.UUID, signature string) error
}

// ErrSolanaSubscribePending: a recurring subscribe's payment is not yet
// readable on-chain, or no wallet has bound the session. The reference stays
// pending.
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
	byMerchant := map[uuid.UUID][]gen.BillingSolanaPayReference{}
	for _, r := range refs {
		byMerchant[r.MerchantID] = append(byMerchant[r.MerchantID], r)
	}
	for mid, group := range byMerchant {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		mctx := merchant.WithID(ctx, billing.MerchantID(mid))
		if err := p.db.RunInMerchantConn(mctx, func(ctx context.Context) error {
			p.pollMerchant(ctx, billing.MerchantID(mid), group)
			return nil
		}); err != nil {
			log.WithError(err).WithField("merchant_id", mid.String()).Warn("Solana Pay merchant pass failed")
		}
	}
	return nil
}

func (p *SolanaPayPoller) pollMerchant(ctx context.Context, mid billing.MerchantID, refs []gen.BillingSolanaPayReference) {
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
func (p *SolanaPayPoller) check(ctx context.Context, rpc *solanarpc.RPCClient, ledger *PayLedger, ref gen.BillingSolanaPayReference) (time.Duration, error) {
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
	var readErr error
	if ReferenceKind(ref.Kind) == ReferencePurchase {
		readErr = p.settlePurchase(ctx, rpc, ledger, ref, session)
	} else {
		sigs, err := rpc.SignaturesBetween(ctx, ref.Reference, "", "", signaturePage)
		if err != nil {
			readErr = err
		} else {
			var awaiting bool
			awaiting, readErr = p.confirmMirror(ctx, ledger, ref, sigs)
			if awaiting && readErr == nil {
				return pendingRecheck, nil
			}
		}
	}
	// Expiry never waits on the chain read: settlement judges a transfer by
	// when it landed, so an expired reference still credits an on-time one.
	now := p.clock.Now()
	if _, err := ledger.Expire(ctx, ref.Reference, now); err != nil {
		return 0, errors.Join(readErr, err)
	}
	if readErr != nil {
		return 0, readErr
	}
	current, err := ledger.Get(ctx, ref.Reference)
	if err != nil {
		return 0, err
	}
	if current.Status == ReferencePending || len(current.ScanStack) > 0 {
		return pendingRecheck, nil
	}
	return watchInterval(now.Sub(current.CreatedAt)), nil
}

// errHistoryGap: the node answering does not hold the history a page below a
// cursor must come from, so its short or empty answer proves nothing.
var errHistoryGap = errors.New("solana: node does not hold the reference's history; read again later")

// settlePurchase walks the reference's whole signature history, oldest
// first, and hands every signature not yet recorded to the checkout: the
// first transfer that settles the reference is credited and every other one
// is recorded. The walk's position is stored, so a pass resumes where the last
// one stopped and no signature is skipped however many name the reference.
//
// The walk goes down from the newest signature one full page at a time,
// pushing each page's oldest signature as the next cursor, until a page is
// short: that page is complete, so it is processed and its cursor popped. A
// full page is complete only when it ends at the cursor just walked below;
// otherwise newer signatures arrived and the walk goes down again. A short
// answer is trusted only from a node that holds both its cursor and the
// newest signature already processed.
func (p *SolanaPayPoller) settlePurchase(ctx context.Context, rpc *solanarpc.RPCClient, ledger *PayLedger, ref gen.BillingSolanaPayReference, session *models.CheckoutSession) error {
	known, err := ledger.KnownSignatures(ctx, ref.Reference)
	if err != nil {
		return err
	}
	seen, below, stack := "", "", append([]string{}, ref.ScanStack...)
	if ref.SeenUntil != nil {
		seen = *ref.SeenUntil
	}
	if ref.ScanBelow != nil {
		below = *ref.ScanBelow
	}
	if len(stack) == 0 {
		stack, below = []string{""}, ""
	}
	save := func() error {
		return ledger.SaveScan(ctx, ref.Reference, nonEmpty(seen), stack, nonEmpty(below))
	}
	for range pagesPerPass {
		cursor := stack[len(stack)-1]
		page, err := rpc.SignaturesBetween(ctx, ref.Reference, cursor, seen, signaturePage)
		if err != nil {
			return errors.Join(err, save())
		}
		page = distinctSignatures(page)
		full := len(page) >= signaturePage
		if full && (below == "" || page[len(page)-1].Signature != below) {
			stack, below = append(stack, page[len(page)-1].Signature), ""
			continue
		}
		if !full {
			if proof := nonEmptyAll(cursor, seen); len(proof) > 0 {
				held, err := rpc.SignaturesKnown(ctx, proof...)
				if err != nil {
					return errors.Join(err, save())
				}
				if !held {
					return errors.Join(errHistoryGap, save())
				}
			}
		}
		for i := len(page) - 1; i >= 0; i-- {
			if page[i].HasError || known[page[i].Signature] {
				continue
			}
			if err := p.settleSignature(ctx, rpc, ref, session, page[i].Signature); err != nil {
				return errors.Join(err, save())
			}
			known[page[i].Signature] = true
		}
		if len(page) > 0 {
			seen = page[0].Signature
		}
		stack, below = stack[:len(stack)-1], cursor
		if len(stack) == 0 {
			below = ""
			break
		}
	}
	return save()
}

// distinctSignatures drops a signature a node listed twice.
func distinctSignatures(page []solanarpc.SignatureInfo) []solanarpc.SignatureInfo {
	seen := make(map[string]bool, len(page))
	out := page[:0]
	for _, s := range page {
		if !seen[s.Signature] {
			seen[s.Signature] = true
			out = append(out, s)
		}
	}
	return out
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonEmptyAll(values ...string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// settleSignature reads one transaction and records it through the checkout.
// Only an error worth retrying is returned: a transaction that is foreign,
// failed or unreadable is recorded as such.
func (p *SolanaPayPoller) settleSignature(ctx context.Context, rpc *solanarpc.RPCClient, ref gen.BillingSolanaPayReference, session *models.CheckoutSession, signature string) error {
	policy := solanarpc.MemoRequired
	if stateString(session.RailState, "flow") == "transfer_request" {
		policy = solanarpc.MemoPresenceOptional
	}
	octx, cancel := context.WithTimeout(ctx, observeTimeout)
	obs, err := rpc.ObserveTransfer(octx, solanarpc.ObserveTransferRequest{
		Signature: signature, Recipient: stateString(session.RailState, "recipient"), TokenMint: stateString(session.RailState, "token_mint"),
		Reference: ref.Reference, MemoLocalID: session.ID, MemoPolicy: policy,
	})
	cancel()
	t := ObservedTransfer{Signature: signature}
	switch {
	case errors.Is(err, solanarpc.ErrForeignTransfer), errors.Is(err, solanarpc.ErrFailedOnChain):
		t.Foreign = true
	case errors.Is(err, solanarpc.ErrUnreadableTransfer):
		t.Unreadable = true
	case err != nil:
		return err
	default:
		t.Amount, t.Other, t.Payer, t.LandedAt = obs.Amount, obs.Other, obs.Payer, obs.LandedAt
	}
	receipt, err := p.checkout.SettleSolanaTransfer(ctx, ref.Reference, t)
	if err != nil {
		return err
	}
	if receipt.ReviewReason != "" {
		log.WithFields(log.Fields{"reference": ref.Reference, "signature": signature, "disposition": receipt.Disposition, "reason": receipt.ReviewReason}).
			Warn("Solana transfer recorded for review")
	}
	return nil
}

// confirmMirror routes a landed cancel, tier change or subscribe to its
// session mirror. awaiting reports a subscribe whose funded step has not landed.
func (p *SolanaPayPoller) confirmMirror(ctx context.Context, ledger *PayLedger, ref gen.BillingSolanaPayReference, sigs []solanarpc.SignatureInfo) (bool, error) {
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

// watchInterval spaces reads of a settled reference out as it ages: minutes
// at first, six hours near the end of its watch window.
func watchInterval(age time.Duration) time.Duration {
	return min(max(age/4, time.Minute), 6*time.Hour)
}

func stateString(state map[string]any, key string) string {
	v, _ := state[key].(string)
	return strings.TrimSpace(v)
}
