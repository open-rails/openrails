package recurring

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

const testDevnetUSDCMint = "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ"

var testMerchantID = merchant.ID(uuid.MustParse("a5a5a5a5-0000-4000-8000-000000000001"))

func testTokens() map[string]config.TokenConfig {
	return map[string]config.TokenConfig{"usdc": {Mint: testDevnetUSDCMint}}
}

func randKey(t *testing.T) solanago.PrivateKey {
	t.Helper()
	k, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func randAddr(t *testing.T) string { return randKey(t).PublicKey().String() }

type staticSecret string

func (s staticSecret) GetSecret(context.Context, merchant.ID, string) (string, error) {
	return string(s), nil
}

// newCranker returns a keypair signer and a submitter resolving the SAME
// merchant address the signer co-signs with.
func newCranker(t *testing.T) (solanaint.Signer, Submitter, solanago.PublicKey) {
	t.Helper()
	key := randKey(t)
	signer := solanaint.NewKeypairSigner(staticSecret(key.String()), 0)
	return signer, NewSignerSubmitter(signer, nil), key.PublicKey()
}

// chain fakes the RPC reads the prepare paths make. A nil authority means the
// subscriber's SubscriptionAuthority does not exist yet.
type chain struct {
	authorityInitID *int64
	balance         uint64
	balanceErr      error
}

func withAuthority(initID int64) *int64 { return &initID }

func (c chain) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	if c.authorityInitID == nil {
		return nil, nil
	}
	data := make([]byte, subscriptionAuthorityInitIDOffset+8)
	binary.LittleEndian.PutUint64(data[subscriptionAuthorityInitIDOffset:], uint64(*c.authorityInitID))
	return data, nil
}

func (chain) GetLatestBlockhash(context.Context) (solanago.Hash, error) { return solanago.Hash{}, nil }

func (c chain) GetTokenBalanceForMint(context.Context, solanago.PublicKey, solanago.PublicKey) (uint64, error) {
	return c.balance, c.balanceErr
}

func decodeTx(t *testing.T, b64 string) *solanago.Transaction {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("not base64: %v", err)
	}
	tx, err := solanago.TransactionFromBytes(raw)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	return tx
}

// txShape is the wallet-facing shape of a prepared transaction.
type txShape struct {
	instructions int
	signers      int
	preSigned    int
	programAccts []int // account count per subscriptions-program instruction
}

func shapeOf(t *testing.T, tx *solanago.Transaction) txShape {
	t.Helper()
	s := txShape{instructions: len(tx.Message.Instructions), signers: int(tx.Message.Header.NumRequiredSignatures)}
	if len(tx.Signatures) != s.signers {
		t.Fatalf("signature slots = %d, want one per required signer (%d)", len(tx.Signatures), s.signers)
	}
	for _, sig := range tx.Signatures {
		if !sig.IsZero() {
			s.preSigned++
		}
	}
	for _, ix := range tx.Message.Instructions {
		prog, err := tx.Message.Program(ix.ProgramIDIndex)
		if err != nil {
			t.Fatal(err)
		}
		if prog.Equals(subscriptions.ProgramID) {
			s.programAccts = append(s.programAccts, len(ix.Accounts))
		}
	}
	return s
}

// requireReferenceTag asserts the Solana Pay reference rides its own trailing
// zero-lamport System self-transfer from payer, as a read-only non-signer, so
// getSignaturesForAddress(reference) finds the tx without the subscriptions
// program mistaking it for an optional payer.
func requireReferenceTag(t *testing.T, tx *solanago.Transaction, payer, reference string) {
	t.Helper()
	tag := tx.Message.Instructions[len(tx.Message.Instructions)-1]
	prog, err := tx.Message.Program(tag.ProgramIDIndex)
	if err != nil || !prog.Equals(solanago.SystemProgramID) {
		t.Fatalf("last instruction must be the System reference tag, got %v (%v)", prog, err)
	}
	if len(tag.Data) != 12 || binary.LittleEndian.Uint32(tag.Data[:4]) != 2 || binary.LittleEndian.Uint64(tag.Data[4:]) != 0 {
		t.Fatalf("reference tag must be a zero-lamport transfer, data=%v", tag.Data)
	}
	keys := tx.Message.AccountKeys
	if len(tag.Accounts) != 3 || keys[tag.Accounts[0]].String() != payer || keys[tag.Accounts[1]].String() != payer ||
		keys[tag.Accounts[2]].String() != reference {
		t.Fatal("reference tag must be payer->payer with the reference as the third account")
	}
	ref := solanago.MustPublicKeyFromBase58(reference)
	writable, _ := tx.Message.IsWritable(ref)
	if tx.Message.IsSigner(ref) || writable {
		t.Fatal("reference must be read-only and non-signer")
	}
}
