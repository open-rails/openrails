package recurring

import (
	"context"
	"reflect"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
)

// subFakeRPCAbsent implements prepareRPC with a MISSING SubscriptionAuthority
// (empty account data) so Prepare returns the init step (first-time subscriber).
type subFakeRPCAbsent struct{ balance uint64 }

func (subFakeRPCAbsent) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	return nil, nil // absent → first-timer → init step
}
func (subFakeRPCAbsent) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash{}, nil
}
func (f subFakeRPCAbsent) GetTokenBalanceForMint(context.Context, solanago.PublicKey, solanago.PublicKey) (uint64, error) {
	return f.balance, nil
}

// A recurring subscribe over Solana Pay carries the reference in the SUBSCRIBE
// step's atomic bundle as a read-only, non-signer account — the same shape the
// poller needs to find the landed tx — on its own tag instruction, never on the
// program instructions (the program reads a trailing account as a payer that
// must sign). The bundle stays the co-signed [subscribe+transfer] plus the tag
// (2 required signers, 1 pre-signed).
func TestPrepareSubscribe_AttachesReferenceToSubscribeStep(t *testing.T) {
	svc, _ := newSubscribeSvc(t, subFakeRPC{initID: 42, balance: 50_000_000})
	in := newSubscribeInput(t)
	reference := randKeyStr(t)
	in.Reference = reference

	res, err := svc.Prepare(context.Background(), in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	tx := decodeTx(t, res.Transactions[0])
	found, ro := referenceInAccountKeys(t, tx, reference)
	if !found {
		t.Fatal("subscribe tx must contain the Solana Pay reference in its account keys")
	}
	if !ro {
		t.Error("reference must be a read-only, non-signer account")
	}
	// Reference must not disturb the atomic co-signing.
	if int(tx.Message.Header.NumRequiredSignatures) != 2 {
		t.Fatalf("want 2 required signers, got %d", tx.Message.Header.NumRequiredSignatures)
	}
	if len(tx.Message.Instructions) != 3 {
		t.Fatalf("subscribe bundle should be [subscribe, transfer, reference tag], got %d instructions", len(tx.Message.Instructions))
	}
	noRef := newSubscribeInput(t)
	noRef.SubscriberWallet = in.SubscriberWallet
	resNo, err := svc.Prepare(context.Background(), noRef)
	if err != nil {
		t.Fatalf("Prepare (no ref): %v", err)
	}
	want := programInstructionAccountCounts(t, decodeTx(t, resNo.Transactions[0]))
	if got := programInstructionAccountCounts(t, tx); !reflect.DeepEqual(got, want) {
		t.Fatalf("program instruction accounts with reference = %v, want unchanged %v", got, want)
	}
}

// A first-time subscriber over Solana Pay gets the one-step bundle [init,
// subscribe, transfer] tagged with the reference, so the poller detects the single
// landed tx and enrolls.
func TestPrepareSubscribe_AttachesReferenceToFirstTimerBundle(t *testing.T) {
	// Shrink the read-after-write retry so the absent-authority path (which retries
	// the empty read up to the bound before treating it as first-time) is fast.
	svc, _ := newSubscribeSvc(t, subFakeRPCAbsent{balance: 50_000_000})
	svc.authorityReadBackoff = time.Millisecond
	in := newSubscribeInput(t)
	reference := randKeyStr(t)
	in.Reference = reference

	res, err := svc.Prepare(context.Background(), in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.AuthorityExists {
		t.Fatalf("AuthorityExists = true, want false (one-step first-timer)")
	}
	tx := decodeTx(t, res.Transactions[0])
	found, ro := referenceInAccountKeys(t, tx, reference)
	if !found {
		t.Fatal("first-timer bundle must contain the Solana Pay reference in its account keys")
	}
	if !ro {
		t.Error("reference must be a read-only, non-signer account")
	}
	if len(tx.Message.Instructions) != 4 {
		t.Fatalf("bundle should be [init, subscribe, transfer, reference tag], got %d instructions", len(tx.Message.Instructions))
	}
	// initialize_subscription_authority takes exactly 6 accounts; a 7th would be
	// read as an optional payer that must sign (NotSigner, code 100). subscribe
	// (8) and transfer_subscription (10) likewise carry only their own accounts.
	if got := programInstructionAccountCounts(t, tx); !reflect.DeepEqual(got, []int{6, 8, 10}) {
		t.Fatalf("program instruction accounts = %v, want [6 8 10]", got)
	}
}
