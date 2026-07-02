package routesurface

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
)

func solanaRails() config.RailMerchantAccountSet {
	return config.RailMerchantAccountSet{
		"solana": &config.RailMerchantAccountConfig{Rail: models.RailSolana, Solana: &config.SolanaRailConfig{}},
	}
}

// #661: Solana-signing routes and the secret-write surface mount only when
// OpenRails can actually perform them; one-off Solana stays on the config alone.
func TestProviderRoutesCapabilityGating(t *testing.T) {
	rails := solanaRails()

	full := ProviderRoutesFromRailsWithCapabilities(rails, RuntimeCapabilities{SolanaCanSign: true, SecretWrite: true})
	if !full.Solana || !full.SolanaSigning || !full.SecretWrite {
		t.Fatalf("full caps = %+v, want all on", full)
	}

	noSign := ProviderRoutesFromRailsWithCapabilities(rails, RuntimeCapabilities{SolanaCanSign: false, SecretWrite: true})
	if !noSign.Solana || noSign.SolanaSigning {
		t.Fatalf("noSign = %+v, want Solana && !SolanaSigning", noSign)
	}

	noWrite := ProviderRoutesFromRailsWithCapabilities(rails, RuntimeCapabilities{SolanaCanSign: true, SecretWrite: false})
	if noWrite.SecretWrite {
		t.Fatalf("noWrite.SecretWrite = true, want false")
	}

	// Back-compat constructor (no probed caps) stays permissive.
	perm := ProviderRoutesFromRails(rails)
	if !perm.Solana || !perm.SolanaSigning || !perm.SecretWrite {
		t.Fatalf("ProviderRoutesFromRails not permissive: %+v", perm)
	}

	// AllProviderRoutes (standalone multi-merchant) is fully permissive.
	all := AllProviderRoutes()
	if !all.SolanaSigning || !all.SecretWrite {
		t.Fatalf("AllProviderRoutes not permissive: %+v", all)
	}
}
