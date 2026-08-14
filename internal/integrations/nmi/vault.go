package nmi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type CreateCustomerVaultData struct {
	PaymentToken string
	FirstName    string
	LastName     string
	Address1     string
	City         string
	State        string
	Zip          string
	Country      string
	Phone        string
	Email        string
	Company      string
	Address2     string
}

type UpdateCustomerVaultData struct {
	CustomerVaultID string
	// BillingID targets the exact stored instrument inside a multi-entry
	// customer vault. Empty falls back to resolving the priority-1 entry.
	BillingID string
	CreateCustomerVaultData
}

type DeleteCustomerVaultData struct {
	CustomerVaultID string
}

type CreateCustomerVaultResponse struct {
	CustomerVaultID string
	// BillingID is the created billing entry's id (the instrument-scope handle
	// inside the vault). Recorded verbatim (#682: safe to capture now that the
	// rebill-driver mode has its own column and no longer keys off this).
	BillingID string
}

func (d *CreateCustomerVaultData) v5Billing(requireToken bool) (*v5CustomerBillingRequest, error) {
	billing := &v5CustomerBillingRequest{
		// The live v5 create-customer REQUIRES a billing currency (verified
		// 2026-07-01; the docs omit it). The classic API had no such field —
		// the gateway defaulted to the account currency. USD mirrors the
		// existing money-path default (money.DefaultCurrency) for the one
		// account class we run NMI on.
		Currency:  "USD",
		FirstName: d.FirstName,
		LastName:  d.LastName,
		Company:   d.Company,
		Address1:  d.Address1,
		Address2:  d.Address2,
		City:      d.City,
		State:     d.State,
		Zip:       d.Zip,
		Country:   d.Country,
		Phone:     d.Phone,
		Email:     d.Email,
	}
	token := strings.TrimSpace(d.PaymentToken)
	if token == "" && requireToken {
		return nil, errors.New("payment token is required")
	}
	if token != "" {
		billing.PaymentDetails = &v5PaymentDetails{PaymentToken: token}
	}
	return billing, nil
}

// CreateCustomerVault stores a Collect.js / Payment Component token as a new
// vault customer via POST /v5/customers.
func (c *NMIClient) CreateCustomerVault(ctx context.Context, data CreateCustomerVaultData) (*CreateCustomerVaultResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	billing, err := data.v5Billing(true)
	if err != nil {
		return nil, err
	}

	var customer V5Customer
	if err := c.sendV5Request(ctx, http.MethodPost, "/customers", map[string]any{"billing": billing}, &customer); err != nil {
		return nil, err
	}
	if strings.TrimSpace(customer.ID) == "" {
		return nil, fmt.Errorf("failed to create customer vault: response carried no customer id")
	}
	resp := &CreateCustomerVaultResponse{CustomerVaultID: customer.ID}
	if billing := customer.PrimaryBilling(); billing != nil {
		resp.BillingID = strings.TrimSpace(billing.ID)
	}
	return resp, nil
}

// UpdateCustomerVault updates the primary billing record (payment token
// and/or address fields) via PATCH /v5/customers/{id}. The live gateway
// REQUIRES billing[].id (verified 2026-07-01 — the documented
// omit-for-priority-1 behavior 400s). Callers that already know the exact
// billing entry pass BillingID; otherwise the priority-1 id is resolved with
// a read first.
func (c *NMIClient) UpdateCustomerVault(ctx context.Context, data UpdateCustomerVaultData) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	vaultID := strings.TrimSpace(data.CustomerVaultID)
	if vaultID == "" {
		return errors.New("customer vault ID is required")
	}
	billing, err := data.v5Billing(false)
	if err != nil {
		return err
	}

	billingID := strings.TrimSpace(data.BillingID)
	if billingID == "" {
		page, err := c.ListCustomersPage(ctx, "", 1, vaultID)
		if err != nil {
			return fmt.Errorf("failed to update customer vault: lookup: %w", err)
		}
		var primary *V5CustomerBilling
		for i := range page.Customers {
			if strings.TrimSpace(page.Customers[i].ID) == vaultID {
				primary = page.Customers[i].PrimaryBilling()
				break
			}
		}
		if primary == nil {
			return fmt.Errorf("failed to update customer vault: customer %s has no billing record at NMI", vaultID)
		}
		billingID = strings.TrimSpace(primary.ID)
	}
	if billingID == "" {
		return fmt.Errorf("failed to update customer vault: customer %s billing record has no id", vaultID)
	}
	billing.ID = billingID

	body := map[string]any{"billing": []*v5CustomerBillingRequest{billing}}
	if err := c.sendV5Request(ctx, http.MethodPatch, "/customers/"+url.PathEscape(vaultID), body, nil); err != nil {
		return fmt.Errorf("failed to update customer vault: %w", err)
	}
	return nil
}

// DeleteCustomerBillingEntry removes ONE stored card from a multi-entry vault
// via DELETE /v5/customers/{vault}/billing/{billing_id} (live-verified
// 2026-07-02; the DOCUMENTED /billing-addresses/{id} path answers
// E_ROUTE_NOT_FOUND). NMI refuses to empty a vault ("Customer Vault must have
// at least one billing", HTTP 400) — deleting the LAST entry means deleting
// the whole vault customer instead (DeleteCustomerVault).
func (c *NMIClient) DeleteCustomerBillingEntry(ctx context.Context, vaultID, billingID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	vaultID = strings.TrimSpace(vaultID)
	billingID = strings.TrimSpace(billingID)
	if vaultID == "" || billingID == "" {
		return errors.New("customer vault ID and billing ID are required")
	}
	if err := c.sendV5Request(ctx, http.MethodDelete, "/customers/"+url.PathEscape(vaultID)+"/billing/"+url.PathEscape(billingID), nil, nil); err != nil {
		return fmt.Errorf("failed to delete vault billing entry: %w", err)
	}
	return nil
}

// DeleteCustomerVault removes a vault customer via DELETE /v5/customers/{id}.
func (c *NMIClient) DeleteCustomerVault(ctx context.Context, data DeleteCustomerVaultData) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	vaultID := strings.TrimSpace(data.CustomerVaultID)
	if vaultID == "" {
		return errors.New("customer vault ID is required")
	}
	if err := c.sendV5Request(ctx, http.MethodDelete, "/customers/"+url.PathEscape(vaultID), nil, nil); err != nil {
		return fmt.Errorf("failed to delete customer vault: %w", err)
	}
	return nil
}
