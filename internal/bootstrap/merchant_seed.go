package bootstrap

import (
	"fmt"

	"github.com/open-rails/openrails/config"
)

// ResolvePushMerchantConfigOptions maps push-merchant-config CLI flags onto
// reconcile options per the merchant-source doctrine (#723/#851):
//
//   - MODE 1 (manifest): --insert/--overwrite/--prune compose as documented;
//     --seed is refused (the manifest is already the truth — nothing to import).
//   - MODE 2 (api): the command is a SEED-ONCE importer. --seed is required
//     (without it the command refuses, preserving the two-truths protection
//     against accidental manifest drift) and implies create-only semantics:
//     missing merchants/PSPs/secrets are created into the persistent stores
//     through the same store services the HTTP APIs use; existing values are
//     never re-asserted. The mutation flags are refused with --seed — after
//     seeding, the APIs own merchant config.
func ResolvePushMerchantConfigOptions(cfg *config.Config, seed, insert, overwrite, prune bool) (MerchantManifestReconcileOptions, error) {
	if cfg.IsManifestMerchantSource() {
		if seed {
			return MerchantManifestReconcileOptions{}, fmt.Errorf("--seed is the merchant_source=api importer gate (#851); manifest mode (#723) is already manifest-is-truth — use --insert/--overwrite/--prune")
		}
		return MerchantManifestReconcileOptions{Insert: insert, Overwrite: overwrite, Prune: prune}, nil
	}
	if !seed {
		return MerchantManifestReconcileOptions{}, fmt.Errorf("merchant_source=api: push-merchant-config runs only as a seed-once importer — pass --seed to bootstrap merchants/PSPs/secrets into the persistent stores; afterward the HTTP APIs own merchant config (two truths, #723/#851)")
	}
	if insert || overwrite || prune {
		return MerchantManifestReconcileOptions{}, fmt.Errorf("--seed does not combine with --insert/--overwrite/--prune: seeding is create-only and never re-asserts the manifest over API-owned state (#851)")
	}
	return MerchantManifestReconcileOptions{Insert: true}, nil
}
