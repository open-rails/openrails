package vault

import (
	solanasign "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
)

// Compile-time proof that the adapters satisfy the consumer interfaces declared
// elsewhere (so neither tenancy nor solana imports hashicorp/vault/api). If a
// consumer interface changes, this fails to compile rather than at runtime.
var (
	_ merchants.VaultKV        = (*KVv2Adapter)(nil)
	_ solanasign.TransitClient = (*TransitAdapter)(nil)
)
