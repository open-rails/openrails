package nmi

import (
	"errors"
	"fmt"
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

func (c *NMIClient) CreateCustomerVault(data CreateCustomerVaultData) (*CreateCustomerVaultResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}

	values := url.Values{
		"customer_vault": {"add_customer"},
		"security_key":   {c.SecurityKey},
	}
	values.Set("payment_token", strings.TrimSpace(data.PaymentToken))

	if data.FirstName != "" {
		values.Set("first_name", data.FirstName)
	}
	if data.LastName != "" {
		values.Set("last_name", data.LastName)
	}
	if data.Address1 != "" {
		values.Set("address1", data.Address1)
	}
	if data.City != "" {
		values.Set("city", data.City)
	}
	if data.State != "" {
		values.Set("state", data.State)
	}
	if data.Zip != "" {
		values.Set("zip", data.Zip)
	}
	if data.Country != "" {
		values.Set("country", data.Country)
	}
	if data.Phone != "" {
		values.Set("phone", data.Phone)
	}
	if data.Email != "" {
		values.Set("email", data.Email)
	}

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return nil, err
	}

	output, err := parseDirectResponse(response)
	if err != nil {
		return nil, err
	}
	if !isDirectResponseApproved(output) {
		return nil, newCustomerVaultError(response, output)
	}

	vaultID := output.Get("customer_vault_id")
	if vaultID == "" {
		return nil, fmt.Errorf("failed to create customer vault: %s", responseText(output, response))
	}

	return &CreateCustomerVaultResponse{CustomerVaultID: vaultID}, nil
}

func (c *NMIClient) UpdateCustomerVault(data UpdateCustomerVaultData) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(data.CustomerVaultID) == "" {
		return errors.New("customer vault ID is required")
	}

	values := url.Values{
		"customer_vault":    {"update_customer"},
		"security_key":      {c.SecurityKey},
		"customer_vault_id": {data.CustomerVaultID},
	}

	if strings.TrimSpace(data.PaymentToken) != "" {
		values.Set("payment_token", strings.TrimSpace(data.PaymentToken))
	}
	if data.FirstName != "" {
		values.Set("first_name", data.FirstName)
	}
	if data.LastName != "" {
		values.Set("last_name", data.LastName)
	}
	if data.Address1 != "" {
		values.Set("address1", data.Address1)
	}
	if data.City != "" {
		values.Set("city", data.City)
	}
	if data.State != "" {
		values.Set("state", data.State)
	}
	if data.Zip != "" {
		values.Set("zip", data.Zip)
	}
	if data.Country != "" {
		values.Set("country", data.Country)
	}
	if data.Phone != "" {
		values.Set("phone", data.Phone)
	}
	if data.Email != "" {
		values.Set("email", data.Email)
	}
	if data.Company != "" {
		values.Set("company", data.Company)
	}
	if data.Address2 != "" {
		values.Set("address2", data.Address2)
	}

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return err
	}

	output, err := parseDirectResponse(response)
	if err != nil {
		return err
	}
	if !isDirectResponseApproved(output) {
		return fmt.Errorf("failed to update customer vault: %s", responseText(output, response))
	}

	return nil
}

func (c *NMIClient) DeleteCustomerVault(data DeleteCustomerVaultData) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(data.CustomerVaultID) == "" {
		return errors.New("customer vault ID is required")
	}

	values := url.Values{
		"customer_vault":    {"delete_customer"},
		"security_key":      {c.SecurityKey},
		"customer_vault_id": {data.CustomerVaultID},
	}

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return err
	}

	output, err := parseDirectResponse(response)
	if err != nil {
		return err
	}
	if !isDirectResponseApproved(output) {
		return fmt.Errorf("failed to delete customer vault: %s", responseText(output, response))
	}

	return nil
}
