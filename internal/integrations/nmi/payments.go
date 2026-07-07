package nmi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

type SaleParams struct {
	CustomerVaultID string
	// BillingID targets ONE stored card inside the vault (#682 shared-vault
	// support). Empty = the vault's priority-1 entry — always correct under the
	// one-vault-per-card minting policy; set it (from the payment method's
	// rail_method_ref) when the vault may hold multiple entries.
	BillingID string
	// Amount is CENTS (typed, #671) — rendered as a two-decimal wire amount.
	Amount           moneyutil.Cents
	Currency         string
	OrderDescription string
	OrderID          string
	// StoredCredential carries the CIT/MIT credential-on-file fields (#297).
	// Non-nil routes the sale through classic Direct Post — the lane the NMI
	// portal documents the fields on. nil preserves the legacy shape.
	StoredCredential *StoredCredential
}

type SaleResponse struct {
	TransactionID string
	Authcode      string
	ResponseText  string
}

type RefundParams struct {
	TransactionID string
	// Amount is CENTS (typed, #671); 0 = full refund.
	Amount moneyutil.Cents
}

type RefundResponse struct {
	TransactionID string
	ResponseText  string
}

// RunSale charges a vaulted customer via POST /v5/payments/sale using the
// top-level customer_vault:{id} object (the live-verified vault-sale form;
// the documented payment_details.customer_vault_id is rejected).
//
// BILLING-TARGETED sales go through classic Direct Post DELIBERATELY: the live
// v5 sale rejects every billing-selection shape (customer_vault.billing_id,
// top-level billing_id, payment_details.* — all "extra parameters",
// live-verified 2026-07-02) and always charges the priority-1 entry, while
// classic sale + billing_id charges the exact card (same lane the dunning
// rebill already uses).
func (c *NMIClient) RunSale(params SaleParams) (*SaleResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.CustomerVaultID) == "" {
		return nil, errors.New("customer vault ID is required")
	}
	if params.Amount <= 0 {
		return nil, errors.New("amount must be greater than 0")
	}
	currency := strings.TrimSpace(params.Currency)
	if currency == "" {
		// #651: a money path must not silently default the currency.
		return nil, errors.New("currency is required")
	}
	orderDesc := params.OrderDescription
	if orderDesc == "" {
		orderDesc = "One-time purchase"
	}
	// v5 caps order_details.id at 50 chars (live-verified). The order id is the
	// correlation handle evidence probes search by — refuse loudly rather than
	// silently truncate it into an unfindable reference.
	if len(params.OrderID) > 50 {
		return nil, fmt.Errorf("order id %q exceeds NMI's 50-character limit", params.OrderID)
	}
	if err := params.StoredCredential.validate(); err != nil {
		return nil, err
	}

	// Stored-credential (CIT/MIT) sales go classic DELIBERATELY (#297): the
	// integration portal documents initiated_by / stored_credential_indicator /
	// initial_transaction_id on the Direct Post variables (v5 nests them under
	// cit_mit, but the classic vault-sale lane is the one already live-verified
	// here — billing-targeted sales and dunning rebills ride it today).
	if billingID := strings.TrimSpace(params.BillingID); billingID != "" || params.StoredCredential != nil {
		return c.runClassicSale(params, currency, orderDesc, billingID)
	}

	req := v5PaymentRequest{
		Amount:        centsJSONAmount(params.Amount),
		Currency:      currency,
		CustomerVault: &v5CustomerVaultRef{ID: params.CustomerVaultID},
		OrderDetails:  &v5OrderDetails{ID: params.OrderID, OrderDescription: orderDesc},
	}

	var txn v5Transaction
	if err := c.sendV5Request(http.MethodPost, "/payments/sale", req, &txn); err != nil {
		return nil, err
	}
	if !txn.approved() {
		return nil, newV5TransactionError("sale failed", &txn)
	}

	return &SaleResponse{
		TransactionID: txn.ID,
		Authcode:      txn.AuthCode,
		ResponseText:  txn.ResponseText,
	}, nil
}

// runClassicSale charges a vault via classic Direct Post (type=sale +
// customer_vault_id): billingID targets ONE specific billing entry (see the
// RunSale doc for why v5 cannot do this); "" charges the priority-1 entry.
// Stored-credential fields ride this lane (#297).
func (c *NMIClient) runClassicSale(params SaleParams, currency, orderDesc, billingID string) (*SaleResponse, error) {
	values := url.Values{
		"type":              {"sale"},
		"security_key":      {c.SecurityKey},
		"customer_vault_id": {params.CustomerVaultID},
		"amount":            {string(centsJSONAmount(params.Amount))},
		"currency":          {currency},
		"order_description": {orderDesc},
	}
	if billingID != "" {
		values.Set("billing_id", billingID)
	}
	if params.OrderID != "" {
		values.Set("orderid", params.OrderID)
	}
	params.StoredCredential.applyToForm(values)

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return nil, err
	}
	output, err := parseDirectResponse(response)
	if err != nil {
		return nil, err
	}
	if !isDirectResponseApproved(output) {
		return nil, newSaleError(response, output)
	}
	return &SaleResponse{
		TransactionID: output.Get("transactionid"),
		Authcode:      output.Get("authcode"),
		ResponseText:  responseText(output, response),
	}, nil
}

// Refund reverses a settled transaction via POST /v5/payments/{id}/refund.
// Amount 0 refunds the full settled amount (both classic and v5 semantics).
func (c *NMIClient) Refund(params RefundParams) (*RefundResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	txnID := strings.TrimSpace(params.TransactionID)
	if txnID == "" {
		return nil, errors.New("transaction ID is required")
	}

	body := map[string]any{}
	if params.Amount > 0 {
		body["amount"] = centsJSONAmount(params.Amount)
	}

	var txn v5Transaction
	if err := c.sendV5Request(http.MethodPost, "/payments/"+url.PathEscape(txnID)+"/refund", body, &txn); err != nil {
		return nil, err
	}
	if !txn.approved() {
		return nil, newV5TransactionError("refund failed", &txn)
	}

	return &RefundResponse{
		TransactionID: txn.ID,
		ResponseText:  txn.ResponseText,
	}, nil
}

// Void cancels an unsettled transaction via POST /v5/payments/{id}/void.
func (c *NMIClient) Void(transactionID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	txnID := strings.TrimSpace(transactionID)
	if txnID == "" {
		return errors.New("transaction ID is required")
	}

	var txn v5Transaction
	if err := c.sendV5Request(http.MethodPost, "/payments/"+url.PathEscape(txnID)+"/void", map[string]any{}, &txn); err != nil {
		return err
	}
	if !txn.approved() {
		return fmt.Errorf("void failed: %s", strings.TrimSpace(txn.ResponseText))
	}
	return nil
}
