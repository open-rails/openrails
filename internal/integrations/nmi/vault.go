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
	// CardExp, MMYY, sets the stored card's expiry without a new token.
	CardExp string
	CreateCustomerVaultData
}

type DeleteCustomerVaultData struct {
	CustomerVaultID string
}

type CreateCustomerVaultResponse struct {
	CustomerVaultID string
	// BillingID is the created billing entry's id, recorded verbatim.
	BillingID string
	// Card is the gateway's masked display data for the stored card.
	Card V5BillingCardData
}

func (d *CreateCustomerVaultData) v5Billing(requireToken bool) (*v5CustomerBillingRequest, error) {
	billing := &v5CustomerBillingRequest{
		// The live v5 create-customer requires a billing currency (the docs
		// omit it). USD matches money.DefaultCurrency for the one account
		// class OpenRails runs on NMI.
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
		resp.Card = billing.PaymentDetails
	}
	return resp, nil
}

// UpdateCustomerVault updates the primary billing record (payment token
// and/or address) via PATCH /v5/customers/{id}. The live gateway requires
// billing[].id (the documented omit-for-priority-1 400s), so without
// BillingID the priority-1 id is read first.
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
		customer, found, err := c.GetCustomer(ctx, vaultID)
		if err != nil {
			return fmt.Errorf("failed to update customer vault: lookup: %w", err)
		}
		var primary *V5CustomerBilling
		if found {
			primary = customer.PrimaryBilling()
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
	if exp := strings.TrimSpace(data.CardExp); exp != "" {
		if billing.PaymentDetails == nil {
			billing.PaymentDetails = &v5PaymentDetails{}
		}
		billing.PaymentDetails.CardExp = exp
	}

	body := map[string]any{"billing": []*v5CustomerBillingRequest{billing}}
	if err := c.sendV5Request(ctx, http.MethodPatch, "/customers/"+url.PathEscape(vaultID), body, nil); err != nil {
		return fmt.Errorf("failed to update customer vault: %w", err)
	}
	return nil
}

// DeleteCustomerBillingEntry removes one stored card from a multi-entry vault
// via DELETE /v5/customers/{vault}/billing/{billing_id} (live-verified; the
// documented /billing-addresses/{id} answers E_ROUTE_NOT_FOUND). NMI refuses
// to empty a vault (HTTP 400), so the last entry goes with DeleteCustomerVault.
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
