package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// NMICollectionAdapter collects invoices from NMI customer vault payment
// methods through the #297 charge seam: every collection is a
// merchant-initiated unscheduled credential-on-file charge carrying the
// instrument's stored-credential replay reference.
type NMICollectionAdapter struct {
	Charger *nmidirect.Charger
}

func NewNMICollectionAdapter(client *nmi.NMIClient) *NMICollectionAdapter {
	return &NMICollectionAdapter{Charger: nmidirect.New(client)}
}

func (a *NMICollectionAdapter) Prepare(_ context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (PreparedCharge, error) {
	// or#864: NO default. A guessed currency here mints a real charge in a
	// currency nobody established; the gate answers before anything else.
	currency := normalizeCurrency(req.Currency)
	if err := moneyutil.ValidateCurrency(currency); err != nil {
		return nil, fmt.Errorf("nmi collection: refusing to charge without an established currency: %w", err)
	}
	if a == nil || a.Charger == nil || a.Charger.Client == nil {
		return nil, fmt.Errorf("nmi collection adapter not initialized")
	}
	if a.Charger.Client.ReadOnly {
		return nil, fmt.Errorf("nmi client is read-only (mode=readonly)")
	}
	if strings.TrimSpace(method.RailCustomerRef) == "" {
		return nil, fmt.Errorf("nmi payment method missing customer vault id")
	}
	anchor := strings.TrimSpace(method.StoredCredentialUnscheduledRef)
	if anchor == "" {
		return nil, fmt.Errorf("nmi payment method missing approved unscheduled credential reference")
	}
	if req.AmountCents <= 0 {
		return nil, fmt.Errorf("amount_cents must be positive")
	}
	rail := normalizeRail(method.Rail)
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "OpenRails invoice collection"
	}
	request := charge.Request{
		Instrument: charge.Instrument{
			PaymentMethodID: method.ID,
			Rail:            rail,
			CustomerRef:     strings.TrimSpace(method.RailCustomerRef),
			MethodRef:       strings.TrimSpace(method.RailMethodRef),
		},
		AmountMinor: req.AmountCents,
		Currency:    currency,
		Description: description,
		OrderRef:    strings.TrimSpace(req.IdempotencyKey),
		Context:     charge.UnscheduledMIT(anchor),
	}
	return PreparedChargeFunc(func(ctx context.Context) (ChargeResult, error) {
		res, err := a.Charger.Charge(ctx, request)
		if err != nil {
			return ChargeResult{}, err
		}
		return ChargeResult{
			Rail:                        rail,
			TransactionID:               res.TransactionID,
			Declined:                    res.Declined,
			FailureCode:                 res.FailureCode,
			FailureMessage:              res.FailureMessage,
			CapturedStoredCredentialRef: res.CapturedRef,
		}, nil
	}), nil
}
