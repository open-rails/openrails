package nmi

import (
	"encoding/xml"
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
	OrderID        string
	PONumber       string
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

// ErrSubscriptionDeletesDisabled is returned by DeleteRecurringSubscription when
// processor-side subscription deletions are blocked by feature flag. Callers
// must treat it as "deliberately skipped": proceed with local lifecycle changes
// and leave the remote subscription for reconciliation — never as success.
var ErrSubscriptionDeletesDisabled = errors.New("nmi: processor subscription deletes are disabled (feature_flags.disable_processor_subscription_deletions)")

func (c *NMIClient) DeleteRecurringSubscription(subscriptionID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if c.SubscriptionDeletesDisabled {
		return ErrSubscriptionDeletesDisabled
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
	if trimmed := strings.TrimSpace(params.OrderID); trimmed != "" {
		values.Set("orderid", trimmed)
	}
	if trimmed := strings.TrimSpace(params.PONumber); trimmed != "" {
		values.Set("ponumber", trimmed)
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
		transactionID := strings.TrimSpace(output.Get("transactionid"))
		if transactionID == "" {
			err := errors.New("approved manual rebill missing transaction id")
			return &ManualRebillResponse{Success: false, ErrorMessage: err.Error()}, err
		}
		return &ManualRebillResponse{Success: true, TransactionID: transactionID}, nil
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
		"report_type":    {"transaction"},
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
		"report_type":  {"customer_vault"},
		"security_key": {c.SecurityKey},
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
		"report_type":  {"recurring_plans"},
		"security_key": {c.SecurityKey},
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

// AddRecurringPlan creates a new NMI Recurring Plan via the Direct Post API
// (recurring=add_plan). NMI plan amounts are denominated in dollars, while
// OpenRails stores money in integer cents, so planAmountCents is converted to a
// dollar string here. dayFrequency is the billing interval in days (NMI's
// day_frequency). planPayments is the total number of payments (0 = bill
// forever). Frequency and payments are immutable once a plan is created.
func (c *NMIClient) AddRecurringPlan(planID, planName string, planAmountCents int64, dayFrequency, planPayments int) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(planID) == "" {
		return errors.New("planID is required")
	}
	if strings.TrimSpace(planName) == "" {
		return errors.New("planName is required")
	}
	if dayFrequency <= 0 {
		return errors.New("dayFrequency must be greater than zero")
	}

	values := url.Values{
		"recurring":     {"add_plan"},
		"security_key":  {c.SecurityKey},
		"plan_id":       {planID},
		"plan_name":     {planName},
		"plan_amount":   {centsToDollarString(planAmountCents)},
		"day_frequency": {strconv.Itoa(dayFrequency)},
		"plan_payments": {strconv.Itoa(planPayments)},
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
		return fmt.Errorf("failed to add recurring plan: %s", responseText(output, response))
	}

	return nil
}

// EditRecurringPlan updates the mutable fields of an existing NMI Recurring Plan
// (recurring=edit_plan). NMI only permits the plan name and amount to change;
// frequency and payment count are immutable once a plan exists. planAmountCents
// is converted from cents to a dollar string for NMI.
func (c *NMIClient) EditRecurringPlan(planID, planName string, planAmountCents int64) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(planID) == "" {
		return errors.New("planID is required")
	}

	values := url.Values{
		"recurring":    {"edit_plan"},
		"security_key": {c.SecurityKey},
		"plan_id":      {planID},
		"plan_amount":  {centsToDollarString(planAmountCents)},
	}
	if name := strings.TrimSpace(planName); name != "" {
		values.Set("plan_name", name)
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
		return fmt.Errorf("failed to edit recurring plan: %s", responseText(output, response))
	}

	return nil
}

// DeleteRecurringPlan soft-deletes an NMI Recurring Plan (recurring=delete_plan).
// NOTE: deleting a plan in NMI does not stop existing subscriptions billing on
// it, so OpenRails deliberately does not call this on price deactivation; it is
// retained for explicit cleanup paths only.
func (c *NMIClient) DeleteRecurringPlan(planID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(planID) == "" {
		return errors.New("planID is required")
	}

	values := url.Values{
		"recurring":    {"delete_plan"},
		"security_key": {c.SecurityKey},
		"plan_id":      {planID},
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
		return fmt.Errorf("failed to delete recurring plan: %s", responseText(output, response))
	}

	return nil
}

// recurringPlanQueryResponse mirrors the XML returned by the NMI Query API for
// the recurring_plans report type. Only the fields OpenRails needs are mapped.
type recurringPlanQueryResponse struct {
	XMLName xml.Name             `xml:"nm_response"`
	Plans   []recurringPlanQuery `xml:"plan"`
}

type recurringPlanQuery struct {
	PlanID       string `xml:"plan_id"`
	PlanName     string `xml:"plan_name"`
	PlanAmount   string `xml:"plan_amount"`
	DayFrequency string `xml:"day_frequency"`
}

// RecurringPlanDetail is the parsed view of a single NMI recurring plan returned
// by GetRecurringPlanDetailByID. Found=false means no plan matched the id.
// DayFrequency is the billing interval in days; it is 0 when NMI reports a
// month-based frequency (the query API does not return a day count for those).
type RecurringPlanDetail struct {
	Found        bool
	Name         string
	AmountCents  int64
	DayFrequency int
}

// GetRecurringPlanByID performs a strongly-consistent lookup of a single
// recurring plan by its operator-chosen plan_id. It queries the NMI Query API
// (recurring_plans report) filtered by plan_id and parses the XML response.
// Returns found=false when no plan matches. amountCents is the plan amount
// converted from NMI dollars back into integer cents to match OpenRails storage.
func (c *NMIClient) GetRecurringPlanByID(planID string) (found bool, name string, amountCents int64, err error) {
	detail, err := c.GetRecurringPlanDetailByID(planID)
	if err != nil {
		return false, "", 0, err
	}
	return detail.Found, detail.Name, detail.AmountCents, nil
}

// GetRecurringPlanDetailByID is the richer sibling of GetRecurringPlanByID: it
// returns the plan name, amount (cents), AND billing frequency (day_frequency)
// so callers validating an operator-supplied link can confirm the linked plan
// matches the OpenRails price's money terms, not just that it exists.
func (c *NMIClient) GetRecurringPlanDetailByID(planID string) (RecurringPlanDetail, error) {
	if err := c.checkConfiguration(); err != nil {
		return RecurringPlanDetail{}, err
	}
	if strings.TrimSpace(planID) == "" {
		return RecurringPlanDetail{}, errors.New("planID is required")
	}

	values := url.Values{
		"report_type":  {"recurring_plans"},
		"security_key": {c.SecurityKey},
		"plan_id":      {planID},
	}

	response, err := c.sendQueryRequest(values)
	if err != nil {
		return RecurringPlanDetail{}, err
	}

	var parsed recurringPlanQueryResponse
	if err := xml.Unmarshal([]byte(response), &parsed); err != nil {
		return RecurringPlanDetail{}, fmt.Errorf("failed to parse recurring plan query response: %w", err)
	}

	for _, p := range parsed.Plans {
		if strings.TrimSpace(p.PlanID) == strings.TrimSpace(planID) {
			cents, convErr := dollarStringToCents(p.PlanAmount)
			if convErr != nil {
				return RecurringPlanDetail{Found: true, Name: p.PlanName}, fmt.Errorf("failed to parse plan amount %q: %w", p.PlanAmount, convErr)
			}
			// day_frequency is optional in the query response (absent for
			// month-based plans); a blank/invalid value leaves DayFrequency 0.
			dayFreq, _ := strconv.Atoi(strings.TrimSpace(p.DayFrequency))
			return RecurringPlanDetail{Found: true, Name: p.PlanName, AmountCents: cents, DayFrequency: dayFreq}, nil
		}
	}
	return RecurringPlanDetail{}, nil
}

// centsToDollarString converts integer cents into a fixed two-decimal dollar
// string as required by NMI's plan_amount parameter.
func centsToDollarString(cents int64) string {
	return strconv.FormatFloat(float64(cents)/100.0, 'f', 2, 64)
}

// dollarStringToCents converts an NMI dollar amount string (e.g. "9.99") back
// into integer cents, rounding to the nearest cent.
func dollarStringToCents(dollars string) (int64, error) {
	trimmed := strings.TrimSpace(dollars)
	if trimmed == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, err
	}
	if f < 0 {
		return -int64(-f*100 + 0.5), nil
	}
	return int64(f*100 + 0.5), nil
}

func (c *NMIClient) SearchTransactions(filter QueryFilter) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}

	values := url.Values{
		"report_type":  {"transaction"},
		"security_key": {c.SecurityKey},
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
