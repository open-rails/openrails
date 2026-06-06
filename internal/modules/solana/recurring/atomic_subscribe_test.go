package recurring

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

// subSecrets serves a fixed merchant private key to the keypair signer.
type subSecrets struct{ key string }

func (m subSecrets) GetSecret(context.Context, tenant.ID, string) (string, error) {
	return m.key, nil
}

// subFakeRPC implements prepareRPC: a present SubscriptionAuthority (initId at the
// proven offset), a zero blockhash, and a configurable USDC ATA balance.
type subFakeRPC struct {
	initID  int64
	balance uint64
	balErr  error
}

func (f subFakeRPC) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	data := make([]byte, subscriptionAuthorityInitIDOffset+8)
	binary.LittleEndian.PutUint64(data[subscriptionAuthorityInitIDOffset:], uint64(f.initID))
	return data, nil
}
func (subFakeRPC) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash{}, nil
}
func (f subFakeRPC) GetTokenBalanceForMint(context.Context, solanago.PublicKey, solanago.PublicKey) (uint64, error) {
	return f.balance, f.balErr
}

func newSubscribeSvc(t *testing.T, rpc prepareRPC) (*PrepareSubscribeService, solanago.PublicKey) {
	t.Helper()
	merchant, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := solana.NewKeypairSigner(subSecrets{key: merchant.String()}, 0)
	// The Submitter must resolve the SAME merchant address the signer co-signs with.
	submitter := NewSignerSubmitter(signer, nil)
	return NewPrepareSubscribeService(submitter, signer, rpc, "devnet", testSolanaTokens()), merchant.PublicKey()
}

func newSubscribeInput(t *testing.T) PrepareSubscribeInput {
	t.Helper()
	return PrepareSubscribeInput{
		TenantID:         tenant.DefaultID,
		SubscriberWallet: randKeyStr(t),
		PlanID:           7,
		MintSymbol:       "USDC",
		AmountBaseUnits:  10_000_000,
		PeriodHours:      720,
		PlanCreatedAt:    1_700_000_000,
	}
}

