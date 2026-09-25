package nmi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
	// Amount is rail minor units, rendered in the explicitly supplied currency.
	Amount           moneyutil.Cents
	Currency         string
	OrderDescription string
	OrderID          string
	// StoredCredential carries the CIT/MIT credential-on-file fields (#297).
	// It is required and is sent through classic Direct Post, the lane on which
	// NMI documents initiated_by, stored_credential_indicator, and the sequence
	// reference.
	StoredCredential *StoredCredential
	// DupSeconds, when positive, sets NMI's duplicate-check window for this
	// request. A lost-submission resend covers the time since its fence, so a
	// late-indexed original is refused rather than charged twice.
	DupSeconds int
}

type SaleResponse struct {
	TransactionID string
	Authcode      string
	ResponseText  string
}

type RefundParams struct {
	TransactionID string
	// Amount is rail minor units in Currency; 0 requests a full refund.
	Amount   moneyutil.Cents
	Currency string
}

type RefundResponse struct {
	TransactionID string
	ResponseText  string
}

// RunSale charges a vaulted customer through classic Direct Post. All sales
// in this package use a stored credential, and this is the NMI lane whose wire
// contract exposes the required credential-on-file fields. It also supports
// billing_id so a shared vault can target one exact card.
func (c *NMIClient) RunSale(ctx context.Context, params SaleParams) (*SaleResponse, error) {
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
	// NMI caps orderid at 50 chars (live-verified). The order id is the
	// correlation handle evidence probes search by — refuse loudly rather than
	// silently truncate it into an unfindable reference.
	if len(params.OrderID) > 50 {
		return nil, fmt.Errorf("order id %q exceeds NMI's 50-character limit", params.OrderID)
	}
	if err := params.StoredCredential.Validate(); err != nil {
		return nil, err
	}

	return c.runClassicSale(ctx, params, currency, orderDesc, strings.TrimSpace(params.BillingID))
}

// runClassicSale charges a vault via classic Direct Post (type=sale +
// customer_vault_id): billingID targets ONE specific billing entry; ""
// charges the priority-1 entry. Stored-credential fields ride this lane (#297).
func (c *NMIClient) runClassicSale(ctx context.Context, params SaleParams, currency, orderDesc, billingID string) (*SaleResponse, error) {
	amount, err := WireAmount(params.Amount, currency)
	if err != nil {
		return nil, err
	}
	values := url.Values{
		"type":              {"sale"},
		"security_key":      {c.SecurityKey},
		"customer_vault_id": {params.CustomerVaultID},
		"amount":            {amount},
		"currency":          {currency},
		"order_description": {orderDesc},
	}
	if billingID != "" {
		values.Set("billing_id", billingID)
	}
	if params.OrderID != "" {
		values.Set("orderid", params.OrderID)
	}
	if params.DupSeconds > 0 {
		values.Set("dup_seconds", strconv.Itoa(params.DupSeconds))
	}
	params.StoredCredential.ApplyToForm(values)

	response, err := c.sendDirectRequest(ctx, values)
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
func (c *NMIClient) Refund(ctx context.Context, params RefundParams) (*RefundResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	txnID := strings.TrimSpace(params.TransactionID)
	if txnID == "" {
		return nil, errors.New("transaction ID is required")
	}
	if params.Amount < 0 {
		return nil, errors.New("refund amount cannot be negative")
	}
	if strings.TrimSpace(params.Currency) == "" {
		return nil, errors.New("refund currency is required")
	}
	amount, err := WireAmount(params.Amount, params.Currency)
	if err != nil {
		return nil, err
	}

	body := map[string]any{}
	if params.Amount > 0 {
		body["amount"] = json.RawMessage(amount)
	}

	var txn v5Transaction
	if err := c.sendV5Request(ctx, http.MethodPost, "/payments/"+url.PathEscape(txnID)+"/refund", body, &txn); err != nil {
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
func (c *NMIClient) Void(ctx context.Context, transactionID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	txnID := strings.TrimSpace(transactionID)
	if txnID == "" {
		return errors.New("transaction ID is required")
	}

	var txn v5Transaction
	if err := c.sendV5Request(ctx, http.MethodPost, "/payments/"+url.PathEscape(txnID)+"/void", map[string]any{}, &txn); err != nil {
		return err
	}
	if !txn.approved() {
		return fmt.Errorf("void failed: %s", strings.TrimSpace(txn.ResponseText))
	}
	return nil
}
