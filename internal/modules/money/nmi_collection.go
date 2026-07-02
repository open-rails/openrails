package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// NMICollectionAdapter collects invoices and top-ups from NMI customer
// vault payment methods.
type NMICollectionAdapter struct {
	Client *nmi.NMIClient
}

func NewNMICollectionAdapter(client *nmi.NMIClient) *NMICollectionAdapter {
	return &NMICollectionAdapter{Client: client}
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

func (a *NMICollectionAdapter) ChargeSavedMethod(_ context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (ChargeResult, error) {
	if a == nil || a.Client == nil {
		return ChargeResult{}, fmt.Errorf("nmi collection adapter not initialized")
	}
	vaultID := strings.TrimSpace(method.RailCustomerRef)
	if vaultID == "" {
		return ChargeResult{}, fmt.Errorf("nmi payment method missing customer vault id")
	}
	if req.AmountCents <= 0 {
		return ChargeResult{}, fmt.Errorf("amount_cents must be positive")
	}
	rail := normalizeRail(method.Rail)
	currency := normalizeCurrency(req.Currency)
	if currency == "" {
		currency = DefaultCurrency
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "OpenRails invoice collection"
	}

	sale, err := a.Client.RunSale(nmi.SaleParams{
		CustomerVaultID:  vaultID,
		Amount:           req.AmountCents,
		Currency:         currency,
		OrderDescription: description,
		OrderID:          nmiWireOrderRef(req.IdempotencyKey),
	})
	if err != nil {
		var vaultErr *nmi.CustomerVaultError
		if errors.As(err, &vaultErr) && nmiCollectionHardDecline(vaultErr.ResponseCode) {
			code := nmiFailureCode(vaultErr)
			message := vaultErr.Error()
			return ChargeResult{
				Rail:           rail,
				Declined:       true,
				FailureCode:    &code,
				FailureMessage: &message,
			}, nil
		}
		return ChargeResult{}, err
	}
	return ChargeResult{
		Rail:          rail,
		TransactionID: strings.TrimSpace(sale.TransactionID),
	}, nil
}

// nmiWireOrderRef compacts the invoice idempotency key into NMI's 50-char
// order-id budget ("invoice:<uuid>:attempt:N" is 54+). Deterministic and
// readable; the LOCAL idempotency identity is untouched.
func nmiWireOrderRef(idempotencyKey string) string {
	return strings.NewReplacer("invoice:", "inv:", ":attempt:", ":a").Replace(strings.TrimSpace(idempotencyKey))
}

func nmiFailureCode(err *nmi.CustomerVaultError) string {
	if err == nil {
		return "nmi_declined"
	}
	if code := strings.TrimSpace(err.LocalizationID); code != "" {
		return code
	}
	if err.ResponseCode != 0 {
		return fmt.Sprintf("nmi_response_%d", err.ResponseCode)
	}
	return "nmi_declined"
}

func nmiCollectionHardDecline(code int) bool {
	switch code {
	case 420, 421, 430:
		return false
	default:
		return true
	}
}
