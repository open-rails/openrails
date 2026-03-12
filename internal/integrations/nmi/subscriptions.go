package nmi

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type CardUserData struct {
	FirstName string
	LastName  string
	Address1  string
	City      string
	State     string
	Zip       string
	Country   string
}

type RecurringPaymentData struct {
	CardUserData
	PlanID          string
	CustomerVaultID string
	Email           string
	Currency        string
	PaymentToken    string
	Amount          float64
	OrderID         string
	PONumber        string
	CustomerID      string
	StartDate       string
}

type QueryFilter struct {
	StartDate   string
	EndDate     string
	Condition   string
	ActionType  string
	PageNumber  int
	ResultLimit int
}

type AddSubscriptionResponse struct {
	Type           string
	SubscriptionID string
	TransactionID  string
	Authcode       string
}

type ManualRebillParams struct {
	VaultID        string
	BillingID      string
	SubscriptionID string
}

type ManualRebillResponse struct {
	Success       bool
	TransactionID string
	ErrorMessage  string
}

type RecurringQueryParams struct {
	SubscriptionID string
	ResultLimit    int
	PageNumber     int
	ResultOrder    string
}

func (c *NMIClient) AddRecurringSubscription(data RecurringPaymentData) (*AddSubscriptionResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	if data.PlanID == "" {
		return nil, errors.New("PlanID is required")
	}
	if data.CustomerVaultID == "" && data.PaymentToken == "" {
		return nil, errors.New("either customer vault or payment token is required")
	}

	amtStr := strconv.FormatFloat(data.Amount, 'f', 2, 64)
	values := url.Values{
		"type":              {"sale"},
		"amount":            {amtStr},
		"email":             {data.Email},
		"plan_id":           {data.PlanID},
		"billing_method":    {"recurring"},
		"security_key":      {c.SecurityKey},
		"currency":          {data.Currency},
		"recurring":         {"add_subscription"},
		"order_description": {"Open Rails Subscription"},
		"first_name":        {data.FirstName},
		"last_name":         {data.LastName},
		"address1":          {data.Address1},
		"city":              {data.City},
		"state":             {data.State},
		"zip":               {data.Zip},
		"country":           {data.Country},
	}

	if trimmed := strings.TrimSpace(data.OrderID); trimmed != "" {
		values.Set("orderid", trimmed)
	}
	if trimmed := strings.TrimSpace(data.PONumber); trimmed != "" {
		values.Set("ponumber", trimmed)
	}
	if trimmed := strings.TrimSpace(data.CustomerID); trimmed != "" && strings.TrimSpace(data.CustomerVaultID) == "" {
		values.Set("customerid", trimmed)
	}
	if data.PaymentToken != "" {
		values.Set("payment_token", data.PaymentToken)
	}
	if data.CustomerVaultID != "" {
		values.Set("customer_vault_id", data.CustomerVaultID)
	}
	if trimmed := strings.TrimSpace(data.StartDate); trimmed != "" {
		values.Set("start_date", trimmed)
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
		return nil, newAddSubscriptionError(response, output)
	}

	return &AddSubscriptionResponse{
		Type:           output.Get("type"),
		Authcode:       output.Get("authcode"),
		TransactionID:  output.Get("transactionid"),
		SubscriptionID: output.Get("subscription_id"),
	}, nil
}

func (c *NMIClient) UpdateRecurringSubscription(subscriptionID, planAmount string, planPayments int) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}
	if strings.TrimSpace(subscriptionID) == "" || strings.TrimSpace(planAmount) == "" {
		return "", errors.New("missing required fields: subscriptionID, planAmount")
	}

	values := url.Values{
		"recurring":       {"update_subscription"},
		"security_key":    {c.SecurityKey},
		"subscription_id": {subscriptionID},
		"plan_amount":     {planAmount},
		"plan_payments":   {fmt.Sprintf("%d", planPayments)},
	}

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return "", err
	}

	output, err := parseDirectResponse(response)
	if err != nil {
		return "", err
	}
	if !isDirectResponseApproved(output) {
		return "", fmt.Errorf("failed to update subscription: %s", responseText(output, response))
	}

	return response, nil
}

func (c *NMIClient) UpdateSubscriptionPaymentSource(subscriptionID, customerVaultID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(subscriptionID) == "" {
		return errors.New("subscription ID is required")
	}
	if strings.TrimSpace(customerVaultID) == "" {
		return errors.New("customer vault ID is required")
	}

	values := url.Values{
		"recurring":         {"update_subscription"},
		"security_key":      {c.SecurityKey},
		"subscription_id":   {subscriptionID},
		"customer_vault_id": {customerVaultID},
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
		return fmt.Errorf("failed to update subscription payment source: %s", responseText(output, response))
	}

	return nil
}

