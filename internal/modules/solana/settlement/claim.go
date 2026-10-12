// Package settlement binds landed Solana transactions to the purchases they
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
// purchase, or a purchase already settled by a different transaction.
var ErrClaimed = errors.New("solana settlement: transaction and purchase are already settled apart")

// ClaimOrder binds a verified landed transaction to the purchase it
// settles, before anything is granted. A signature settles at most one purchase
// across every merchant and PSP, and a purchase at most one signature; the
// first claim wins and repeating it is a no-op.
func ClaimOrder(ctx context.Context, database *db.DB, sessionID uuid.UUID, signature string) error {
	signature = strings.TrimSpace(signature)
	if database == nil || sessionID == uuid.Nil || signature == "" {
		return fmt.Errorf("solana settlement: claim needs a database, a purchase and a signature")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	n, err := database.Gen(ctx).ClaimSolanaOrderSignature(ctx, gen.ClaimSolanaOrderSignatureParams{
		Signature: signature, MerchantID: mid.UUID(), ID: sessionID,
	})
	if db.IsUniqueViolation(err) {
		return fmt.Errorf("%w: %s settles another purchase", ErrClaimed, signature)
	}
	if err != nil {
		return fmt.Errorf("solana settlement: claim %s for purchase %s: %w", signature, sessionID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: purchase %s is settled by another transaction", ErrClaimed, sessionID)
	}
	return nil
}
