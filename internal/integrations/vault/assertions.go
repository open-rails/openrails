package vault

import solanasign "github.com/open-rails/openrails/internal/integrations/solana"

// Compile-time proof that the adapters satisfy the consumer interfaces declared
// elsewhere, so a consumer interface change fails here rather than at runtime.
var _ solanasign.TransitClient = (*TransitAdapter)(nil)
