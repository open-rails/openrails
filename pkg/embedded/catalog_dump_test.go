package embedded

import "testing"

func TestProviderLinksOmitsSolanaMintSnapshot(t *testing.T) {
	links := providerLinks([]byte(`{
		"solana": {
			"rail": "solana",
			"token": "USD1",
			"mint_symbol": "USD1",
			"plan_pda": "plan"
		}
	}`))

	solana := links["solana"]
	if solana["plan_pda"] != "plan" {
		t.Fatalf("declarative Solana link fields were not preserved: %v", solana)
	}
	if _, ok := solana["token"]; ok {
		t.Fatalf("token must not be dumped beside an authoritative plan_pda: %v", solana)
	}
	if _, ok := solana["mint_symbol"]; ok {
		t.Fatalf("Solana mint snapshot must not be dumped as manifest input: %v", solana)
	}
	if _, ok := solana["rail"]; ok {
		t.Fatalf("storage rail stamp must not be dumped as manifest input: %v", solana)
	}

	defaultLink := providerLinks([]byte(`{
		"solana": {
			"rail": "solana",
			"token": "USDC",
			"mint_symbol": "USDC",
			"plan_pda": "default-plan"
		}
	}`))["solana"]
	if _, ok := defaultLink["token"]; ok {
		t.Fatalf("default USDC token should be omitted from catalog dumps: %v", defaultLink)
	}

	tokenOnly := providerLinks([]byte(`{
		"solana": {
			"rail": "solana",
			"token": "USD1"
		}
	}`))["solana"]
	if tokenOnly["token"] != "USD1" {
		t.Fatalf("USD1 creation intent should be preserved without a plan_pda: %v", tokenOnly)
	}
}