// The subscribe step yields a 2-instruction co-signed bundle (subscribe+transfer)
// with two required signers, exactly one (the cranker) pre-signed.
func TestPrepareSubscribe_AtomicCosignedBundle(t *testing.T) {
	svc, _ := newSubscribeSvc(t, subFakeRPC{initID: 42, balance: 50_000_000})
	res, err := svc.Prepare(context.Background(), newSubscribeInput(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.Step != "subscribe" {
		t.Fatalf("Step = %q, want subscribe", res.Step)
	}
	if !res.AuthorityExists {
		t.Fatalf("AuthorityExists must be true (returning subscriber)")
	}
	if len(res.Transactions) != 1 {
		t.Fatalf("want a single tx, got %d", len(res.Transactions))
	}
	tx := decodeTx(t, res.Transactions[0])
	if len(tx.Message.Instructions) != 2 {
		t.Fatalf("subscribe bundle should have 2 instructions (subscribe+transfer), got %d", len(tx.Message.Instructions))
	}
	if int(tx.Message.Header.NumRequiredSignatures) != 2 {
		t.Fatalf("want 2 required signers, got %d", tx.Message.Header.NumRequiredSignatures)
	}
	signed := 0
	for _, s := range tx.Signatures {
		if !s.IsZero() {
			signed++
		}
	}
	if signed != 1 {
		t.Errorf("exactly one slot (the cranker) should be pre-signed, got %d", signed)
	}
}

func TestPrepareSubscribe_InitTransactionHasUnsignedSignatureSlots(t *testing.T) {
	orig := authorityReadBackoff
	authorityReadBackoff = 0
	defer func() { authorityReadBackoff = orig }()

	svc, _ := newSubscribeSvc(t, subFakeRPCAbsent{balance: 50_000_000})
	res, err := svc.Prepare(context.Background(), newSubscribeInput(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.Step != "init" {
		t.Fatalf("Step = %q, want init", res.Step)
	}
	tx := decodeTx(t, res.Transactions[0])
	assertUnsignedSignatureSlots(t, tx)
}

// Pre-flight: balance below the first-period amount returns the typed
// insufficient-USDC error (and never builds the tx).
func TestPrepareSubscribe_PreflightInsufficient(t *testing.T) {
	svc, _ := newSubscribeSvc(t, subFakeRPC{initID: 42, balance: 1_000_000}) // < 10_000_000
	_, err := svc.Prepare(context.Background(), newSubscribeInput(t))
	if err == nil {
		t.Fatal("expected an insufficient-USDC error")
	}
	if !errors.Is(err, ErrInsufficientUSDC) {
		t.Fatalf("error must wrap ErrInsufficientUSDC, got %v", err)
	}
	var ie *InsufficientUSDCError
	if !errors.As(err, &ie) {
		t.Fatalf("error must be an *InsufficientUSDCError, got %T", err)
	}
	if ie.HaveBaseUnits != 1_000_000 || ie.NeedBaseUnits != 10_000_000 {
		t.Fatalf("have/need = %d/%d, want 1000000/10000000", ie.HaveBaseUnits, ie.NeedBaseUnits)
	}
}

// Pre-flight: balance >= amount proceeds (builds the co-signed bundle).
func TestPrepareSubscribe_PreflightSufficientProceeds(t *testing.T) {
	svc, _ := newSubscribeSvc(t, subFakeRPC{initID: 42, balance: 10_000_000}) // == amount
	res, err := svc.Prepare(context.Background(), newSubscribeInput(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(res.Transactions) != 1 {
		t.Fatalf("want a single co-signed tx, got %d", len(res.Transactions))
	}
}

// Pre-flight: a transient balance-read failure must NOT hard-fail the flow — the
// atomic tx is the real guarantee, so Prepare proceeds.
func TestPrepareSubscribe_PreflightReadFailureProceeds(t *testing.T) {
	svc, _ := newSubscribeSvc(t, subFakeRPC{initID: 42, balErr: errors.New("rpc blip")})
	res, err := svc.Prepare(context.Background(), newSubscribeInput(t))
	if err != nil {
		t.Fatalf("read failure must not hard-fail; got %v", err)
	}
	if len(res.Transactions) != 1 {
		t.Fatalf("want a single co-signed tx, got %d", len(res.Transactions))
	}
}

// --- ConfirmEnrollment: no separate crank ---

type fakeLifecycle struct {
	called int
	params *submod.CreateMembershipParams
	out    *models.Subscription
}

func (f *fakeLifecycle) CreateMembership(_ context.Context, p *submod.CreateMembershipParams) (*models.Subscription, error) {
	f.called++
	f.params = p
	return f.out, nil
}

type fakeRepo struct {
	called int
	row    *models.SolanaSubscription
}

func (f *fakeRepo) Upsert(_ context.Context, s *models.SolanaSubscription) error {
	f.called++
	f.row = s
	return nil
}

// fakeBalanceChecker reports a present subscription PDA account (proving the
// atomic subscribe bundle landed). bal>0 => account exists (non-empty data).
type fakeBalanceChecker struct{ bal uint64 }

func (f fakeBalanceChecker) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	if f.bal == 0 {
		return nil, nil
	}
	return []byte{1}, nil
}

func TestConfirmEnrollment_CreatesMembershipWithoutCrank(t *testing.T) {
	subID := uuid.New()
	lc := &fakeLifecycle{out: &models.Subscription{ID: subID}}
	repo := &fakeRepo{}
	merchant, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := solana.NewKeypairSigner(subSecrets{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, nil)

	// Note: NewEnrollService no longer takes a *CrankService — the first pull is
	// inside the atomic subscribe tx. The PDA is funded, so confirm just creates
	// the membership.
	svc := NewEnrollService(lc, repo, fakeBalanceChecker{bal: 2_039_280}, submitter, "devnet", testSolanaTokens())

	sub, err := svc.ConfirmEnrollment(context.Background(), EnrollInput{
		TenantID:         tenant.DefaultID,
		UserID:           "user-1",
		PriceID:          uuid.New(),
		SubscriberWallet: randKeyStr(t),
		PlanID:           7,
		MintSymbol:       "USDC",
		AmountBaseUnits:  10_000_000,
		PeriodHours:      720,
		PlanCreatedAt:    1_700_000_000,
		FiatAmount:       999,
		Currency:         "USD",
		Signature:        "atomic-sig-123",
	})
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if sub == nil || sub.ID != subID {
		t.Fatalf("expected the created membership back")
	}
	if lc.called != 1 {
		t.Fatalf("CreateMembership called %d times, want 1", lc.called)
	}
	if repo.called != 1 {
		t.Fatalf("repo.Upsert called %d times, want 1", repo.called)
	}
	// The atomic-tx signature is recorded on both the membership + the row.
	if lc.params.TransactionID != "atomic-sig-123" {
		t.Errorf("membership TransactionID = %q, want atomic-sig-123", lc.params.TransactionID)
	}
	if repo.row.LastSignature == nil || *repo.row.LastSignature != "atomic-sig-123" {
		t.Errorf("row LastSignature not set to the atomic signature")
	}
	if repo.row.Status != models.SolanaSubscriptionActive {
		t.Errorf("row Status = %v, want active", repo.row.Status)
	}
}

// When no signature is supplied, confirm falls back to the subscription PDA as a
// stable identifier (still no crank).
func TestConfirmEnrollment_FallsBackToPDAIdentifier(t *testing.T) {
	lc := &fakeLifecycle{out: &models.Subscription{ID: uuid.New()}}
	repo := &fakeRepo{}
	merchant, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := solana.NewKeypairSigner(subSecrets{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, nil)
	svc := NewEnrollService(lc, repo, fakeBalanceChecker{bal: 1}, submitter, "devnet", testSolanaTokens())

	if _, err := svc.ConfirmEnrollment(context.Background(), EnrollInput{
		TenantID:         tenant.DefaultID,
		UserID:           "user-2",
		PriceID:          uuid.New(),
		SubscriberWallet: randKeyStr(t),
		PlanID:           7,
		MintSymbol:       "USDC",
		AmountBaseUnits:  10_000_000,
		PeriodHours:      720,
		Currency:         "USD",
	}); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if lc.params.TransactionID == "" {
		t.Fatal("expected a non-empty fallback TransactionID (the subscription PDA)")
	}
}
