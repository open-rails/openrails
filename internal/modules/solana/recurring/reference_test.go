package recurring

import (
	"context"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
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

// programInstructionAccountCounts returns, per subscriptions-program instruction
// in tx, how many accounts it carries. The program treats a trailing account as
// an optional payer that must sign (or destructures an exact count), so a
// reference must never ride on one of these.
func programInstructionAccountCounts(t *testing.T, tx *solanago.Transaction) []int {
	t.Helper()
	var out []int
	for _, ix := range tx.Message.Instructions {
		prog, err := tx.Message.Program(ix.ProgramIDIndex)
		if err != nil {
			t.Fatalf("program index: %v", err)
		}
		if prog.Equals(subscriptions.ProgramID) {
			out = append(out, len(ix.Accounts))
		}
	}
	return out
}

func TestReferenceTagInstruction(t *testing.T) {
	payer := solanago.MustPublicKeyFromBase58(randKeyStr(t))

	// Empty reference -> nothing to tag.
	none, err := referenceTagInstruction(payer, "")
	if err != nil || none != nil {
		t.Fatalf("empty reference: ix=%v err=%v, want nil, nil", none, err)
	}
	ixs, err := withReference([]solanago.Instruction{}, payer, "")
	if err != nil || len(ixs) != 0 {
		t.Fatalf("withReference(empty) = %v, %v; want unchanged", ixs, err)
	}

	reference := randKeyStr(t)
	tag, err := referenceTagInstruction(payer, reference)
	if err != nil {
		t.Fatalf("referenceTagInstruction: %v", err)
	}
	if !tag.ProgramID().Equals(solanago.SystemProgramID) {
		t.Fatalf("tag program = %s, want System Program", tag.ProgramID())
	}
	data, _ := tag.Data()
	if len(data) != 12 || data[0] != 2 || data[4] != 0 {
		t.Fatalf("tag data = %x, want Transfer (index 2) of 0 lamports", data)
	}
	metas := tag.Accounts()
	if len(metas) != 3 {
		t.Fatalf("expected 3 accounts (payer, payer, reference), got %d", len(metas))
	}
	if !metas[0].PublicKey.Equals(payer) || !metas[0].IsSigner || !metas[0].IsWritable {
		t.Error("payer must be the signing, writable sender")
	}
	last := metas[2]
	if last.PublicKey.String() != reference {
		t.Errorf("trailing account = %s, want reference %s", last.PublicKey, reference)
	}
	if last.IsSigner || last.IsWritable {
		t.Error("reference meta must be read-only + non-signer")
	}

	if _, err := referenceTagInstruction(payer, "not-base58!"); err == nil {
		t.Error("invalid reference must error")
	}
}

// The reference never lands on a subscriptions-program instruction: cancel keeps
// its exact 5 accounts with the reference in a separate tag instruction.
func TestPrepareCancel_ReferenceDoesNotTouchProgramInstruction(t *testing.T) {
	row := newCancelRow(t)
	svc := NewPrepareCancelService(fakeCancelReader{row: row}, fakeCancelRPC{})

	res, err := svc.PrepareWithReference(context.Background(), uuid.New(), randKeyStr(t))
	if err != nil {
		t.Fatalf("PrepareWithReference: %v", err)
	}
	tx := decodeTx(t, res.Transaction)
	if got := programInstructionAccountCounts(t, tx); len(got) != 1 || got[0] != 5 {
		t.Fatalf("cancel_subscription accounts = %v, want [5]", got)
	}
	if len(tx.Message.Instructions) != 2 {
		t.Fatalf("referenced cancel tx should be [cancel, reference tag], got %d instructions", len(tx.Message.Instructions))
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
