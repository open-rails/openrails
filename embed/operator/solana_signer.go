package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ApproveSolanaSigner accepts the Solana identity a Vault Transit signer key
// now reports after it changed (a rotated key, or Vault reached at another
// address, namespace or mount). Until then the Solana rail refuses and the
// openrails_solana_signer_identity probe fails. The previous identity is
// drained and the new one provisioned; approval is stored in Postgres.
// Verify the new public key (in the ERROR log and the probe) before calling.
func (r *Operator) ApproveSolanaSigner(ctx context.Context, merchantID merchant.ID, key string) error {
	if err := r.initialized(); err != nil {
		return err
	}
	if r.app.Runtime.ApproveSolanaSigner == nil {
		return fmt.Errorf("operator: this runtime cannot approve Solana signers")
	}
	return r.app.Runtime.ApproveSolanaSigner(ctx, merchantID, key)
}
