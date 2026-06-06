package recurring

import (
	"context"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

// referenceInAccountKeys reports whether the given base58 pubkey appears in the
// transaction's account list as a read-only, NON-SIGNER account — exactly the
// shape a Solana Pay reference must take so the poller can find the tx via
// getSignaturesForAddress.
func referenceInAccountKeys(t *testing.T, tx *solanago.Transaction, reference string) (found, readonlyNonSigner bool) {
	t.Helper()
	refKey, err := solanago.PublicKeyFromBase58(reference)
	if err != nil {
		t.Fatalf("bad reference: %v", err)
	}
	for _, key := range tx.Message.AccountKeys {
		if !key.Equals(refKey) {
			continue
		}
		isSigner := tx.Message.IsSigner(key)
		isWritable, _ := tx.Message.IsWritable(key)
		return true, !isSigner && !isWritable
	}
	return false, false
}

// fakeCancelReader returns a fixed on-chain row for the cancel prepare path.
type fakeCancelReader struct{ row *models.SolanaSubscription }

func (f fakeCancelReader) GetBySubscriptionID(context.Context, uuid.UUID) (*models.SolanaSubscription, error) {
	return f.row, nil
}

// fakeCancelRPC serves a zero blockhash so Prepare can build the tx offline.
type fakeCancelRPC struct{}

func (fakeCancelRPC) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash{}, nil
}

func newCancelRow(t *testing.T) *models.SolanaSubscription {
	t.Helper()
	return &models.SolanaSubscription{
		SubscriberWallet: randKeyStr(t),
		PlanPDA:          randKeyStr(t),
		SubscriptionPDA:  randKeyStr(t),
		MerchantAddress:  randKeyStr(t),
	}
}

func TestPrepareCancel_AttachesReference(t *testing.T) {
	row := newCancelRow(t)
	svc := NewPrepareCancelService(fakeCancelReader{row: row}, fakeCancelRPC{})
	reference := randKeyStr(t)

	res, err := svc.PrepareWithReference(context.Background(), uuid.New(), reference)
	if err != nil {
		t.Fatalf("PrepareWithReference: %v", err)
	}
	tx := decodeTx(t, res.Transaction)
	found, ro := referenceInAccountKeys(t, tx, reference)
	if !found {
		t.Fatal("cancel tx must contain the Solana Pay reference in its account keys")
	}
	if !ro {
		t.Error("reference must be a read-only, non-signer account")
	}
	if res.SubscriptionPDA != row.SubscriptionPDA {
		t.Errorf("SubscriptionPDA = %q, want %q", res.SubscriptionPDA, row.SubscriptionPDA)
	}
}

func TestPrepareCancel_NoReferenceWhenEmpty(t *testing.T) {
	row := newCancelRow(t)
	svc := NewPrepareCancelService(fakeCancelReader{row: row}, fakeCancelRPC{})

	// Both Prepare and PrepareWithReference("") must build the SAME tx (no extra
	// account), so the existing auth-gated cancel handler is unaffected.
	res, err := svc.Prepare(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	tx := decodeTx(t, res.Transaction)
	// cancel_subscription has 5 accounts; no reference appended.
	if got := len(tx.Message.AccountKeys); got != 5 {
		t.Errorf("unreferenced cancel tx should have 5 account keys, got %d", got)
	}
}

func TestPrepareTierChange_AttachesReference(t *testing.T) {
	svc := newTierChangeSvc(t)
	reference := randKeyStr(t)

	// Downgrade keeps the tx fully unsigned + subscriber-only, so the reference is
	// unambiguously a read-only non-signer.
	in := newTierChangeInput(t)
	in.IsUpgrade = false
	in.Reference = reference

	res, err := svc.Prepare(context.Background(), in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	tx := decodeTx(t, res.Transaction)
	found, ro := referenceInAccountKeys(t, tx, reference)
	if !found {
		t.Fatal("tier-change tx must contain the Solana Pay reference in its account keys")
	}
	if !ro {
		t.Error("reference must be a read-only, non-signer account")
	}
}

func TestWithReferenceMeta(t *testing.T) {
	prog := solanago.MustPublicKeyFromBase58(randKeyStr(t))
	acct := solanago.MustPublicKeyFromBase58(randKeyStr(t))
	var base solanago.Instruction = solanago.NewInstruction(prog, solanago.AccountMetaSlice{
		solanago.NewAccountMeta(acct, true, true),
	}, []byte{1})

	// Empty reference -> unchanged instruction.
	same, err := withReferenceMeta(base, "")
	if err != nil {
		t.Fatalf("empty reference: %v", err)
	}
	if len(same.(*solanago.GenericInstruction).AccountValues) != 1 {
		t.Error("empty reference must not add accounts")
	}

	reference := randKeyStr(t)
	tagged, err := withReferenceMeta(base, reference)
	if err != nil {
		t.Fatalf("withReferenceMeta: %v", err)
	}
	metas := tagged.(*solanago.GenericInstruction).AccountValues
	if len(metas) != 2 {
		t.Fatalf("expected 2 accounts after tagging, got %d", len(metas))
	}
	last := metas[1]
	if last.PublicKey.String() != reference {
		t.Errorf("trailing account = %s, want reference %s", last.PublicKey, reference)
	}
	if last.IsSigner || last.IsWritable {
		t.Error("reference meta must be read-only + non-signer")
	}
	// The original instruction must be untouched (we copy).
	if len(base.(*solanago.GenericInstruction).AccountValues) != 1 {
		t.Error("withReferenceMeta must not mutate the input instruction")
	}

	if _, err := withReferenceMeta(base, "not-base58!"); err == nil {
		t.Error("invalid reference must error")
	}
}

func TestPrepareTierChange_UpgradeWithReference(t *testing.T) {
	svc := newTierChangeSvc(t)
	reference := randKeyStr(t)
	in := newTierChangeInput(t)
	in.IsUpgrade = true
	in.FirstChargeBaseUnits = 31_330_000
	in.Reference = reference

	res, err := svc.Prepare(context.Background(), in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	tx := decodeTx(t, res.Transaction)
	if found, ro := referenceInAccountKeys(t, tx, reference); !found || !ro {
		t.Fatalf("upgrade tx must carry the reference read-only/non-signer (found=%v ro=%v)", found, ro)
	}
	// Attaching the reference must not disturb the co-signing: still exactly one
	// pre-signed (cranker) slot of two required signers.
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
		t.Errorf("exactly one slot (cranker) should be pre-signed, got %d", signed)
	}
}
