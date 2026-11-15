package hosttools

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchant"
)

// Local maintenance over an owned engine graph: process ownership is the
// authority, so none of these is reachable through a Client or HTTP route.

func initialized(a *app.App) error {
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return fmt.Errorf("openrails: engine is not initialized")
	}
	return nil
}

// ResolveSolanaPayReview closes a Solana Pay review receipt (a second, late,
// short or unreadable transfer, or an overpayment's excess) once its money was
// settled outside OpenRails. Unresolved reviews hold the billing archive back.
func ResolveSolanaPayReview(ctx context.Context, a *app.App, merchantID billing.MerchantID, signature, resolution string) error {
	if err := initialized(a); err != nil {
		return err
	}
	if a.Runtime.OrderAttemptService == nil {
		return fmt.Errorf("openrails: this engine has no order attempts")
	}
	return a.Runtime.OrderAttemptService.ResolveSolanaPayReview(merchant.WithID(ctx, merchantID), signature, resolution)
}
