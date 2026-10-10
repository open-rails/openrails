package bootstrap

import (
	"fmt"

	"github.com/open-rails/openrails/internal/config"
)

// ResolvePushMerchantConfigOptions accepts create-only provisioning of the
// manifest's merchants. Configuration changes are edits at a revision.
func ResolvePushMerchantConfigOptions(cfg *config.Config, seed, insert, overwrite, prune bool) (ReconcileOptions, error) {
	if overwrite || prune {
		return ReconcileOptions{}, fmt.Errorf("--overwrite/--prune are retired: edit the merchant's configuration at its revision (PATCH /v1/admin/configuration, the PSP routes)")
	}
	return ReconcileOptions{Insert: insert || seed}, nil
}
