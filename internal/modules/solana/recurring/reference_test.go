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
	if got := programInstructionAccountCounts(t, tx); len(got) != 1 || got[0] != 5 {
		t.Fatalf("cancel instruction accounts = %v, want [5]", got)
	}
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
