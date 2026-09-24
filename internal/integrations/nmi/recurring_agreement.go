package nmi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// EstablishRecurringAgreement verifies a newly vaulted card as the initial
// customer-initiated transaction of a recurring credential-on-file agreement:
// Direct Post type=validate (no funds move) with billing_method=recurring,
// initiated_by=customer and stored_credential_indicator=stored. NMI's
// transactionid is the agreement's initial_transaction_id for later MITs.
// A decline is a *CustomerVaultError; anything else unproven is ambiguous.
func (c *NMIClient) EstablishRecurringAgreement(ctx context.Context, vaultID, billingID, orderID string) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}
	vaultID, billingID = strings.TrimSpace(vaultID), strings.TrimSpace(billingID)
	if vaultID == "" {
		return "", errors.New("customer vault ID is required")
	}
	if len(orderID) > 50 {
		return "", fmt.Errorf("order id %q exceeds NMI's 50-character limit", orderID)
	}
	sc := &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: IndicatorStored, Recurring: true}
	values := url.Values{
		"type":              {"validate"},
		"security_key":      {c.SecurityKey},
		"customer_vault_id": {vaultID},
	}
	if billingID != "" {
		values.Set("billing_id", billingID)
	}
	if orderID != "" {
		values.Set("orderid", orderID)
	}
	sc.ApplyToForm(values)
	response, err := c.sendDirectRequest(ctx, values)
	if err != nil {
		return "", err
	}
	output, err := parseDirectResponse(response)
	if err != nil {
		return "", err
	}
	if !isDirectResponseApproved(output) {
		return "", newSaleError(response, output)
	}
	id := strings.TrimSpace(output.Get("transactionid"))
	if id == "" {
		return "", ambiguous(errors.New("NMI approved the card verification without a transaction id"))
	}
	return id, nil
}
