package nmi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// VerifyStoredCredential verifies a vaulted card as the storing customer-
// initiated transaction of an agreement: Direct Post type=validate (no funds
// move) with initiated_by=customer and stored_credential_indicator=stored, and
// billing_method=recurring for a recurring agreement. NMI's transactionid is
// the agreement's initial_transaction_id for later charges. A decline is a
// *CustomerVaultError; anything else unproven is ambiguous.
func (c *NMIClient) VerifyStoredCredential(ctx context.Context, vaultID, billingID, orderID string, recurring bool) (string, error) {
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
	sc := &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: IndicatorStored, Recurring: recurring}
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
