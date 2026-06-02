package recurring

import (
	"context"
	"testing"

	solanago "github.com/doujins-org/solana-go"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

func TestCrankBuildsTransferSubscription(t *testing.T) {
	merchant := newMerchant(t)
	subscriber := newMerchant(t)
	mint := newMerchant(t)
	planPDA := newMerchant(t)
	subPDA := newMerchant(t)
	saPDA := newMerchant(t)

	fs := &fakeSubmitter{merchant: merchant}
	svc := NewCrankService(fs)
	sub := &models.SolanaSubscription{
		MerchantAddress:  merchant.String(),
		Mint:             mint.String(),
		SubscriberWallet: subscriber.String(),
		PlanPDA:          planPDA.String(),
		SubscriptionPDA:  subPDA.String(),
		AuthorityPDA:     saPDA.String(),
	}

	sig, err := svc.Crank(context.Background(), tenant.DefaultID, sub, 10_000_000)
	if err != nil {
		t.Fatalf("Crank: %v", err)
	}
	if sig == "" {
		t.Fatal("expected a signature")
	}
	if fs.submits != 1 || len(fs.lastInstrs) != 1 {
		t.Fatalf("expected one submitted instruction, got %d", fs.submits)
	}
	ix := fs.lastInstrs[0]
	if !ix.ProgramID().Equals(subscriptions.ProgramID) {
		t.Errorf("program = %s, want subscriptions program", ix.ProgramID())
	}
	accs := ix.Accounts()
	if len(accs) != 10 {
		t.Fatalf("transfer_subscription accounts = %d, want 10", len(accs))
	}
	// caller (index 5) is the merchant and the sole signer.
	if !accs[5].PublicKey.Equals(merchant) || !accs[5].IsSigner {
		t.Error("caller (index 5) must be the merchant signer")
	}
	// delegator ATA (index 3) is the subscriber's canonical ATA.
	wantDelegatorATA, _, _ := subscriptions.DeriveATA(subscriber, mint, solanago.TokenProgramID)
	if !accs[3].PublicKey.Equals(wantDelegatorATA) {
		t.Error("delegator ATA (index 3) must be subscriber's canonical ATA")
	}
}

func TestCrankRejectsBadInput(t *testing.T) {
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	svc := NewCrankService(fs)
	// zero amount
	if _, err := svc.Crank(context.Background(), tenant.DefaultID, &models.SolanaSubscription{}, 0); err == nil {
		t.Error("zero amount should error")
	}
	// invalid pubkey field
	bad := &models.SolanaSubscription{MerchantAddress: "not-a-key", Mint: "x", SubscriberWallet: "y", PlanPDA: "z", SubscriptionPDA: "w", AuthorityPDA: "v"}
	if _, err := svc.Crank(context.Background(), tenant.DefaultID, bad, 1); err == nil {
		t.Error("invalid pubkey should error")
	}
	if fs.submits != 0 {
		t.Error("bad input must never reach submit")
	}
}
