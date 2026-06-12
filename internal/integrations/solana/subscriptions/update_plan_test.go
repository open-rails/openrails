package subscriptions

import (
	"bytes"
	"encoding/binary"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
)

// TestBuildUpdatePlanByteLayout pins update_plan's exact wire encoding to the
// IDL: u8 discriminator (8) + updatePlanData{status u8, endTs i64 le,
// pullers [4]pubkey, metadataUri fixed 128 utf8}.
func TestBuildUpdatePlanByteLayout(t *testing.T) {
	owner := mustKey(t)
	planPDA := mustKey(t)
	var pullers [4]solanago.PublicKey
	pullers[0] = mustKey(t)

	ix, err := BuildUpdatePlan(UpdatePlanParams{
		Owner:       owner,
		PlanPDA:     planPDA,
		Status:      PlanStatusSunset,
		EndTs:       -7,
		Pullers:     pullers,
		MetadataURI: "ipfs://meta",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !ix.ProgramID().Equals(ProgramID) {
		t.Fatal("wrong program id")
	}

	// Accounts (IDL order): owner(signer, not writable), planPda(writable).
	accounts := ix.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	if !accounts[0].PublicKey.Equals(owner) || !accounts[0].IsSigner || accounts[0].IsWritable {
		t.Fatalf("owner must be signer+readonly, got %+v", accounts[0])
	}
	if !accounts[1].PublicKey.Equals(planPDA) || accounts[1].IsSigner || !accounts[1].IsWritable {
		t.Fatalf("planPda must be writable non-signer, got %+v", accounts[1])
	}

	data, err := ix.Data()
	if err != nil {
		t.Fatalf("data: %v", err)
	}
	wantLen := 1 + 1 + 8 + 4*32 + 128
	if len(data) != wantLen {
		t.Fatalf("expected %d data bytes, got %d", wantLen, len(data))
	}
	if data[0] != 8 {
		t.Fatalf("discriminator must be 8, got %d", data[0])
	}
	if data[1] != PlanStatusSunset {
		t.Fatalf("status must be sunset (0), got %d", data[1])
	}
	var endTs int64
	if err := binary.Read(bytes.NewReader(data[2:10]), binary.LittleEndian, &endTs); err != nil || endTs != -7 {
		t.Fatalf("endTs must round-trip little-endian, got %d (err %v)", endTs, err)
	}
	if !bytes.Equal(data[10:42], pullers[0].Bytes()) {
		t.Fatal("pullers[0] mismatch")
	}
	uri := data[10+4*32:]
	if string(uri[:len("ipfs://meta")]) != "ipfs://meta" {
		t.Fatal("metadataUri prefix mismatch")
	}
	for _, b := range uri[len("ipfs://meta"):] {
		if b != 0 {
			t.Fatal("metadataUri must be zero-padded")
		}
	}

	// Over-long URI is rejected before any on-chain action.
	if _, err := BuildUpdatePlan(UpdatePlanParams{Owner: owner, PlanPDA: planPDA, MetadataURI: string(make([]byte, 129))}); err == nil {
		t.Fatal("expected over-long metadataUri to be rejected")
	}
}
