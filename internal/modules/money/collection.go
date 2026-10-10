package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// CollectionAdapter arms a rail-specific saved-method charge. Prepare performs
// validation and request building, with at most read-only qualification.
// The returned PreparedCharge's Submit is the provider submission.
type CollectionAdapter interface {
	Prepare(ctx context.Context, method gen.BillingPaymentMethod, req ChargeRequest) (PreparedCharge, error)
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
// Nothing here sends a provider mutation.
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
	if err := moneyutil.RequireFiatCurrency(req.Currency); err != nil {
		return nil, fmt.Errorf("refusing to charge without an established currency: %w", err)
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	merchantID := tid.UUID()
	if req.MerchantID != uuid.Nil && req.MerchantID != merchantID {
		return nil, fmt.Errorf("charge request belongs to another merchant")
	}
	req.MerchantID = merchantID

	method, err := c.db.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID, ID: req.PaymentMethodID})
	if err != nil {
		return nil, fmt.Errorf("load payment method: %w", err)
	}
	if method.MerchantID != merchantID {
		return nil, fmt.Errorf("payment method belongs to another merchant")
	}
	if method.CustomerID != req.Payer.UUID() {
		return nil, fmt.Errorf("payment method belongs to another customer")
	}
	if !paymentmethods.Chargeable(method) {
		return nil, fmt.Errorf("%w: payment method cannot be charged", charge.ErrInstrumentChanged)
	}
	if err := req.Instrument.Validate(); err != nil {
		return nil, err
	}
	if err := req.Instrument.Matches(method); err != nil {
		return nil, err
	}
	if req.Initiator == charge.InitiatorMerchant {
		// The card lost its agreement after enqueue: nothing is sent, and the
		// next attempt stops for the customer.
		if err := requireCollectionAgreement(ctx, c.db.Gen(ctx), charge.CustodyOf(c.db), method, req.Currency); err != nil {
			if errors.Is(err, charge.ErrAgreementRequired) {
				return nil, fmt.Errorf("%w: %w", charge.ErrInstrumentChanged, err)
			}
			return nil, err
		}
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
		stored, ok, rerr := c.resolver.ResolveCollectionAdapter(ctx, method, req.Instrument.PSPID)
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
		// Admission freezes under the same customer/method order as deletion.
		// Recheck immediately before first send; after this transaction the
		// unresolved accepted intent pins the method against supported removal
		// and custody remap. No provider request runs while these locks are held.
		if err := c.checkInstrumentForSubmit(ctx, req, rail); err != nil {
			return ChargeResult{}, err
		}
		res, err := inner.Submit(ctx)
		if err != nil {
			return ChargeResult{}, err
		}
		if strings.TrimSpace(res.Rail) == "" {
			res.Rail = rail
		}
		return res, nil
	}), nil
}

func (c *ScopedCharger) checkInstrumentForSubmit(ctx context.Context, req ChargeRequest, rail string) error {
	return c.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: req.MerchantID, ID: req.Payer.UUID()}); err != nil {
			return err
		}
		method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: req.MerchantID, ID: req.PaymentMethodID})
		if errors.Is(err, pgx.ErrNoRows) {
			return charge.ErrInstrumentChanged
		}
		if err != nil {
			return err
		}
		if method.CustomerID != req.Payer.UUID() || !paymentmethods.Chargeable(method) || normalizeRail(method.Rail) != rail {
			return charge.ErrInstrumentChanged
		}
		if err := req.Instrument.Matches(method); err != nil {
			return err
		}
		if err := mandates.Recheck(ctx, q, req.MerchantID, req.Instrument.Mandate); err != nil {
			return errors.Join(charge.ErrInstrumentChanged, err)
		}
		if method.Custodian == models.CustodianHyperSwitch {
			if req.HyperSwitch == nil {
				return charge.ErrInstrumentChanged
			}
			binding, err := charge.FreezeHyperSwitchBinding(ctx, q, charge.CustodyOf(c.db), method, req.Instrument.PSPID, req.HyperSwitch.APIBaseURL)
			if err != nil {
				return err
			}
			if binding != *req.HyperSwitch {
				return charge.ErrInstrumentChanged
			}
		}
		return nil
	})
}

func normalizeRail(rail string) string {
	return strings.ToLower(strings.TrimSpace(rail))
}
