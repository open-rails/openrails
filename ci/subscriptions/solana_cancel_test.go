//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/failpoint"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/intents"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// A Solana subscription cancels like a card one: OpenRails stops pulling at
// period end, undoable until then, with no wallet signature.

// subscribed is a buyer holding the shop's plan.
func (s *solanaShop) subscribed(t *testing.T) (*solanaBuyer, billing.SubscriptionID) {
	t.Helper()
	b := s.buyer(t, false)
	c := s.purchase(t, b, b.wallet.PublicKey())
	done, err := b.confirm(c, s.land(t, signAs(t, c.bundle, b.wallet), s.w.clock.Now()))
	require.NoError(t, err)
	require.NotNil(t, done.SubscriptionID)
	return b, *done.SubscriptionID
}

func (b *solanaBuyer) cancel(sub billing.SubscriptionID, body map[string]any) (int, map[string]any) {
	b.w.t.Helper()
	return b.call(http.MethodPost, "/subscriptions/"+sub.String()+"/cancel", "", body)
}

// onChain is the subscription's account on chain.
func (s *solanaShop) onChain(t *testing.T, sub billing.SubscriptionID) (plan, account solanago.PublicKey) {
	t.Helper()
	var planPDA, subPDA string
	require.NoError(t, s.w.pool.QueryRow(t.Context(), s.w.q(`SELECT plan_pda, subscription_pda FROM billing.solana_subscriptions WHERE subscription_id = $1`), sub.UUID()).Scan(&planPDA, &subPDA))
	return solanago.MustPublicKeyFromBase58(planPDA), solanago.MustPublicKeyFromBase58(subPDA)
}

func (s *solanaShop) canceledOnChain(t *testing.T, sub billing.SubscriptionID) bool {
	t.Helper()
	_, account := s.onChain(t, sub)
	raw, ok := s.fake.Account(account)
	require.True(t, ok)
	decoded, err := subscriptions.DecodeSubscriptionAccount(raw)
	require.NoError(t, err)
	return decoded.Canceled()
}

func TestSolanaCancelEndsAtPeriodEndWithoutTheWallet(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b, sub := s.subscribed(t)
	walletTxs := s.fake.Transactions(b.wallet.PublicKey())

	status, body := b.cancel(sub, map[string]any{"reason": "too expensive"})
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Nil(t, body["next_action"], "no wallet step")
	require.Equal(t, []any{"canceled", true, true}, []any{body["status"], body["cancel_scheduled"], body["resumable"]}, "%v", body)
	require.Equal(t, walletTxs, s.fake.Transactions(b.wallet.PublicKey()), "the wallet signs nothing and pays no fee")
	require.False(t, s.canceledOnChain(t, sub), "nothing changes on chain")
	require.Equal(t, sub.String(), b.notificationData("subscription_canceled")["subscription_id"])

	w.advance(monthHours*time.Hour - time.Hour)
	require.True(t, b.entitled(s.key), "access runs to the paid period end")
	w.advance(2 * time.Hour)
	s.crankPass(t)
	require.Equal(t, uint64(solanaPlanAmount), s.fake.Balance(s.treasury, s.mint), "the crank pulls nothing")
	require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(b.wallet.PublicKey(), s.mint))
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, sub).Status)
	require.False(t, w.subscription(embedded, sub).Resumable)
	require.False(t, b.entitled(s.key), "access ends with the paid period")
}

func TestSolanaResumedCancelPullsTheNextPeriod(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b, sub := s.subscribed(t)

	status, body := b.cancel(sub, map[string]any{"reason": "maybe later"})
	require.Equal(t, http.StatusOK, status, "%v", body)
	w.advance(10 * 24 * time.Hour)
	status, body = b.call(http.MethodPost, "/subscriptions/"+sub.String()+"/resume", "", map[string]any{})
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, "active", body["status"])

	w.advance(monthHours*time.Hour - 10*24*time.Hour + time.Hour)
	s.crankPass(t)
	w.until(func() bool { return s.fake.Balance(s.treasury, s.mint) == 2*solanaPlanAmount }, "the resumed subscription's next period is pulled")
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, sub).Status)
}

