package subscriptions

import (
	"encoding/binary"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
)

// buildPlanBlob assembles a synthetic Plan account in the exact on-chain byte
// order so we can prove DecodePlanAccount slices each field at the right offset.
// Field values are distinct so a transposed offset would surface as a wrong value.
func buildPlanBlob(t *testing.T) (blob []byte, owner, mint solanago.PublicKey, dests, pullers [4]solanago.PublicKey) {
	t.Helper()
	pk := func(seed byte) solanago.PublicKey {
		var b [32]byte
		for i := range b {
			b[i] = seed + byte(i)
		}
		return solanago.PublicKeyFromBytes(b[:])
	}
	owner = pk(1)
	mint = pk(40)
	for i := range 4 {
		dests[i] = pk(byte(80 + i*32))
		pullers[i] = pk(byte(200 + i*10))
	}

	blob = make([]byte, 0, PlanAccountSize)
	blob = append(blob, planAccountDiscriminator) // discriminator
	blob = append(blob, owner.Bytes()...)         // owner
	blob = append(blob, 254)                      // bump
	blob = append(blob, 2)                        // status
	u64 := func(v uint64) { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); blob = append(blob, b...) }
	u64(123456)                          // planId
	blob = append(blob, mint.Bytes()...) // mint
	u64(990000)                          // amount
	u64(720)                             // periodHours (30d)
	u64(uint64(int64(1717200000)))       // createdAt
	u64(uint64(int64(0)))                // endTs (perpetual)
	for i := range 4 {
		blob = append(blob, dests[i].Bytes()...)
	}
	for i := range 4 {
		blob = append(blob, pullers[i].Bytes()...)
	}
	meta := make([]byte, metadataURILen)
	copy(meta, "ipfs://plan-meta")
	blob = append(blob, meta...)
	return blob, owner, mint, dests, pullers
}

func TestDecodePlanAccount_RoundTrip(t *testing.T) {
	blob, owner, mint, dests, pullers := buildPlanBlob(t)
	if len(blob) != PlanAccountSize {
		t.Fatalf("synthetic blob is %d bytes, expected PlanAccountSize=%d", len(blob), PlanAccountSize)
	}

	p, err := DecodePlanAccount(blob)
	if err != nil {
		t.Fatalf("DecodePlanAccount: %v", err)
	}
	if !p.Owner.Equals(owner) {
		t.Errorf("owner = %s, want %s", p.Owner, owner)
	}
	if p.Bump != 254 {
		t.Errorf("bump = %d, want 254", p.Bump)
	}
	if p.Status != 2 {
		t.Errorf("status = %d, want 2", p.Status)
	}
	if p.PlanID != 123456 {
		t.Errorf("planId = %d, want 123456", p.PlanID)
	}
	if !p.Mint.Equals(mint) {
		t.Errorf("mint = %s, want %s", p.Mint, mint)
	}
	if p.Amount != 990000 {
		t.Errorf("amount = %d, want 990000", p.Amount)
	}
	if p.PeriodHours != 720 {
		t.Errorf("periodHours = %d, want 720", p.PeriodHours)
	}
	if p.CreatedAt != 1717200000 {
		t.Errorf("createdAt = %d, want 1717200000", p.CreatedAt)
	}
	if p.EndTs != 0 {
		t.Errorf("endTs = %d, want 0", p.EndTs)
	}
	for i := range 4 {
		if !p.Destinations[i].Equals(dests[i]) {
			t.Errorf("destinations[%d] = %s, want %s", i, p.Destinations[i], dests[i])
		}
		if !p.Pullers[i].Equals(pullers[i]) {
			t.Errorf("pullers[%d] = %s, want %s", i, p.Pullers[i], pullers[i])
		}
	}
	if p.MetadataURI != "ipfs://plan-meta" {
		t.Errorf("metadataUri = %q, want %q", p.MetadataURI, "ipfs://plan-meta")
	}
}

func TestDecodePlanAccount_Rejects(t *testing.T) {
	blob, _, _, _, _ := buildPlanBlob(t)

	if _, err := DecodePlanAccount(blob[:PlanAccountSize-1]); err == nil {
		t.Error("expected error for short buffer")
	}
	bad := make([]byte, PlanAccountSize)
	copy(bad, blob)
	bad[0] = 9 // wrong discriminator
	if _, err := DecodePlanAccount(bad); err == nil {
		t.Error("expected error for wrong discriminator")
	}
}
