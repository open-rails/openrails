package intents

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

type manualDeleteContextKey struct{}

var ErrManualPaymentMethodDeleteRequired = errors.New("card removal requires an explicit request; background execution is disabled")

func manualDeleteAllowed(ctx context.Context, id uuid.UUID) bool {
	allowed, _ := ctx.Value(manualDeleteContextKey{}).(uuid.UUID)
	return allowed == id && id != uuid.Nil
}

// ManualPaymentMethodDelete identifies request-only operations. Their ledger
// supplies locks and retry evidence, never permission for a worker to run them.
func ManualPaymentMethodDelete(in gen.BillingProviderIntent) bool {
	return manualMethodDeleteType(in.IntentType)
}

func manualMethodDeleteType(kind string) bool {
	return kind == TypeNMIPaymentMethodDelete || kind == TypePlatformMethodDelete || kind == TypeHyperSwitchMethodDelete
}

func manualMethodDeleteKey(kind string, id uuid.UUID) string {
	return "manual:" + kind + ":" + id.String()
}

func nativeMethodDeleteKeyMatches(key string, id uuid.UUID) bool {
	return methodDeleteKeyMatches(TypeNMIPaymentMethodDelete, key, id)
}

func methodDeleteKeyMatches(kind, key string, id uuid.UUID) bool {
	return key == kind+":"+id.String() || key == manualMethodDeleteKey(kind, id)
}

// executeManual runs at most one provider mutation for this HTTP request.
// A previous ambiguous attempt is read back first; it is never blindly sent
// again. A fresh ambiguous response gets one readback before returning an error.
func (t *PaymentMethodDeleteThrough) executeManual(ctx context.Context, in gen.BillingProviderIntent) (gen.BillingProviderIntent, error) {
	store, ok := t.Runner.Store.(*Store)
	if !ok || !ManualPaymentMethodDelete(in) {
		return in, errors.New("manual payment-method deletion store unavailable")
	}
	ctx = context.WithValue(ctx, manualDeleteContextKey{}, in.ID)
	row, err := store.PrepareDispatch(ctx, in.ID, t.Runner.now())
	if err != nil {
		return row, err
	}
	if row.Status == StatusUnknownNeedsVerify {
		row, err = t.Runner.VerifyByID(ctx, row.ID)
		if err != nil || row.Status != StatusFailedRetryable {
			return row, err
		}
	}
	// No scheduled wake runs a manual deletion: this request is its retry.
	row, err = t.Runner.ExecuteNowByID(ctx, row.ID)
	if err != nil || row.Status != StatusUnknownNeedsVerify {
		return row, err
	}
	return t.Runner.VerifyByID(ctx, row.ID)
}

func deleteRecordMatches(in gen.BillingProviderIntent, pm *models.PaymentMethod) bool {
	if pm == nil {
		return false
	}
	switch in.IntentType {
	case TypeNMIPaymentMethodDelete:
		p, err := decodeNMIVaultDeletePayload(in)
		return err == nil && p.PaymentMethodID == pm.ID && p.UserID == pm.CustomerID.String()
	case TypePlatformMethodDelete:
		p, err := DecodePlatformMethodDelete(in)
		return err == nil && p.PaymentMethodID == pm.ID && p.CustomerID == pm.CustomerID
	case TypeHyperSwitchMethodDelete:
		p, err := DecodeHyperSwitchMethodDelete(in)
		return err == nil && p.PaymentMethodID == pm.ID && p.CustomerID == pm.CustomerID
	}
	return false
}

// existingDelete preserves exact prior evidence when a person explicitly
// retries. Completed tombstones need no provider call; uncertain legacy work
// is verified by executeManual, not retired.
func (t *PaymentMethodDeleteThrough) existingDelete(ctx context.Context, pm *models.PaymentMethod) (gen.BillingProviderIntent, bool, error) {
	key := pm.ParkReason
	if !strings.HasPrefix(key, "delete:") && !strings.HasPrefix(key, "removed:") {
		return gen.BillingProviderIntent{}, false, nil
	}
	id, err := uuid.Parse(strings.SplitN(key, ":", 2)[1])
	if err != nil {
		return gen.BillingProviderIntent{}, false, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	row, err := t.Runner.Store.Get(ctx, id)
	if err != nil {
		return row, false, err
	}
	if !deleteRecordMatches(row, pm) {
		return row, false, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	return row, true, nil
}
