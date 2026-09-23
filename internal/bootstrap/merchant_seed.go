package bootstrap

import (
	"fmt"

	"github.com/open-rails/openrails/config"
)

// ResolvePushMerchantConfigOptions accepts create-only initialization. Metadata
// changes use explicit application IDs/revisions; managed credentials use their
// provider publication operation. Custody never selects metadata authority.
func ResolvePushMerchantConfigOptions(cfg *config.Config, seed, insert, overwrite, prune bool) (MerchantManifestReconcileOptions, error) {
	if overwrite || prune {
		return MerchantManifestReconcileOptions{}, fmt.Errorf("--overwrite/--prune are retired: use a metadata application with expected revision or the provider publication API")
	}
	return MerchantManifestReconcileOptions{Insert: insert || seed}, nil
}
