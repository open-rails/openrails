package paymentmethods

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
)

// LockNativeVault joins admission and local alias attachment before method
// locks. NMI refuses deletion of the final billing entry, so whole-vault
// deletion must retain its accepted absence of sibling aliases across HTTP.
func LockNativeVault(ctx context.Context, q *gen.Queries, merchant, psp uuid.UUID, vault string) error {
	if vault == "" {
		return nil
	}
	return q.LockCustodianMethodHandle(ctx, "native-vault:"+merchant.String()+":"+psp.String()+":"+vault)
}

func RequireNativeVaultAvailable(ctx context.Context, q *gen.Queries, merchant, psp uuid.UUID, vault, method string) error {
	if vault == "" {
		return nil
	}
	state, err := q.NativeVaultDeletionState(ctx, gen.NativeVaultDeletionStateParams{MerchantID: merchant, PspID: psp, CustomerRef: vault, MethodRef: method})
	if err != nil {
		return err
	}
	if state.Erased {
		return ErrPaymentMethodDeleteUnsafe
	}
	if state.Pending {
		return ErrPaymentMethodDeleteProcessing
	}
	return nil
}
