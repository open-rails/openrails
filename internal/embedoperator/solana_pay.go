package embedoperator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ResolveSolanaPayReview closes a Solana Pay review receipt (a second, late,
// short or unreadable transfer, or an overpayment's excess) once its money was
// refunded or otherwise settled outside OpenRails. resolution says how.
// Unresolved reviews hold the billing archive back.
func (r *Operator) ResolveSolanaPayReview(ctx context.Context, merchantID merchant.ID, signature, resolution string) error {
	if err := r.initialized(); err != nil {
		return err
	}
	if r.app.Runtime.CheckoutSessionService == nil {
		return fmt.Errorf("operator: this runtime has no checkout sessions")
	}
	return r.app.Runtime.CheckoutSessionService.ResolveSolanaPayReview(merchant.WithID(ctx, merchantID), signature, resolution)
}
