//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/solana/settlement"
	"github.com/open-rails/openrails/internal/solanafake"
)

// A Solana subscription activates only from its first payment: the bundle its
// checkout prepared and the merchant co-signed, landed on-chain, signed by the
// checkout's wallet, pulling the full first period into the merchant's account
// within the checkout's validity. Any other signature is refused and grants
// nothing: no subscription, no payment, no entitlement.

const solanaPlanAmount = 23_000_000

type solanaShop struct {
	w        *world
	fake     *solanafake.Node
	merchant solanago.PrivateKey
	mint     solanago.PublicKey
	option   billing.CheckoutOption
	price    string
	key      string
}

func openSolanaShop(t *testing.T) *solanaShop {
	t.Helper()
	w := prepareWorld(t, 12)
	fake, merchant := withSolana(t, w)
	w.start()
	plan, err := fake.Plan(merchant.PublicKey(), 4242, solanafake.DevnetDUSDMint, solanaPlanAmount, monthHours)
	require.NoError(t, err)
	key, err := w.applyCatalog(fmt.Sprintf(`  "{key}-monthly":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: 720
    access_duration_hours: 720
    psps: [nmi, solana]
    psp_links:
      solana:
        plan_pda: %s
        plan_id: "4242"
`, plan))
	require.NoError(t, err)
	mint := solanago.MustPublicKeyFromBase58(solanafake.DevnetDUSDMint)
	fake.Fund(merchant.PublicKey(), mint, 0)
	return &solanaShop{w: w, fake: fake, merchant: merchant, mint: mint, option: w.options(billing.GetCheckoutConfigParams{ProductKey: key, PriceKey: key + "-monthly"})["solana"], price: priceID(t, w, key, key+"-monthly"), key: key}
}

type solanaBuyer struct {
	*customer
	wallet solanago.PrivateKey
}

// buyer is a customer with a funded wallet; a returning one already holds a
// subscription authority for the mint.
func (s *solanaShop) buyer(t *testing.T, returning bool) *solanaBuyer {
	t.Helper()
	b := &solanaBuyer{customer: s.w.newCustomer(), wallet: solanago.NewWallet().PrivateKey}
	s.fake.Fund(b.wallet.PublicKey(), s.mint, 3*solanaPlanAmount)
	if returning {
		require.NoError(t, s.fake.Authority(b.wallet.PublicKey(), s.mint, 42))
	}
	return b
}

type solanaCheckout struct {
	id        billing.CheckoutAttemptID
	reference solanago.PublicKey
	expiresAt time.Time
	// bundle is the prepared first payment: co-signed by the merchant, awaiting
	// the wallet's signature.
	bundle *solanago.Transaction
}

// checkout opens a wallet-connected subscribe checkout for b naming wallet.
func (s *solanaShop) checkout(t *testing.T, b *solanaBuyer, wallet solanago.PublicKey) *solanaCheckout {
	t.Helper()
	session, err := s.w.client[embedded].CreateCheckoutAttempt(t.Context(), billing.CreateCheckoutAttemptParams{
		Customer: billing.CheckoutCustomerIdentity{ID: cid(b.id)}, PriceID: pid(s.price), IdempotencyKey: "sol-" + uuid.NewString(),
		PaymentOptions: billing.CheckoutPaymentOptions{PSP: s.option.PSP, TokenSymbol: "DUSD", Wallet: wallet.String()},
		SuccessURL:     "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
	})
	require.NoError(t, err)
	got := s.w.attempt(session.ID)
	require.Equal(t, "requires_action", got["status"], "%v", got)
	next := got["next_action"].(map[string]any)
	require.Equal(t, "solana_sign_transactions", next["type"])
	txs := next["transactions"].([]any)
	require.Len(t, txs, 1)
	bundle, err := solanago.TransactionFromBase64(txs[0].(string))
	require.NoError(t, err)
	expires, err := time.Parse(time.RFC3339, got["expires_at"].(string))
	require.NoError(t, err)
	var reference string
	require.NoError(t, s.w.pool.QueryRow(t.Context(), s.w.q(`SELECT reference FROM billing.checkout_attempts WHERE id = $1`), session.ID.UUID()).Scan(&reference))
	return &solanaCheckout{
		id:        session.ID,
		reference: solanago.MustPublicKeyFromBase58(reference),
		expiresAt: expires,
		bundle:    bundle,
	}
}

