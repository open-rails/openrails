package nmi

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// PrepareRecurringSale verifies the native engine's supported single-card vault
// without moving money. Unlike legacy sales, the engine never selects a default
// billing entry. This read proves the account/instrument binding, not consent.
func (c *NMIClient) PrepareRecurringSale(ctx context.Context, vaultID, billingID string) error {
	if c == nil || c.accountMerchantID == uuid.Nil || c.accountPSPID == uuid.Nil || c.accountSecurityKey == "" || c.SecurityKey != c.accountSecurityKey {
		return errors.New("native recurring sale requires its immutable account credentials")
	}
	if c.ReadOnly {
		return ErrProviderReadOnly
	}
	if vaultID == "" || billingID == "" || strings.TrimSpace(vaultID) != vaultID || strings.TrimSpace(billingID) != billingID {
		return errors.New("native recurring sale requires exact vault and billing identity")
	}
	_, err := c.ReadSingleCardVaultBilling(ctx, vaultID, billingID)
	return err
}

// ReadRecurringSaleEvidence qualifies the existing order/exact-transaction sale
// proof and the vault's sole billing entry. NMI's documented transaction receipt
// does not echo COF fields: those remain the accepted, transmitted request, not
// manufactured provider facts. The account must be qualified for that contract.
// Missing/ambiguous readback never authorizes another sale.
func (c *NMIClient) ReadRecurringSaleEvidence(ctx context.Context, orderReference, reference, vaultID, billingID string) (SaleEvidence, bool, error) {
	if c == nil || c.accountMerchantID == uuid.Nil || c.accountPSPID == uuid.Nil || c.accountSecurityKey == "" {
		return SaleEvidence{}, false, errors.New("recurring receipt requires an account-scoped reader")
	}
	if vaultID == "" || billingID == "" || strings.TrimSpace(vaultID) != vaultID || strings.TrimSpace(billingID) != billingID {
		return SaleEvidence{}, false, errors.New("recurring receipt requires exact vault and billing identity")
	}
	scoped := *c
	scoped.SecurityKey = c.accountSecurityKey
	facts, found, err := scoped.ReadSaleEvidence(ctx, orderReference, reference)
	if err != nil || !found {
		return facts, found, err
	}
	if facts.CustomerVaultID != vaultID {
		return SaleEvidence{}, false, receiptMismatch("recurring sale is not on the accepted customer vault")
	}
	facts.VaultBillingID, err = scoped.ReadSingleCardVaultBilling(ctx, vaultID, billingID)
	if err != nil {
		return SaleEvidence{}, false, err
	}
	return facts, true, nil
}