// A cancel the subscriber makes in their own wallet, which OpenRails is not
// told of, is found at the period boundary: the pull is refused, the
// subscription ends, and nothing is dunned.
func TestSolanaWalletCancelEndsAtPeriodEnd(t *testing.T) {
	for name, cancel := range map[string]func(s *solanaShop, b *solanaBuyer, sub billing.SubscriptionID) solanago.Instruction{
		"cancel_subscription": func(s *solanaShop, b *solanaBuyer, sub billing.SubscriptionID) solanago.Instruction {
			plan, account := s.onChain(t, sub)
			events, _, err := subscriptions.DeriveEventAuthority()
			require.NoError(t, err)
			return subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{Subscriber: b.wallet.PublicKey(), PlanPDA: plan, SubscriptionPDA: account, EventAuthority: events})
		},
		"delegate revoked": func(s *solanaShop, b *solanaBuyer, _ billing.SubscriptionID) solanago.Instruction {
			ata, _, err := solanago.FindAssociatedTokenAddress(b.wallet.PublicKey(), s.mint)
			require.NoError(t, err)
			return token.NewRevokeInstruction(ata, b.wallet.PublicKey(), nil).Build()
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := openSolanaShop(t)
			w := s.w
			b, sub := s.subscribed(t)
			w.advance(5 * 24 * time.Hour)
			s.land(t, build(t, []solanago.Instruction{cancel(s, b, sub)}, b.wallet), w.clock.Now())

			require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, sub).Status)
			require.True(t, b.entitled(s.key), "the paid period stands")

			w.advance(monthHours*time.Hour - 5*24*time.Hour + time.Hour)
			s.crankPass(t)
			w.until(func() bool { return w.subscription(embedded, sub).Status == billing.SubscriptionCanceled }, "the refused pull ends the subscription")
			require.Equal(t, uint64(solanaPlanAmount), s.fake.Balance(s.treasury, s.mint), "nothing more is pulled")
			require.Nil(t, w.subscription(embedded, sub).Dunning, "never dunned")
			require.False(t, b.entitled(s.key))
		})
	}
}

// A customer who wants the on-chain guarantee also cancels in their wallet:
// the cancel is OpenRails' at once, and the wallet signs the on-chain one.
// A subscription canceled on chain cannot be resumed here.
func TestSolanaCancelCanAlsoRevokeOnChain(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b, sub := s.subscribed(t)

	status, body := b.cancel(sub, map[string]any{"reason": "for good", "revoke_on_chain": true})
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, "canceled", body["status"], "canceled before the wallet signs")
	next := body["next_action"].(map[string]any)
	require.Equal(t, "solana_sign_transactions", next["type"])
	transactions := next["transactions"].([]any)
	require.Len(t, transactions, 1)
	tx, err := solanago.TransactionFromBase64(transactions[0].(string))
	require.NoError(t, err)
	signature := s.land(t, signAs(t, tx, b.wallet), w.clock.Now())
	require.True(t, s.canceledOnChain(t, sub))

	status, body = b.cancel(sub, map[string]any{"reason": "for good", "signature": signature})
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, "canceled", body["status"])
	require.Nil(t, body["next_action"])

	status, body = b.call(http.MethodPost, "/subscriptions/"+sub.String()+"/resume", "", map[string]any{})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	require.Equal(t, billing.CodeInvalidParam, body["error"].(map[string]any)["code"])

	w.advance(monthHours*time.Hour + time.Hour)
	s.crankPass(t)
	require.Equal(t, uint64(solanaPlanAmount), s.fake.Balance(s.treasury, s.mint))
	require.False(t, b.entitled(s.key))
}

