package nmi

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

type SaleParams struct {
	CustomerVaultID  string
	Amount           int64
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
	Amount        int64
}

type RefundResponse struct {
	TransactionID string
	ResponseText  string
}

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

	amountStr := moneyutil.FormatCentsDecimal(params.Amount)
	currency := params.Currency
	if currency == "" {
		currency = "usd"
	}
	orderDesc := params.OrderDescription
	if orderDesc == "" {
		orderDesc = "One-time purchase"
	}

	values := url.Values{
		"type":              {"sale"},
		"security_key":      {c.SecurityKey},
		"customer_vault_id": {params.CustomerVaultID},
		"amount":            {amountStr},
		"currency":          {currency},
		"order_description": {orderDesc},
	}
	if params.OrderID != "" {
		values.Set("orderid", params.OrderID)
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
		return nil, fmt.Errorf("sale failed: %s", responseText(output, response))
	}

	return &SaleResponse{
		TransactionID: output.Get("transactionid"),
		Authcode:      output.Get("authcode"),
		ResponseText:  responseText(output, response),
	}, nil
}

func (c *NMIClient) Refund(params RefundParams) (*RefundResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.TransactionID) == "" {
		return nil, errors.New("transaction ID is required")
	}

	values := url.Values{
		"type":          {"refund"},
		"security_key":  {c.SecurityKey},
		"transactionid": {params.TransactionID},
	}
	if params.Amount > 0 {
		values.Set("amount", moneyutil.FormatCentsDecimal(params.Amount))
	}

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return nil, err
	}

	output, err := parseDirectResponse(response)
	if err != nil {
		return nil, fmt.Errorf("failed to parse refund response: %w", err)
	}
	if !isDirectResponseApproved(output) {
		responseCode := parseMobiusResponseCode(output)
		return nil, &CustomerVaultError{
			Message:        fmt.Sprintf("refund failed: %s", responseText(output, response)),
			ResponseCode:   responseCode,
			LocalizationID: mobiusLocalizationID(responseCode),
			Detail:         mobiusResponseDetail(responseCode),
			RawResponse:    response,
		}
	}

	return &RefundResponse{
		TransactionID: output.Get("transactionid"),
		ResponseText:  responseText(output, response),
	}, nil
}

func (c *NMIClient) Void(transactionID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(transactionID) == "" {
		return errors.New("transaction ID is required")
	}

	values := url.Values{
		"type":          {"void"},
		"security_key":  {c.SecurityKey},
		"transactionid": {transactionID},
	}

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return err
	}

	output, err := parseDirectResponse(response)
	if err != nil {
		return fmt.Errorf("failed to parse void response: %w", err)
	}
	if !isDirectResponseApproved(output) {
		return fmt.Errorf("void failed: %s", responseText(output, response))
	}

	return nil
}
