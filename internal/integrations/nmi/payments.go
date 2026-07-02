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
	// Amount is CENTS (typed, #671) — rendered as a two-decimal wire amount.
	Amount           moneyutil.Cents
	Currency         string
	OrderDescription string
	OrderID          string
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