// Interleave the actual HTTP cancel and production signed pull around its
// submission fence. Both orders preserve one decision and paid access.
func TestSolanaCancelRacesRenewalSubmission(t *testing.T) {
	for _, point := range []failpoint.Point{failpoint.BeforeInline, failpoint.AfterFence} {
		t.Run(string(point), func(t *testing.T) {
			s := openSolanaShop(t)
			w := s.w
			b, sub := s.subscribed(t)
			reached, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			remove := failpoint.Set(func(ctx context.Context, site failpoint.Site) error {
				if site.Kind != "solana_pull" || site.Subscription != sub.UUID() || site.Point != point {
					return nil
				}
				first := false
				once.Do(func() { first = true; close(reached) })
				if first {
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			})
			defer remove()
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			w.advance(monthHours*time.Hour + time.Hour)
			job, err := w.jobs.Insert(t.Context(), solanaCrankPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
			require.NoError(t, err)
			select {
			case <-reached:
			case <-time.After(30 * time.Second):
				t.Fatal("pull did not reach its submission fence")
			}
			status, body := b.cancel(sub, map[string]any{"reason": "stop renewing"})
			if point == failpoint.BeforeInline {
				require.Equal(t, http.StatusOK, status, "%v", body)
			} else {
				require.Equal(t, http.StatusConflict, status, "%v", body)
				require.Equal(t, "payment_in_progress", body["error"].(map[string]any)["code"])
				require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, sub).Status)
			}
			unblock()
			w.waitJob(job.Job.ID)
			if point == failpoint.BeforeInline {
				require.Equal(t, uint64(solanaPlanAmount), s.fake.Balance(s.treasury, s.mint), "the winning cancel prevents the signed transaction from being sent")
				require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, sub).Status)
			} else {
				w.until(func() bool { return len(completed(w.payments(embedded, b.id))) == 2 }, "the winning renewal settles")
				require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(s.treasury, s.mint))
				status, body = b.cancel(sub, map[string]any{"reason": "stop renewing"})
				require.Equal(t, http.StatusOK, status, "%v", body)
				require.True(t, b.entitled(s.key), "the settled renewal bought the next paid period")
			}
		})
	}
}

func TestSolanaCancelMalformedSignatureIsRefused(t *testing.T) {
	s := openSolanaShop(t)
	b, sub := s.subscribed(t)
	status, body := b.cancel(sub, map[string]any{"reason": "stop renewing", "signature": "invalid-signature"})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	require.Equal(t, billing.SubscriptionActive, s.w.subscription(embedded, sub).Status)
}

func TestSolanaResumeRefusesRevokedTokenDelegate(t *testing.T) {
	s := openSolanaShop(t)
	b, sub := s.subscribed(t)
	ata, _, err := solanago.FindAssociatedTokenAddress(b.wallet.PublicKey(), s.mint)
	require.NoError(t, err)
	s.land(t, build(t, []solanago.Instruction{token.NewRevokeInstruction(ata, b.wallet.PublicKey(), nil).Build()}, b.wallet), s.w.clock.Now())
	status, body := b.cancel(sub, map[string]any{"reason": "stop renewing"})
	require.Equal(t, http.StatusOK, status, "%v", body)
	status, body = b.call(http.MethodPost, "/subscriptions/"+sub.String()+"/resume", "", map[string]any{})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	require.Equal(t, billing.CodeInvalidParam, body["error"].(map[string]any)["code"])
	require.Equal(t, billing.SubscriptionCanceled, s.w.subscription(embedded, sub).Status)
}

func TestSolanaCanceledPullDoesNotBlockOtherSubscribers(t *testing.T) {
	s := openSolanaShop(t)
	b, canceled := s.subscribed(t)
	_, active := s.subscribed(t)
	status, body := b.cancel(canceled, map[string]any{"reason": "stop renewing"})
	require.Equal(t, http.StatusOK, status, "%v", body)
	s.w.advance(monthHours*time.Hour + time.Hour)
	s.crankPass(t)
	s.w.until(func() bool { return s.fake.Balance(s.treasury, s.mint) == 3*solanaPlanAmount }, "the active peer renews while the canceled peer is skipped")
	require.Equal(t, billing.SubscriptionCanceled, s.w.subscription(embedded, canceled).Status)
	require.Equal(t, billing.SubscriptionActive, s.w.subscription(embedded, active).Status)
	require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(b.wallet.PublicKey(), s.mint))
	status, body = b.call(http.MethodPost, "/subscriptions/"+canceled.String()+"/resume", "", map[string]any{})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	require.Equal(t, billing.CodeInvalidParam, body["error"].(map[string]any)["code"])
}

