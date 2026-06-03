package recurring

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/programs/token"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// fakeSolanaSubReader stubs the on-chain subscription row lookup.
type fakeSolanaSubReader struct {
	row *models.SolanaSubscription
	err error
}

func (f fakeSolanaSubReader) GetBySubscriptionID(context.Context, uuid.UUID) (*models.SolanaSubscription, error) {
	return f.row, f.err
}

// fakeCancelRPC stubs the recent-blockhash read.
type fakeCancelRPC struct{}

func (fakeCancelRPC) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash{}, nil
}

func newCancelRow(t *testing.T) *models.SolanaSubscription {
	t.Helper()
	return &models.SolanaSubscription{
		SubscriberWallet: newMerchant(t).String(),
		PlanPDA:          newMerchant(t).String(),
		SubscriptionPDA:  newMerchant(t).String(),
		AuthorityPDA:     newMerchant(t).String(),
		MerchantAddress:  newMerchant(t).String(),
		Mint:             newMerchant(t).String(),
	}
}

func TestPrepareCancel_BuildsUnsignedTx(t *testing.T) {
	row := newCancelRow(t)
	svc := NewPrepareCancelService(fakeSolanaSubReader{row: row}, fakeCancelRPC{})

	res, err := svc.Prepare(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.SubscriptionPDA != row.SubscriptionPDA {
		t.Errorf("SubscriptionPDA = %q, want %q", res.SubscriptionPDA, row.SubscriptionPDA)
	}
	raw, err := base64.StdEncoding.DecodeString(res.Transaction)
	if err != nil {
		t.Fatalf("transaction is not valid base64: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("transaction is empty")
	}

	// The deserialized tx must carry the subscriber as fee payer + signer and a
	// single SPL token Revoke instruction targeting the subscriber's ATA — the
	// trustless billing stop (#266). cancel_subscription is NOT what stops pulls.
	tx, err := solanago.TransactionFromBytes(raw)
	if err != nil {
		t.Fatalf("unmarshal tx: %v", err)
	}
	if got := tx.Message.AccountKeys[0].String(); got != row.SubscriberWallet {
		t.Errorf("fee payer = %q, want subscriber %q", got, row.SubscriberWallet)
	}
	if len(tx.Message.Instructions) != 1 {
		t.Fatalf("want 1 instruction, got %d", len(tx.Message.Instructions))
	}

	// The lone instruction must be an SPL token Revoke on the SPL token program.
	subscriber := solanago.MustPublicKeyFromBase58(row.SubscriberWallet)
	mint := solanago.MustPublicKeyFromBase58(row.Mint)
	wantATA, _, err := subscriptions.DeriveATA(subscriber, mint, solanago.TokenProgramID)
	if err != nil {
		t.Fatalf("derive ata: %v", err)
	}

	cix := tx.Message.Instructions[0]
	progKey, err := tx.Message.Program(cix.ProgramIDIndex)
	if err != nil {
		t.Fatalf("resolve program id: %v", err)
	}
	if !progKey.Equals(solanago.TokenProgramID) {
		t.Errorf("instruction program = %q, want SPL token program %q", progKey, solanago.TokenProgramID)
	}

	// Resolve the instruction's account metas and decode it as a token Revoke.
	accs, err := cix.ResolveInstructionAccounts(&tx.Message)
	if err != nil {
		t.Fatalf("resolve instruction accounts: %v", err)
	}
	decoded, err := token.DecodeInstruction(accs, cix.Data)
	if err != nil {
		t.Fatalf("decode token instruction: %v", err)
	}
	revoke, ok := decoded.Impl.(*token.Revoke)
	if !ok {
		t.Fatalf("instruction is %T, want *token.Revoke", decoded.Impl)
	}
	if got := revoke.GetSourceAccount().PublicKey; !got.Equals(wantATA) {
		t.Errorf("revoke source = %q, want subscriber ATA %q", got, wantATA)
	}
	if got := revoke.GetOwnerAccount().PublicKey; !got.Equals(subscriber) {
		t.Errorf("revoke owner = %q, want subscriber %q", got, subscriber)
	}
}

func TestPrepareCancel_RejectsNilID(t *testing.T) {
	svc := NewPrepareCancelService(fakeSolanaSubReader{row: newCancelRow(t)}, fakeCancelRPC{})
	if _, err := svc.Prepare(context.Background(), uuid.Nil); err == nil {
		t.Fatal("nil subscription id must error")
	}
}

func TestPrepareCancel_PropagatesRepoError(t *testing.T) {
	svc := NewPrepareCancelService(fakeSolanaSubReader{err: errors.New("boom")}, fakeCancelRPC{})
	if _, err := svc.Prepare(context.Background(), uuid.New()); err == nil {
		t.Fatal("repo error must propagate")
	}
}

func TestPrepareCancel_RejectsBadStoredKey(t *testing.T) {
	row := newCancelRow(t)
	row.SubscriberWallet = "not-a-key"
	svc := NewPrepareCancelService(fakeSolanaSubReader{row: row}, fakeCancelRPC{})
	if _, err := svc.Prepare(context.Background(), uuid.New()); err == nil {
		t.Fatal("invalid stored subscriber wallet must error")
	}
}
