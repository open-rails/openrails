package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// CollectionAdapter arms a rail-specific saved-method charge. Prepare performs
// only local validation and request building; the returned PreparedCharge's
// Submit is the provider submission.
type CollectionAdapter interface {
	Prepare(ctx context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (PreparedCharge, error)
}

// ScopedCharger validates merchant/customer/payment-method scope and resolves
// the store-armed adapter before an off-session invoice collection charge.
type ScopedCharger struct {
	db       *db.DB
	adapters map[string]CollectionAdapter
	resolver CollectionAdapterResolver
}

func NewScopedCharger(database *db.DB, adapters map[string]CollectionAdapter) *ScopedCharger {
	cp := make(map[string]CollectionAdapter, len(adapters))
	for rail, adapter := range adapters {
		rail = normalizeRail(rail)
		if rail == "" || adapter == nil {
			continue
		}
		cp[rail] = adapter
	}
	return &ScopedCharger{db: database, adapters: cp}
}

// SetAdapterResolver arms per-merchant store resolution (#725/#788). Once
// armed it is the ONLY source of collection adapters: a merchant with no
// declared account on the rail has nothing to charge with (or#893).
func (c *ScopedCharger) SetAdapterResolver(r CollectionAdapterResolver) {
	if c != nil {
		c.resolver = r
	}
}

// Prepare is the ONE dispatch point for every off-session collection charge.
// Nothing here reaches the provider.
func (c *ScopedCharger) Prepare(ctx context.Context, req ChargeRequest) (PreparedCharge, error) {
	if c == nil || c.db == nil {
		return nil, fmt.Errorf("scoped charger not initialized")
	}
	if req.PaymentMethodID == uuid.Nil {
		return nil, fmt.Errorf("payment_method_id required")
	}
	if req.Payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, fmt.Errorf("idempotency_key required")
	}
	// or#864: an absent currency is refused here, once, before any credential
	// is resolved. Registry-validated, not merely non-blank.
	req.Currency = normalizeCurrency(req.Currency)
	if err := moneyutil.ValidateCurrency(req.Currency); err != nil {
		return nil, fmt.Errorf("refusing to charge without an established currency: %w", err)
	}
	merchantID := req.MerchantID
	if merchantID == uuid.Nil {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		merchantID = tid.UUID()
		req.MerchantID = merchantID
	}

	method, err := c.db.Gen(ctx).GetPaymentMethodByID(ctx, req.PaymentMethodID)
	if err != nil {
		return nil, fmt.Errorf("load payment method: %w", err)
	}
	if method.MerchantID != merchantID {
		return nil, fmt.Errorf("payment method belongs to another merchant")
	}
	if method.CustomerID != req.Payer.UUID() {
		return nil, fmt.Errorf("payment method belongs to another customer")
	}

	rail := normalizeRail(method.Rail)
	if rail == "" {
		return nil, fmt.Errorf("payment method rail required")
	}
	if d, ok := rails.Lookup(models.Rail(rail)); ok && !d.SupportsChargeSavedMethod {
		return nil, fmt.Errorf("rail %q does not support invoice collection", rail)
	}

	// Store-armed per-merchant credentials are the ONLY source once a resolver
	// is armed; there is no boot-plane fallback (a fail-open credential path).
	adapter := c.adapters[rail]
	if c.resolver != nil {
		stored, ok, rerr := c.resolver.ResolveCollectionAdapter(ctx, method)
		if rerr != nil {
			return nil, fmt.Errorf("resolve merchant %s collection credentials: %w", rail, rerr)
		}
		if !ok {
			return nil, fmt.Errorf(
				"merchant %s has no armed PSP on rail %q: payment method %s cannot be charged until that account is declared and its credentials are stored",
				merchantID, rail, method.ID)
		}
		adapter = stored
	}
	if adapter == nil {
		return nil, fmt.Errorf("no invoice collection adapter configured for rail %q", rail)
	}
	inner, err := adapter.Prepare(ctx, method, req)
	if err != nil {
		return nil, err
	}
	return PreparedChargeFunc(func(ctx context.Context) (ChargeResult, error) {
		res, err := inner.Submit(ctx)
		if err != nil {
			return ChargeResult{}, err
		}
		if strings.TrimSpace(res.Rail) == "" {
			res.Rail = rail
		}
		// #297: a successful charge on an instrument with no stored-credential
		// replay reference anchors its unscheduled sequence, write-once.
		// Best-effort: the charge itself succeeded and must never fail on this.
		if ref := strings.TrimSpace(res.CapturedStoredCredentialRef); ref != "" && !res.Declined {
			if _, cerr := c.db.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{
				MerchantID: merchantID,
				ID:         method.ID,
				Agreement:  string(charge.AgreementUnscheduled),
				Ref:        ref,
			}); cerr != nil {
				log.WithContext(ctx).WithError(cerr).WithField("payment_method_id", method.ID).
					Warn("failed to persist captured stored-credential reference (#297); next charge re-captures")
			}
		}
		return res, nil
	}), nil
}

func normalizeRail(rail string) string {
	return strings.ToLower(strings.TrimSpace(rail))
}