func TestSolanaResumeRefusesExpiredRenewalMirror(t *testing.T) {
	s := openSolanaShop(t)
	b, sub := s.subscribed(t)
	status, body := b.cancel(sub, map[string]any{"reason": "stop renewing"})
	require.Equal(t, http.StatusOK, status, "%v", body)
	_, err := s.w.pool.Exec(t.Context(), s.w.q(`UPDATE billing.solana_subscriptions SET status = 'expired' WHERE subscription_id = $1`), sub.UUID())
	require.NoError(t, err)
	status, body = b.call(http.MethodPost, "/subscriptions/"+sub.String()+"/resume", "", map[string]any{})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	require.Equal(t, billing.CodeInvalidParam, body["error"].(map[string]any)["code"])
	require.Equal(t, billing.SubscriptionCanceled, s.w.subscription(embedded, sub).Status)
	require.True(t, b.entitled(s.key), "a refused undo preserves the paid access")
}

// Only positive nonexecution of the recorded signature releases the cancel
// fence. Missing history or proof about another transaction keeps it held.
func TestSolanaCancelReleasedOnlyByExactRevertedSignature(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b, sub := s.subscribed(t)
	ata, _, err := solanago.FindAssociatedTokenAddress(b.wallet.PublicKey(), s.mint)
	require.NoError(t, err)
	receiver, _, err := solanago.FindAssociatedTokenAddress(s.treasury, s.mint)
	require.NoError(t, err)
	// A real signed, landed transfer reverts for insufficient funds; the RPC
	// node returns its execution metadata, not a manufactured nonexecution flag.
	sig := s.land(t, build(t, []solanago.Instruction{token.NewTransferInstruction(100*solanaPlanAmount, ata, receiver, b.wallet.PublicKey(), nil).Build()}, b.wallet), w.clock.Now())
	var mid, psp, rowID uuid.UUID
	var pda string
	var next time.Time
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT merchant_id, psp_id FROM billing.subscriptions WHERE id=$1`), sub.UUID()).Scan(&mid, &psp))
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id, subscription_pda, next_pull_at FROM billing.solana_subscriptions WHERE subscription_id=$1`), sub.UUID()).Scan(&rowID, &pda, &next))
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	// Enqueuing an intent queues its job in the same transaction.
	d.SetRiverJobInserter(w.jobs)
	require.NoError(t, d.RunInMerchantScope(t.Context(), billing.MerchantID(mid), "verify reverted pull", func(ctx context.Context) error {
		store := intents.NewStore(d)
		id := sub.UUID()
		in, err := store.Enqueue(ctx, intents.EnqueueParams{MerchantID: mid, PspID: psp, Provider: "solana", IntentType: riverjobs.TypeSolanaPull,
			SubscriptionID: &id, IdempotencyKey: "reverted-pull-" + uuid.NewString(), NextAttemptAt: w.clock.Now().Add(24 * time.Hour),
			Payload: riverjobs.SolanaPullPayload{RowID: rowID, SubscriptionPDA: pda, NextPullAt: next}, Origin: intents.OriginSystem})
		if err != nil {
			return err
		}
		if err := store.RecordProgress(ctx, in.ID, map[string]any{"transaction_id": sig, "reverted_transaction_id": solanago.Signature{1}.String()}); err != nil {
			return err
		}
		in.ResultEvidence = []byte(`{"transaction_id":"` + sig + `"}`)
		status, body := b.cancel(sub, map[string]any{"reason": "stop renewing"})
		require.Equal(t, http.StatusConflict, status, "%v", body)
		h := &riverjobs.SolanaPullIntentHandler{Store: store, Chain: solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{PublicURL: s.fake.URL(), Network: "devnet"})}
		// The node lags until told otherwise: background chain readers may read it too.
		s.fake.Forget(sig, 1_000_000)
		require.Equal(t, intents.OutcomeAmbiguous, h.Verify(ctx, in).Class)
		status, body = b.cancel(sub, map[string]any{"reason": "stop renewing"})
		require.Equal(t, http.StatusConflict, status, "%v", body)
		s.fake.Forget(sig, 0)
		require.Equal(t, intents.OutcomeRetryable, h.Verify(ctx, in).Class)
		status, body = b.cancel(sub, map[string]any{"reason": "stop renewing"})
		require.Equal(t, http.StatusOK, status, "%v", body)
		require.True(t, b.entitled(s.key), "paid access survives the cancellation")
		require.Equal(t, uint64(solanaPlanAmount), s.fake.Balance(s.treasury, s.mint), "the reverted transaction moved no money")
		return nil
	}))
}
