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

// NMICollectionAdapter collects invoices and top-ups from NMI customer
// vault payment methods through the #297 charge seam: every collection is a
// merchant-initiated unscheduled credential-on-file charge carrying the
// instrument's stored-credential replay reference.
type NMICollectionAdapter struct {
	Charger *nmidirect.Charger
}

func NewNMICollectionAdapter(client *nmi.NMIClient) *NMICollectionAdapter {
	return &NMICollectionAdapter{Charger: nmidirect.New(client)}
}

func NewNMICollectionAdapters(clients map[string]*nmi.NMIClient) map[string]CollectionAdapter {
	adapters := make(map[string]CollectionAdapter, len(clients))
	for rail, client := range clients {
		rail = normalizeRail(rail)
		if rail == "" || client == nil {
			continue
		}
		adapters[rail] = NewNMICollectionAdapter(client)
	}
	return adapters
}

func (a *NMICollectionAdapter) ChargeSavedMethod(ctx context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (ChargeResult, error) {
	if a == nil || a.Charger == nil {
		return ChargeResult{}, fmt.Errorf("nmi collection adapter not initialized")
	}
	if strings.TrimSpace(method.RailCustomerRef) == "" {
		return ChargeResult{}, fmt.Errorf("nmi payment method missing customer vault id")
	}
	if req.AmountCents <= 0 {
		return ChargeResult{}, fmt.Errorf("amount_cents must be positive")
	}
	rail := normalizeRail(method.Rail)
	// or#864: NO default. This is the last statement before a card is charged;
	// a guessed currency here mints a real charge in a currency nobody
	// established. Registry-validated (CUR-8), not merely non-blank.
	currency := normalizeCurrency(req.Currency)
	if err := moneyutil.ValidateCurrency(currency); err != nil {
		return ChargeResult{}, fmt.Errorf("nmi collection: refusing to charge without an established currency: %w", err)
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "OpenRails invoice collection"
	}

	// NMI requires every subsequent CIT/MIT to reference the approved initial
	// transaction. Never send a reference-less off-session charge: the customer
	// must complete a customer-present transaction to establish this agreement
	// sequence first.
	priorRef := strings.TrimSpace(method.StoredCredentialUnscheduledRef)
	if priorRef == "" {
		return ChargeResult{}, fmt.Errorf(
			"nmi collection: payment method %s has no unscheduled stored-credential anchor; customer-initiated re-enrollment is required",
			method.ID,
		)
	}

	res, err := a.Charger.Charge(ctx, charge.Request{
		Instrument: charge.Instrument{
			PaymentMethodID: method.ID,
			Rail:            rail,
			CustomerRef:     strings.TrimSpace(method.RailCustomerRef),
			MethodRef:       strings.TrimSpace(method.RailMethodRef),
		},
		AmountMinor: req.AmountCents,
		Currency:    currency,
		Description: description,
		OrderRef:    nmiWireOrderRef(req.IdempotencyKey),
		Context:     charge.UnscheduledMIT(priorRef),
	})
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
}

// nmiWireOrderRef compacts the invoice idempotency key into NMI's 50-char
// order-id budget ("invoice:<uuid>:attempt:N" is 54+). Deterministic and
// readable; the LOCAL idempotency identity is untouched.
func nmiWireOrderRef(idempotencyKey string) string {
	return strings.NewReplacer("invoice:", "inv:", ":attempt:", ":a").Replace(strings.TrimSpace(idempotencyKey))
}
