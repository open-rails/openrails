package vault

import "testing"

// Pure path-arithmetic test (no Vault): confirms the KV-v2 data/metadata path
// mapping from the tenancy store's full logical paths. This is logic, not a mock
// of Vault behavior — the live behavior is covered by the build-tagged
// integration test.
func TestKVv2PathMapping(t *testing.T) {
	a := NewKVv2Adapter(nil, "secret")
	const full = "secret/openrails/merchants/00000000-0000-0000-0000-000000000001/provider_accounts/solana/live/AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9/private_key"

	if got, want := a.rest(full), "openrails/merchants/00000000-0000-0000-0000-000000000001/provider_accounts/solana/live/AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9/private_key"; got != want {
		t.Errorf("rest = %q, want %q", got, want)
	}
	if got, want := a.dataPath(full), "secret/data/openrails/merchants/00000000-0000-0000-0000-000000000001/provider_accounts/solana/live/AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9/private_key"; got != want {
		t.Errorf("dataPath = %q, want %q", got, want)
	}
	if got, want := a.metadataPath(full), "secret/metadata/openrails/merchants/00000000-0000-0000-0000-000000000001/provider_accounts/solana/live/AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9/private_key"; got != want {
		t.Errorf("metadataPath = %q, want %q", got, want)
	}
}
