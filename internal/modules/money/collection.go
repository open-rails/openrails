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

// CollectionAdapter charges a rail-specific saved payment method.
type CollectionAdapter interface {
	ChargeSavedMethod(ctx context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (ChargeResult, error)
}

// ScopedCharger validates merchant/customer/payment-method scope before
// dispatching an off-session invoice or top-up charge to a rail adapter.
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

// SetAdapterResolver arms per-merchant store resolution (#725/#788). The
// resolver is the only source of collection adapters now — there is no
// boot-config fallback plane; a merchant with no declared account on the
// rail simply has nothing to charge with.
func (c *ScopedCharger) SetAdapterResolver(r CollectionAdapterResolver) {
	if c != nil {
		c.resolver = r
	}
}

func (c *ScopedCharger) ChargeSavedMethod(ctx context.Context, req ChargeRequest) (ChargeResult, error) {
	if c == nil || c.db == nil {
		return ChargeResult{}, fmt.Errorf("scoped charger not initialized")
	}
	if req.PaymentMethodID == uuid.Nil {
		return ChargeResult{}, fmt.Errorf("payment_method_id required")
	}
	if req.Payer.IsZero() {
		return ChargeResult{}, fmt.Errorf("payer required")
	}
	// or#864: the ONE dispatch point for every off-session collection charge.
	// A rail adapter must never have to decide what an absent currency means —
	// the answer is always "refuse", and it is decided here, once, before any
	// credential is resolved.
	req.Currency = normalizeCurrency(req.Currency)
	if err := moneyutil.ValidateCurrency(req.Currency); err != nil {
		return ChargeResult{}, fmt.Errorf("refusing to charge without an established currency: %w", err)
	}
	merchantID := req.MerchantID
	if merchantID == uuid.Nil {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return ChargeResult{}, err
		}
		merchantID = tid.UUID()
		req.MerchantID = merchantID
	}

	method, err := c.db.Gen(ctx).GetPaymentMethodByID(ctx, req.PaymentMethodID)
	if err != nil {
		return ChargeResult{}, fmt.Errorf("load payment method: %w", err)
	}
	if method.MerchantID != merchantID {
		return ChargeResult{}, fmt.Errorf("payment method belongs to another merchant")
	}
	if method.CustomerID != req.Payer.UUID() {
		return ChargeResult{}, fmt.Errorf("payment method belongs to another customer")
	}

	rail := normalizeRail(method.Rail)
	if rail == "" {
		return ChargeResult{}, fmt.Errorf("payment method rail required")
	}
	// Registry-backed exclusion (#669): known rails that can't charge a saved
	// method fail explicitly; unknown rail strings fall to adapter-not-found.
	if d, ok := rails.Lookup(models.Rail(rail)); ok && !d.SupportsChargeSavedMethod {
		return ChargeResult{}, fmt.Errorf("rail %q does not support invoice collection", rail)
	}

	adapter := c.adapters[rail]
	// #725/#788: store-armed per-merchant credentials are the only source;
	// a declared account that cannot arm fails the charge closed.
	if c.resolver != nil {
		stored, ok, rerr := c.resolver.ResolveCollectionAdapter(ctx, method)
		if rerr != nil {
			return ChargeResult{}, fmt.Errorf("resolve merchant %s collection credentials: %w", rail, rerr)
		}
		if ok {
			adapter = stored
		}
	}
	if adapter == nil {
		return ChargeResult{}, fmt.Errorf("no invoice collection adapter configured for rail %q", rail)
	}
	res, err := adapter.ChargeSavedMethod(ctx, method, req)
	if err != nil {
		return ChargeResult{}, err
	}
	if strings.TrimSpace(res.Rail) == "" {
		res.Rail = rail
	}
	// #297: a successful charge on an instrument with no stored-credential
	// replay reference anchors its unscheduled sequence — persist write-once.
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
}

func normalizeRail(rail string) string {
	return strings.ToLower(strings.TrimSpace(rail))
}
