package nmi

import (
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
	CreateCustomerVaultData
}

type DeleteCustomerVaultData struct {
	CustomerVaultID string
}

type CreateCustomerVaultResponse struct {
	CustomerVaultID string
}

func (d *CreateCustomerVaultData) v5Billing(requireToken bool) (*v5CustomerBillingRequest, error) {
	billing := &v5CustomerBillingRequest{
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
func (c *NMIClient) CreateCustomerVault(data CreateCustomerVaultData) (*CreateCustomerVaultResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	billing, err := data.v5Billing(true)
	if err != nil {
		return nil, err
	}

	var customer V5Customer
	if err := c.sendV5Request(http.MethodPost, "/customers", map[string]any{"billing": billing}, &customer); err != nil {
		return nil, err
	}
	if strings.TrimSpace(customer.ID) == "" {
		return nil, fmt.Errorf("failed to create customer vault: response carried no customer id")
	}
	return &CreateCustomerVaultResponse{CustomerVaultID: customer.ID}, nil
}

// UpdateCustomerVault updates the priority-1 billing record (payment token
// and/or address fields) via PATCH /v5/customers/{id}.
func (c *NMIClient) UpdateCustomerVault(data UpdateCustomerVaultData) error {
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

	// billing is an array on update; omitting billing_id targets priority 1.
	body := map[string]any{"billing": []*v5CustomerBillingRequest{billing}}
	if err := c.sendV5Request(http.MethodPatch, "/customers/"+url.PathEscape(vaultID), body, nil); err != nil {
		return fmt.Errorf("failed to update customer vault: %w", err)
	}
	return nil
}

// DeleteCustomerVault removes a vault customer via DELETE /v5/customers/{id}.
func (c *NMIClient) DeleteCustomerVault(data DeleteCustomerVaultData) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	vaultID := strings.TrimSpace(data.CustomerVaultID)
	if vaultID == "" {
		return errors.New("customer vault ID is required")
	}
	if err := c.sendV5Request(http.MethodDelete, "/customers/"+url.PathEscape(vaultID), nil, nil); err != nil {
		return fmt.Errorf("failed to delete customer vault: %w", err)
	}
	return nil
}
