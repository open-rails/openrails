package recurring

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/pkg/tenant"
)

type tierChangeSecrets struct{ key string }

func (m tierChangeSecrets) GetSecret(context.Context, tenant.ID, string) (string, error) {
	return m.key, nil
}

// tcFakeRPC serves a present SubscriptionAuthority (initId at offset 98) + a zero
// blockhash, so Prepare builds the subscribe directly (no init step).
type tcFakeRPC struct{ initID int64 }

func (f tcFakeRPC) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	data := make([]byte, subscriptionAuthorityInitIDOffset+8)
	binary.LittleEndian.PutUint64(data[subscriptionAuthorityInitIDOffset:], uint64(f.initID))
	return data, nil
}
func (tcFakeRPC) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash{}, nil
}

// GetTokenBalanceForMint returns a large balance so the upgrade pre-flight passes
// in the existing tests (the insufficient case is covered separately).
func (tcFakeRPC) GetTokenBalanceForMint(context.Context, solanago.PublicKey, solanago.PublicKey) (uint64, error) {
	return 1_000_000_000, nil
}

func randKeyStr(t *testing.T) string {
	t.Helper()
	k, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKey().String()
}

func newTierChangeInput(t *testing.T) PrepareTierChangeInput {
	t.Helper()
	return PrepareTierChangeInput{
		TenantID:           dbtest.TestTenantID,
		SubscriberWallet:   randKeyStr(t),
		MintSymbol:         "USDC",
		OldPlanPDA:         randKeyStr(t),
		OldSubscriptionPDA: randKeyStr(t),
		NewPlanID:          99,
		NewAmountBaseUnits: 50_000_000,
		NewPeriodHours:     720,
		NewPlanCreatedAt:   1_700_000_000,
	}
}

func newTierChangeSvc(t *testing.T) *PrepareTierChangeService {
	t.Helper()
	merchant, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := solana.NewKeypairSigner(tierChangeSecrets{key: merchant.String()}, 0)
	return NewPrepareTierChangeService(signer, tcFakeRPC{initID: 42}, "devnet", testSolanaTokens())
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

func TestPrepareTierChange_Upgrade(t *testing.T) {
	svc := newTierChangeSvc(t)
	in := newTierChangeInput(t)
	in.IsUpgrade = true
	in.FirstChargeBaseUnits = 31_330_000 // prorated

	res, err := svc.Prepare(context.Background(), in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.Kind != "upgrade" {
		t.Fatalf("Kind = %q, want upgrade", res.Kind)
	}
	if res.NewSubscriptionPDA == "" {
		t.Error("NewSubscriptionPDA must be set")
	}
	tx := decodeTx(t, res.Transaction)
	if len(tx.Message.Instructions) != 3 {
		t.Fatalf("upgrade bundle should have 3 instructions (cancel+subscribe+transfer), got %d", len(tx.Message.Instructions))
	}
	// Two signers (subscriber fee payer + merchant transfer caller); the cranker
	// slot is pre-signed, the subscriber slot is empty for the wallet.
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

func TestPrepareTierChange_Downgrade(t *testing.T) {
	svc := newTierChangeSvc(t)
	in := newTierChangeInput(t)
	in.IsUpgrade = false

	res, err := svc.Prepare(context.Background(), in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.Kind != "downgrade" {
		t.Fatalf("Kind = %q, want downgrade", res.Kind)
	}
	tx := decodeTx(t, res.Transaction)
	if len(tx.Message.Instructions) != 2 {
		t.Fatalf("downgrade should have 2 instructions (cancel+subscribe), got %d", len(tx.Message.Instructions))
	}
	if int(tx.Message.Header.NumRequiredSignatures) != 1 {
		t.Fatalf("downgrade has a single signer (subscriber), got %d", tx.Message.Header.NumRequiredSignatures)
	}
	assertUnsignedSignatureSlots(t, tx)
}

func TestPrepareTierChange_RejectsBadInput(t *testing.T) {
	svc := newTierChangeSvc(t)
	in := newTierChangeInput(t)
	in.IsUpgrade = true
	in.FirstChargeBaseUnits = 0 // upgrade requires a prorated charge
	if _, err := svc.Prepare(context.Background(), in); err == nil {
		t.Fatal("upgrade with zero first charge must error")
	}
	in2 := newTierChangeInput(t)
	in2.MintSymbol = "PYUSD" // not recurring-eligible
	if _, err := svc.Prepare(context.Background(), in2); err == nil {
		t.Fatal("ineligible token must error")
	}
}
