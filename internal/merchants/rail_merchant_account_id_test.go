package merchants

import "testing"

// TestRailMerchantAccountID pins the #662 provider-account id derivation: a pure
// function of the GLOBAL natural key (rail, environment, account_id), normalized
// the SAME way the ownership guard and the (rail, environment, account_id)
// unique index canonicalize it — so the id is identical across environments and
// fresh rebuilds, and the returned components are exactly what the row stores.
func TestRailMerchantAccountID(t *testing.T) {
	base := PspID("nmi", "live", "945280-0000")

	// Canonicalization: rail case/whitespace, and environment aliases, are
	// normalized before hashing (matching lower(rail) + the live/test mapping).
	same := []struct {
		rail, env, acct string
	}{
		{"NMI", "live", "945280-0000"},
		{" nmi ", "LIVE", "945280-0000"},
		{"nmi", "production", "945280-0000"}, // production → live
		{"nmi", "mainnet", "945280-0000"},    // mainnet → live
		{"nmi", "live", " 945280-0000 "},     // account_id trimmed
	}
	for _, c := range same {
		if got := PspID(c.rail, c.env, c.acct); got != base {
			t.Fatalf("(%q,%q,%q) must derive the same id as the canonical key", c.rail, c.env, c.acct)
		}
	}

	// Each natural-key component participates.
	if PspID("stripe", "live", "945280-0000") == base {
		t.Fatal("rail must change the id")
	}
	if PspID("nmi", "test", "945280-0000") == base {
		t.Fatal("environment must change the id")
	}
	if PspID("nmi", "live", "945280-0001") == base {
		t.Fatal("account_id must change the id")
	}

	// The natural-key helper returns the id plus the exact normalized components
	// the upsert stores, so the id corresponds 1:1 to the unique index.
	id, nRail, nEnv, nAcct := PSPNaturalKey("NMI", "production", " 945280-0000 ")
	if id != base {
		t.Fatal("PSPNaturalKey id must match PspID")
	}
	if nRail != "nmi" || nEnv != "live" || nAcct != "945280-0000" {
		t.Fatalf("normalized components = (%q,%q,%q); want (nmi,live,945280-0000)", nRail, nEnv, nAcct)
	}
}
