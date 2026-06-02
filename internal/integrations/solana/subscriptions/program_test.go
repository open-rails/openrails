package subscriptions

import (
	"testing"

	solanago "github.com/doujins-org/solana-go"
)

func mustKey(t *testing.T) solanago.PublicKey {
	t.Helper()
	k, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return k.PublicKey()
}

func TestPDADerivationsAreDeterministicAndOnCurveOff(t *testing.T) {
	user := mustKey(t)
	mint := mustKey(t)

	sa1, b1, err := DeriveSubscriptionAuthority(user, mint)
	if err != nil {
		t.Fatalf("SA: %v", err)
	}
	sa2, b2, err := DeriveSubscriptionAuthority(user, mint)
	if err != nil {
		t.Fatalf("SA2: %v", err)
	}
	if !sa1.Equals(sa2) || b1 != b2 {
		t.Fatal("subscription authority derivation not deterministic")
	}
	if solanago.IsOnCurve(sa1.Bytes()) {
		t.Fatal("PDA must be off-curve")
	}

	plan, _, err := DerivePlanPDA(user, 42)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	planOther, _, _ := DerivePlanPDA(user, 43)
	if plan.Equals(planOther) {
		t.Fatal("different plan_id must yield different PDA")
	}

	sub, _, err := DeriveSubscriptionPDA(plan, user)
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	if sub.Equals(plan) {
		t.Fatal("subscription PDA must differ from plan PDA")
	}
}

func TestEncodeCreatePlanByteLayout(t *testing.T) {
	merchant := mustKey(t)
	mint := mustKey(t)
	plan, _, _ := DerivePlanPDA(merchant, 1)
	data, err := encodeCreatePlan(CreatePlanParams{
		Merchant: merchant, PlanPDA: plan, Mint: mint, TokenProgram: solanago.TokenProgramID,
		PlanID: 1, Terms: PlanTerms{Amount: 10_000_000, PeriodHours: 720, CreatedAt: 1_700_000_000},
		EndTs: 0, MetadataURI: "https://x/p.json",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// 1 disc + 8 planId + 32 mint + 24 terms + 8 endTs + 128 destinations + 128 pullers + 128 uri
	const want = 1 + 8 + 32 + (8 + 8 + 8) + 8 + 4*32 + 4*32 + 128
	if len(data) != want {
		t.Fatalf("create_plan data len = %d, want %d", len(data), want)
	}
	if data[0] != discCreatePlan {
		t.Fatalf("first byte = %d, want discriminator %d", data[0], discCreatePlan)
	}
}

func TestInstructionAccountLayouts(t *testing.T) {
	k := func() solanago.PublicKey { return mustKey(t) }

	t.Run("transfer_subscription", func(t *testing.T) {
		ix := BuildTransferSubscription(TransferSubscriptionParams{
			SubscriptionPDA: k(), PlanPDA: k(), SubscriptionAuthority: k(),
			DelegatorATA: k(), ReceiverATA: k(), Caller: k(), Mint: k(),
			TokenProgram: solanago.TokenProgramID, EventAuthority: k(),
			Amount: 5, Delegator: k(),
		})
		accs := ix.Accounts()
		if len(accs) != 10 {
			t.Fatalf("transfer accounts = %d, want 10", len(accs))
		}
		// caller (index 5) must be the sole signer.
		if !accs[5].IsSigner {
			t.Fatal("transfer caller must be signer")
		}
		for i, a := range accs {
			if i != 5 && a.IsSigner {
				t.Fatalf("account %d unexpectedly a signer", i)
			}
		}
		// delegatorAta(3) + receiverAta(4) + subscriptionPda(0) writable.
		for _, w := range []int{0, 3, 4} {
			if !accs[w].IsWritable {
				t.Fatalf("account %d should be writable", w)
			}
		}
		if !ix.ProgramID().Equals(ProgramID) {
			t.Fatal("wrong program id")
		}
	})

	t.Run("create_plan", func(t *testing.T) {
		ix, err := BuildCreatePlan(CreatePlanParams{
			Merchant: k(), PlanPDA: k(), Mint: k(), TokenProgram: solanago.TokenProgramID,
			PlanID: 1, Terms: PlanTerms{Amount: 1, PeriodHours: 1, CreatedAt: 1},
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		accs := ix.Accounts()
		if len(accs) != 5 {
			t.Fatalf("create_plan accounts = %d, want 5", len(accs))
		}
		if !accs[0].IsSigner || !accs[0].IsWritable {
			t.Fatal("merchant must be signer+writable")
		}
	})

	t.Run("cancel", func(t *testing.T) {
		ix := BuildCancelSubscription(CancelOrResumeParams{Subscriber: k(), PlanPDA: k(), SubscriptionPDA: k(), EventAuthority: k()})
		accs := ix.Accounts()
		if len(accs) != 5 {
			t.Fatalf("cancel accounts = %d, want 5", len(accs))
		}
		if !accs[0].IsSigner {
			t.Fatal("subscriber must sign cancel")
		}
		d, _ := ix.Data()
		if len(d) != 1 || d[0] != discCancelSubscription {
			t.Fatalf("cancel data = %v, want [%d]", d, discCancelSubscription)
		}
	})
}

func TestMetadataURITooLong(t *testing.T) {
	long := make([]byte, 129)
	_, err := encodeCreatePlan(CreatePlanParams{MetadataURI: string(long)})
	if err == nil {
		t.Fatal("expected error for metadata_uri > 128 bytes")
	}
}