// confirm is the merchant relaying the signature the buyer's wallet sent.
func (b *solanaBuyer) confirm(c *solanaCheckout, signature string) (int, map[string]any) {
	return b.w.confirmAttempt(c.id, signature)
}

// requireNothingGranted: no subscription, payment or entitlement, and the
// checkout still awaits its payment.
func (s *solanaShop) requireNothingGranted(t *testing.T, b *solanaBuyer, c *solanaCheckout) {
	t.Helper()
	subs, err := s.w.client[embedded].ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: b.customerID()})
	require.NoError(t, err)
	require.Empty(t, subs.Items)
	require.Empty(t, s.w.payments(embedded, b.id))
	require.False(t, b.entitled(s.key))
	require.Equal(t, "requires_action", s.w.attempt(c.id)["status"])
}

func (s *solanaShop) land(t *testing.T, tx *solanago.Transaction, at time.Time) string {
	t.Helper()
	sig, err := s.fake.Land(tx, at)
	require.NoError(t, err)
	return sig.String()
}

// signAs completes key's signature slot of a partially signed transaction.
func signAs(t *testing.T, tx *solanago.Transaction, key solanago.PrivateKey) *solanago.Transaction {
	t.Helper()
	msg, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	sig, err := key.Sign(msg)
	require.NoError(t, err)
	for i, k := range tx.Message.AccountKeys[:tx.Message.Header.NumRequiredSignatures] {
		if k.Equals(key.PublicKey()) {
			tx.Signatures[i] = sig
			return tx
		}
	}
	t.Fatalf("%s does not sign this transaction", key.PublicKey())
	return nil
}

// instructions decompiles a transaction's instructions for signers to resend.
func instructions(t *testing.T, tx *solanago.Transaction, signers ...solanago.PublicKey) []solanago.Instruction {
	t.Helper()
	var out []solanago.Instruction
	for _, inst := range tx.Message.Instructions {
		program, err := tx.ResolveProgramIDIndex(inst.ProgramIDIndex)
		require.NoError(t, err)
		resolved, err := inst.ResolveInstructionAccounts(&tx.Message)
		require.NoError(t, err)
		metas := make(solanago.AccountMetaSlice, len(resolved))
		for i, m := range resolved {
			signs := false
			for _, k := range signers {
				signs = signs || k.Equals(m.PublicKey)
			}
			metas[i] = solanago.NewAccountMeta(m.PublicKey, m.IsWritable, signs)
		}
		out = append(out, solanago.NewInstruction(program, metas, inst.Data))
	}
	return out
}

func isPull(ix solanago.Instruction) bool {
	data, _ := ix.Data()
	kind, _ := subscriptions.ParseInstructionKind(data)
	return ix.ProgramID().Equals(subscriptions.ProgramID) && kind == subscriptions.KindTransferSubscription
}

// build signs a transaction of ixs with every signer, the first paying fees.
func build(t *testing.T, ixs []solanago.Instruction, signers ...solanago.PrivateKey) *solanago.Transaction {
	t.Helper()
	tx, err := solanago.NewTransaction(ixs, solanago.Hash{1}, solanago.TransactionPayer(signers[0].PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(key solanago.PublicKey) *solanago.PrivateKey {
		for i := range signers {
			if signers[i].PublicKey().Equals(key) {
				return &signers[i]
			}
		}
		return nil
	})
	require.NoError(t, err)
	return tx
}

// withPull is the prepared bundle with its pull replaced, co-signed as the
// merchant would have to: the server itself never builds such a pull.
func (s *solanaShop) withPull(t *testing.T, c *solanaCheckout, b *solanaBuyer, amount uint64, receiver solanago.PublicKey) *solanago.Transaction {
	t.Helper()
	var ixs []solanago.Instruction
	for _, ix := range instructions(t, c.bundle, b.wallet.PublicKey(), s.merchant.PublicKey()) {
		if !isPull(ix) {
			ixs = append(ixs, ix)
			continue
		}
		accounts := ix.Accounts()
		ixs = append(ixs, subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
			SubscriptionPDA: accounts[0].PublicKey, PlanPDA: accounts[1].PublicKey, SubscriptionAuthority: accounts[2].PublicKey,
			DelegatorATA: accounts[3].PublicKey, ReceiverATA: receiver, Caller: s.merchant.PublicKey(), Mint: s.mint,
			TokenProgram: solanago.TokenProgramID, EventAuthority: accounts[8].PublicKey, Amount: amount, Delegator: b.wallet.PublicKey(),
		}))
	}
	return build(t, ixs, b.wallet, s.merchant)
}

