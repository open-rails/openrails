package nmi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// AddCustomerBillingEntry stores a Collect.js token as a further billing
// entry in an existing vault (POST /v5/customers/{id}/billing) and returns
// the new entry's id. known lists the entries present before the request, so
// a response carrying the whole customer resolves the added entry.
func (c *NMIClient) AddCustomerBillingEntry(ctx context.Context, vaultID string, data CreateCustomerVaultData, known []string) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}
	vaultID = strings.TrimSpace(vaultID)
	if vaultID == "" {
		return "", errors.New("customer vault ID is required")
	}
	billing, err := data.v5Billing(true)
	if err != nil {
		return "", err
	}
	var out struct {
		Object  string              `json:"object"`
		ID      string              `json:"id"`
		Billing []V5CustomerBilling `json:"billing"`
	}
	if err := c.sendV5Request(ctx, http.MethodPost, "/customers/"+url.PathEscape(vaultID)+"/billing", billing, &out); err != nil {
		return "", fmt.Errorf("failed to add billing entry: %w", err)
	}
	if len(out.Billing) == 0 && strings.TrimSpace(out.ID) != "" && out.Object != "customer" {
		return strings.TrimSpace(out.ID), nil
	}
	seen := map[string]bool{}
	for _, id := range known {
		seen[strings.TrimSpace(id)] = true
	}
	var added []string
	for _, b := range out.Billing {
		if id := strings.TrimSpace(b.ID); id != "" && !seen[id] {
			added = append(added, id)
		}
	}
	if len(added) != 1 {
		return "", ambiguous(fmt.Errorf("NMI added a billing entry to vault %s without naming it", vaultID))
	}
	return added[0], nil
}

// Verification is a card verification (type=validate) found by its order
// reference.
type Verification struct {
	TransactionID string
	Approved      bool
	ResponseCode  int
	ResponseText  string
}

// ReadVerificationByOrderID reads the card verification submitted under
// orderID from the Query API: found=false means NMI has none.
func (c *NMIClient) ReadVerificationByOrderID(ctx context.Context, orderID string) (Verification, bool, error) {
	var v Verification
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return v, false, errors.New("order id is required")
	}
	raw, err := c.SearchTransactions(ctx, QueryFilter{OrderID: orderID})
	if err != nil {
		return v, false, err
	}
	var parsed saleQueryResponse
	if err := xml.Unmarshal([]byte(raw), &parsed); err != nil {
		return v, false, fmt.Errorf("parse transaction query response: %w", err)
	}
	if msg := strings.TrimSpace(parsed.ErrorResponse); msg != "" {
		return v, false, fmt.Errorf("transaction query error_response: %s", msg)
	}
	found := false
	for _, txn := range parsed.Transactions {
		if strings.TrimSpace(txn.OrderID) != orderID {
			continue
		}
		for _, action := range txn.Actions {
			if !strings.EqualFold(strings.TrimSpace(action.ActionType), "validate") {
				continue
			}
			approved := strings.TrimSpace(action.Success) == "1"
			if found && !approved {
				continue // an approval decides the order reference
			}
			found = true
			code, _ := strconv.Atoi(strings.TrimSpace(action.ResponseCode))
			v = Verification{TransactionID: strings.TrimSpace(txn.TransactionID), Approved: approved, ResponseCode: code, ResponseText: strings.TrimSpace(action.ResponseText)}
		}
	}
	return v, found, nil
}
