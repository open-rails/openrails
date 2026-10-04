// Package settlement binds landed Solana transactions to the checkouts they
// settle.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// ErrClaimed refuses a landed transaction that already settles a different
// checkout, or a checkout already settled by a different transaction.
var ErrClaimed = errors.New("solana settlement: transaction and checkout are already settled apart")

// ClaimCheckout binds a verified landed transaction to the checkout it
// settles, before anything is granted. A signature settles at most one checkout
// across every merchant and PSP, and a checkout at most one signature; the
// first claim wins and repeating it is a no-op.
func ClaimCheckout(ctx context.Context, database *db.DB, sessionID uuid.UUID, signature string) error {
	signature = strings.TrimSpace(signature)
	if database == nil || sessionID == uuid.Nil || signature == "" {
		return fmt.Errorf("solana settlement: claim needs a database, a checkout and a signature")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	n, err := database.Gen(ctx).ClaimSolanaCheckoutSignature(ctx, gen.ClaimSolanaCheckoutSignatureParams{
		Signature: signature, MerchantID: mid.UUID(), ID: sessionID,
	})
	if db.IsUniqueViolation(err) {
		return fmt.Errorf("%w: %s settles another checkout", ErrClaimed, signature)
	}
	if err != nil {
		return fmt.Errorf("solana settlement: claim %s for checkout %s: %w", signature, sessionID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: checkout %s is settled by another transaction", ErrClaimed, sessionID)
	}
	return nil
}