func TestSolanaSubscriptionActivatesOnlyOnItsFirstPayment(t *testing.T) {
	s := openSolanaShop(t)
	merchantATA, _, err := solanago.FindAssociatedTokenAddress(s.merchant.PublicKey(), s.mint)
	require.NoError(t, err)

	var paid struct {
		buyer   *solanaBuyer
		session billing.CheckoutAttemptID
		sig     string
	}
	t.Run("the prepared first payment activates", func(t *testing.T) {
		b := s.buyer(t, false) // first-time: the bundle initializes the authority too
		c := s.checkout(t, b, b.wallet.PublicKey())
		sig := s.land(t, signAs(t, c.bundle, b.wallet), s.w.clock.Now())
		status, out := b.confirm(c, sig)
		require.Equal(t, http.StatusOK, status, "%v", out)
		done := unwrap(out)
		require.Equal(t, "succeeded", done["status"], "%v", done)
		require.NotEmpty(t, done["subscription_id"])
		require.Equal(t, uint64(solanaPlanAmount), s.fake.Balance(s.merchant.PublicKey(), s.mint))

		s.w.advance(time.Minute)
		require.True(t, b.entitled(s.key))
		payments := completed(s.w.payments(embedded, b.id))
		require.Len(t, payments, 1)
		require.Equal(t, sig, payments[0].TransactionID)

		status, out = b.confirm(c, sig)
		require.Equal(t, http.StatusOK, status, "a repeated confirm is idempotent: %v", out)
		require.Len(t, completed(s.w.payments(embedded, b.id)), 1)
		paid.buyer, paid.session, paid.sig = b, c.id, sig
	})

	t.Run("subscribe without the first pull is refused", func(t *testing.T) {
		b := s.buyer(t, true)
		c := s.checkout(t, b, b.wallet.PublicKey())
		var ixs []solanago.Instruction
		for _, ix := range instructions(t, c.bundle, b.wallet.PublicKey()) {
			if !isPull(ix) {
				ixs = append(ixs, ix)
			}
		}
		sig := s.land(t, build(t, ixs, b.wallet), s.w.clock.Now())
		status, out := b.confirm(c, sig)
		require.Equal(t, http.StatusBadRequest, status, "%v", out)
		s.requireNothingGranted(t, b, c)

		// Paying the amount by a plain transfer is not the merchant's pull either.
		from, _, _ := solanago.FindAssociatedTokenAddress(b.wallet.PublicKey(), s.mint)
		ixs = append(ixs, token.NewTransferInstruction(solanaPlanAmount, from, merchantATA, b.wallet.PublicKey(), nil).Build())
		sig = s.land(t, build(t, ixs, b.wallet), s.w.clock.Now())
		status, out = b.confirm(c, sig)
		require.Equal(t, http.StatusBadRequest, status, "%v", out)
		s.requireNothingGranted(t, b, c)
	})

	t.Run("a victim's wallet named without its signature is refused", func(t *testing.T) {
		// The audit's proof of concept: the victim subscribed on-chain by itself
		// (nothing pulled, nothing recorded); the attacker names that wallet and
		// confirms with a made-up signature.
		victim := s.buyer(t, true)
		plan, bump, _ := subscriptions.DerivePlanPDA(s.merchant.PublicKey(), 4242)
		sub, _, _ := subscriptions.DeriveSubscriptionPDA(plan, victim.wallet.PublicKey())
		authority, _, _ := subscriptions.DeriveSubscriptionAuthority(victim.wallet.PublicKey(), s.mint)
		events, _, _ := subscriptions.DeriveEventAuthority()
		victimSubscribe := s.land(t, build(t, []solanago.Instruction{subscriptions.BuildSubscribe(subscriptions.SubscribeParams{
			Subscriber: victim.wallet.PublicKey(), Merchant: s.merchant.PublicKey(), PlanPDA: plan, SubscriptionPDA: sub,
			SubscriptionAuthorityPDA: authority, EventAuthority: events, PlanID: 4242, PlanBump: bump, ExpectedMint: s.mint,
			ExpectedAmount: solanaPlanAmount, ExpectedPeriodHours: monthHours, ExpectedCreatedAt: 1_700_000_000, ExpectedSubscriptionAuthInitID: 42,
		})}, victim.wallet), s.w.clock.Now())

		attacker := s.buyer(t, true)
		c := s.checkout(t, attacker, victim.wallet.PublicKey())
		_, err := s.fake.Land(c.bundle, s.w.clock.Now())
		require.Error(t, err, "the prepared payment cannot land without the victim's signature")
		for _, sig := range []string{"not-a-real-signature", victimSubscribe} {
			status, out := attacker.confirm(c, sig)
			require.Equal(t, http.StatusBadRequest, status, "%s: %v", sig, out)
		}
		s.requireNothingGranted(t, attacker, c)
		require.Equal(t, uint64(3*solanaPlanAmount), s.fake.Balance(victim.wallet.PublicKey(), s.mint))
	})

	t.Run("a replayed signature is refused", func(t *testing.T) {
		// Another checkout for the same wallet and plan: only the landed
		// payment's checkout binding tells them apart.
		require.NotEmpty(t, paid.sig, "needs the activated payment")
		b := s.buyer(t, true)
		c := s.checkout(t, b, paid.buyer.wallet.PublicKey())
		status, out := b.confirm(c, paid.sig)
		require.Equal(t, http.StatusBadRequest, status, "%v", out)
		s.requireNothingGranted(t, b, c)
		require.Len(t, completed(s.w.payments(embedded, paid.buyer.id)), 1)
	})

	t.Run("a pull of the wrong amount or to the wrong recipient is refused", func(t *testing.T) {
		b := s.buyer(t, true)
		c := s.checkout(t, b, b.wallet.PublicKey())
		elsewhere := s.fake.Fund(solanago.NewWallet().PublicKey(), s.mint, 0)
		for _, tx := range []*solanago.Transaction{
			s.withPull(t, c, b, solanaPlanAmount-1, merchantATA),
			s.withPull(t, c, b, solanaPlanAmount, elsewhere),
		} {
			sig := s.land(t, tx, s.w.clock.Now())
			status, out := b.confirm(c, sig)
			require.Equal(t, http.StatusBadRequest, status, "%v", out)
		}
		s.requireNothingGranted(t, b, c)
	})

	t.Run("a payment landing after the quote expired is refused", func(t *testing.T) {
		b := s.buyer(t, true)
		c := s.checkout(t, b, b.wallet.PublicKey())
		sig := s.land(t, signAs(t, c.bundle, b.wallet), c.expiresAt.Add(solanaint.LateSettlementWindow+time.Minute))
		status, out := b.confirm(c, sig)
		require.Equal(t, http.StatusGone, status, "%v", out)
		s.requireNothingGranted(t, b, c)
	})

	t.Run("a landed transfer settles one checkout", func(t *testing.T) {
		// Two PSPs sharing a payout wallet could otherwise both accept one
		// transfer that carries both checkouts' references: the claim is keyed by
		// the signature alone, across every merchant and PSP.
		require.NotEmpty(t, paid.sig, "needs the activated payment")
		other := s.buyer(t, true)
		c := s.checkout(t, other, other.wallet.PublicKey())
		d, err := db.NewWithPGXPool(s.w.pool, s.w.schema)
		require.NoError(t, err)
		first, second := paid.session, c.id
		require.NoError(t, d.RunInMerchantScope(t.Context(), s.w.client[embedded].MerchantID(), "solana claim", func(ctx context.Context) error {
			require.ErrorIs(t, settlement.ClaimCheckout(ctx, d, second.UUID(), paid.sig), settlement.ErrClaimed)
			require.NoError(t, settlement.ClaimCheckout(ctx, d, first.UUID(), paid.sig), "the same claim repeats")
			tag := system.NewTransferInstruction(0, other.wallet.PublicKey(), other.wallet.PublicKey()).Build()
			another := s.land(t, build(t, []solanago.Instruction{tag}, other.wallet), s.w.clock.Now())
			require.ErrorIs(t, settlement.ClaimCheckout(ctx, d, first.UUID(), another), settlement.ErrClaimed, "a settled checkout takes no second transfer")
			return nil
		}))
		var def string
		require.NoError(t, s.w.pool.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes WHERE schemaname = $1 AND indexname = 'checkout_attempts_transaction_id_key'`, s.w.schema).Scan(&def))
		require.Contains(t, def, "(transaction_id)")
		s.requireNothingGranted(t, other, c)
	})
}
