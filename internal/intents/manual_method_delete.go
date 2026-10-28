package intents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
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

// RetireUnattemptedUserMethodDelete retires only an old queued, unattempted
// request. Callers explicitly name its owned method; uncertain work must instead
// be verified by an explicit manual retry.
func (s *Store) RetireUnattemptedUserMethodDelete(ctx context.Context, id uuid.UUID, pm *models.PaymentMethod, now time.Time) error {
	if pm == nil {
		return paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: pm.CustomerID}); err != nil {
			return err
		}
		if pm.Custodian == models.CustodianPSP {
			if err := paymentmethods.LockNativeVault(ctx, q, mid.UUID(), pm.HoldingPSP(), pm.RailCustomerRef); err != nil {
				return err
			}
		}
		current, err := q.LockPaymentMethodForCustodyRemap(ctx, gen.LockPaymentMethodForCustodyRemapParams{MerchantID: mid.UUID(), ID: pm.ID})
		if err != nil {
			return err
		}
		row, err := s.withTxDB(d).Get(ctx, id)
		if err != nil {
			return err
		}
		var retired struct {
			Retired bool `json:"retired_for_manual_delete"`
		}
		_ = json.Unmarshal(row.ResultEvidence, &retired)
		if row.Status == StatusSuperseded && retired.Retired && deleteRecordMatches(row, pm) {
			return nil
		}
		if !deleteRecordMatches(row, pm) || row.Origin != string(OriginUser) || strings.HasPrefix(row.IdempotencyKey, "manual:") || current.CustomerID != pm.CustomerID || models.DerefStr(current.ParkReason) != "delete:"+id.String() {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		n, err := q.RetireUnattemptedUserMethodDelete(ctx, gen.RetireUnattemptedUserMethodDeleteParams{MerchantID: mid.UUID(), OperationID: id, PaymentMethodID: pm.ID, Now: now})
		if err != nil {
			return err
		}
		if n != 1 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		return nil
	})
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
