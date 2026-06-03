package recurring

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"testing"

	solanago "github.com/doujins-org/solana-go"
	"github.com/open-rails/openrails/pkg/tenant"
)

// fakePrepareRPC stubs the on-chain reads PrepareSubscribe needs.
type fakePrepareRPC struct {
	authData []byte // nil => authority does not exist
}

func (f fakePrepareRPC) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	return f.authData, nil
}
func (fakePrepareRPC) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash{}, nil
}

func newPrepareInput(t *testing.T) PrepareSubscribeInput {
	t.Helper()
	return PrepareSubscribeInput{
		TenantID:         tenant.DefaultID,
		SubscriberWallet: newMerchant(t).String(),
		PlanID:           7,
		MintSymbol:       "USDC",
		AmountBaseUnits:  10_000_000,
		PeriodHours:      720,
		PlanCreatedAt:    1_700_000_000,
	}
}

func authDataWithInitID(initID int64) []byte {
	data := make([]byte, subscriptionAuthorityInitIDOffset+8)
	binary.LittleEndian.PutUint64(data[subscriptionAuthorityInitIDOffset:], uint64(initID))
	return data
}

func TestPrepareSubscribe_FirstTime_ReturnsInit(t *testing.T) {
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	svc := NewPrepareSubscribeService(fs, fakePrepareRPC{authData: nil}, "mainnet")

	res, err := svc.Prepare(context.Background(), newPrepareInput(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.AuthorityExists {
		t.Error("authority should not exist when account data is nil")
	}
	if res.Step != "init" {
		t.Errorf("Step = %q, want init", res.Step)
	}
	if len(res.Transactions) != 1 {
		t.Fatalf("want 1 tx, got %d", len(res.Transactions))
	}
	if _, err := base64.StdEncoding.DecodeString(res.Transactions[0]); err != nil {
		t.Errorf("tx is not valid base64: %v", err)
	}
	if res.SubscriptionPDA == "" || res.AuthorityPDA == "" || res.PlanPDA == "" {
		t.Error("derived PDAs must be populated")
	}
}

func TestPrepareSubscribe_Returning_ReturnsSubscribe(t *testing.T) {
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	svc := NewPrepareSubscribeService(fs, fakePrepareRPC{authData: authDataWithInitID(42)}, "mainnet")

	res, err := svc.Prepare(context.Background(), newPrepareInput(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !res.AuthorityExists {
		t.Error("authority should exist when account data is present")
	}
	if res.Step != "subscribe" {
		t.Errorf("Step = %q, want subscribe", res.Step)
	}
	if len(res.Transactions) != 1 {
		t.Fatalf("want 1 tx, got %d", len(res.Transactions))
	}
	if _, err := base64.StdEncoding.DecodeString(res.Transactions[0]); err != nil {
		t.Errorf("tx is not valid base64: %v", err)
	}
}

func TestPrepareSubscribe_RejectsIneligibleToken(t *testing.T) {
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	svc := NewPrepareSubscribeService(fs, fakePrepareRPC{authData: authDataWithInitID(1)}, "mainnet")
	in := newPrepareInput(t)
	in.MintSymbol = "PYUSD" // not recurring-eligible
	if _, err := svc.Prepare(context.Background(), in); err == nil {
		t.Fatal("PYUSD must be rejected (not recurring-eligible)")
	}
}

func TestPrepareSubscribe_RejectsBadInput(t *testing.T) {
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	svc := NewPrepareSubscribeService(fs, fakePrepareRPC{}, "mainnet")
	in := newPrepareInput(t)
	in.SubscriberWallet = "not-a-key"
	if _, err := svc.Prepare(context.Background(), in); err == nil {
		t.Fatal("invalid subscriber wallet must error")
	}
	in2 := newPrepareInput(t)
	in2.AmountBaseUnits = 0
	if _, err := svc.Prepare(context.Background(), in2); err == nil {
		t.Fatal("zero amount must error")
	}
}

func TestReadInitID(t *testing.T) {
	if _, err := readInitID(make([]byte, 10)); err == nil {
		t.Error("short data must error")
	}
	got, err := readInitID(authDataWithInitID(123456789))
	if err != nil {
		t.Fatalf("readInitID: %v", err)
	}
	if got != 123456789 {
		t.Errorf("initID = %d, want 123456789", got)
	}
}