func (c *NMIClient) DeleteRecurringSubscription(subscriptionID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(subscriptionID) == "" {
		return errors.New("subscriptionID is required")
	}

	values := url.Values{
		"recurring":       {"delete_subscription"},
		"security_key":    {c.SecurityKey},
		"subscription_id": {subscriptionID},
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
		return fmt.Errorf("failed to delete subscription: %s", responseText(output, response))
	}

	return nil
}

func (c *NMIClient) AttemptManualRebill(params ManualRebillParams) (*ManualRebillResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return &ManualRebillResponse{Success: false, ErrorMessage: err.Error()}, err
	}
	if params.VaultID == "" || params.BillingID == "" || params.SubscriptionID == "" {
		err := errors.New("vault ID, billing ID, and subscription ID are required")
		return &ManualRebillResponse{Success: false, ErrorMessage: err.Error()}, err
	}

	values := url.Values{
		"type":              {"sale"},
		"security_key":      {c.SecurityKey},
		"customer_vault_id": {params.VaultID},
		"billing_id":        {params.BillingID},
		"subscription_id":   {params.SubscriptionID},
		"recurring":         {"rebill_subscription"},
		"order_description": {"Manual Rebill - Open Rails Subscription"},
	}

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return &ManualRebillResponse{Success: false, ErrorMessage: fmt.Sprintf("request failed: %s", err.Error())}, err
	}

	output, err := parseDirectResponse(response)
	if err != nil {
		return &ManualRebillResponse{Success: false, ErrorMessage: err.Error()}, err
	}
	if isDirectResponseApproved(output) {
		return &ManualRebillResponse{Success: true, TransactionID: output.Get("transactionid")}, nil
	}

	errorMessage := responseText(output, "Unknown error")
	return &ManualRebillResponse{Success: false, ErrorMessage: errorMessage}, nil
}

func (c *NMIClient) GetTransactionDetails(transactionID string) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}
	if strings.TrimSpace(transactionID) == "" {
		return "", errors.New("transactionID is required")
	}

	values := url.Values{
		"Servicert_type": {"transaction"},
		"security_key":   {c.SecurityKey},
		"transaction_id": {transactionID},
	}
	return c.sendQueryRequest(values)
}

func (c *NMIClient) GetCustomerVaultData(customerVaultID string) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}

	values := url.Values{
		"Servicert_type": {"customer_vault"},
		"security_key":   {c.SecurityKey},
	}
	if customerVaultID != "" {
		values.Set("customer_vault_id", customerVaultID)
	}

	return c.sendQueryRequest(values)
}

func (c *NMIClient) GetSubscriptionData(subscriptionID string) (string, error) {
	return c.QueryRecurringSubscriptions(RecurringQueryParams{SubscriptionID: subscriptionID})
}

func (c *NMIClient) GetRecurringPlanData() (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}

	values := url.Values{
		"Servicert_type": {"recurring_plans"},
		"security_key":   {c.SecurityKey},
	}
	return c.sendQueryRequest(values)
}

func (c *NMIClient) QueryRecurringSubscriptions(params RecurringQueryParams) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}

	values := url.Values{
		"report_type":  {"recurring"},
		"security_key": {c.SecurityKey},
	}
	if strings.TrimSpace(params.SubscriptionID) != "" {
		values.Set("subscription_id", params.SubscriptionID)
	}
	if params.ResultLimit > 0 {
		values.Set("result_limit", strconv.Itoa(params.ResultLimit))
	}
	if params.PageNumber >= 0 {
		values.Set("page_number", strconv.Itoa(params.PageNumber))
	}
	if strings.TrimSpace(params.ResultOrder) != "" {
		values.Set("result_order", params.ResultOrder)
	}

	return c.sendQueryRequest(values)
}

func (c *NMIClient) SearchTransactions(filter QueryFilter) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}

	values := url.Values{
		"Servicert_type": {"transaction"},
		"security_key":   {c.SecurityKey},
	}
	if filter.StartDate != "" {
		values.Set("start_date", filter.StartDate)
	}
	if filter.EndDate != "" {
		values.Set("end_date", filter.EndDate)
	}
	if filter.Condition != "" {
		values.Set("condition", filter.Condition)
	}
	if filter.ActionType != "" {
		values.Set("action_type", filter.ActionType)
	}
	if filter.PageNumber > 0 {
		values.Set("page_number", fmt.Sprintf("%d", filter.PageNumber))
	}
	if filter.ResultLimit > 0 {
		values.Set("result_limit", fmt.Sprintf("%d", filter.ResultLimit))
	}

	return c.sendQueryRequest(values)
}
