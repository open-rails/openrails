package paymentmethods

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
)

// CustodianHandle identifies physical custody independently of local PSP aliases.
type CustodianHandle struct {
	Custodian uuid.UUID
	Method    string
}

// LockCustodianHandles follows the customer lock and precedes method locks.
// Remapping joins both old and new handles in one stable order. No caller may
// retain these transaction locks across a provider request.
func LockCustodianHandles(ctx context.Context, q *gen.Queries, merchant uuid.UUID, handles ...CustodianHandle) error {
	keys := make([]string, 0, len(handles))
	for _, h := range handles {
		if h.Custodian != uuid.Nil && h.Method != "" {
			keys = append(keys, "custodian-method:"+merchant.String()+":"+h.Custodian.String()+":"+h.Method)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := q.LockCustodianMethodHandle(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// RequireCustodianHandleAvailable runs under its handle lock. An accepted
// deletion and its completed physical tombstone remain authoritative after
// the local row disappears. A completed alias-only detach does not erase it.
func RequireCustodianHandleAvailable(ctx context.Context, q *gen.Queries, merchant uuid.UUID, h CustodianHandle) error {
	if h.Custodian == uuid.Nil || h.Method == "" {
		return nil
	}
	state, err := q.CustodianMethodDeletionState(ctx, gen.CustodianMethodDeletionStateParams{MerchantID: merchant, CustodianID: h.Custodian, MethodRef: h.Method})
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
