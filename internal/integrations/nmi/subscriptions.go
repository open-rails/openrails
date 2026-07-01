package nmi

import (
	"errors"
	"fmt"
	"net/http"
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
	StartDate  string
	EndDate    string
	Condition  string
	ActionType string
	// OrderID filters by the transaction's order_id — the correlation handle
	// OpenRails stamps on rebills/sales, used by the intent verifier to answer
	// "did this charge land?" via reads.
	OrderID     string
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
	// ResponseCode is the NMI/rail response_code for a declined rebill
	// (0 when approved or unavailable). Used by dunning to distinguish hard
	// declines (stop retries) from soft declines (keep retrying).
	ResponseCode int
}

// AddRecurringSubscription stays on classic Direct Post DELIBERATELY (#663):
// type=sale + recurring=add_subscription is an ATOMIC first-charge + enroll
// (+ delayed start via start_date) in one call. v5's POST /subscriptions has
// no documented start_date for plan-linked subscriptions and returns no
// first-charge transaction, so porting it would split one atomic money op
// into two non-atomic ones.
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

// UpdateRecurringSubscription updates the money terms of a live subscription
// via PATCH /v5/subscriptions/{id}. planAmount is a two-decimal dollar string
// (unchanged classic signature).
func (c *NMIClient) UpdateRecurringSubscription(subscriptionID, planAmount string, planPayments int) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}
	subID := strings.TrimSpace(subscriptionID)
	if subID == "" || strings.TrimSpace(planAmount) == "" {
		return "", errors.New("missing required fields: subscriptionID, planAmount")
	}
	amountCents, err := v5AmountToCents(planAmount)
	if err != nil {
		return "", fmt.Errorf("invalid plan amount %q: %w", planAmount, err)
	}

	body := map[string]any{
		"plan_amount":   centsJSONAmount(amountCents),
		"plan_payments": planPayments,
	}
	var sub V5Subscription
	if err := c.sendV5Request(http.MethodPatch, "/subscriptions/"+url.PathEscape(subID), body, &sub); err != nil {
		return "", fmt.Errorf("failed to update subscription: %w", err)
	}
	return sub.ID, nil
}

// UpdateSubscriptionPaymentSource repoints a subscription at a different vault
// customer via PATCH /v5/subscriptions/{id} (payment_details.customer_vault_id).
func (c *NMIClient) UpdateSubscriptionPaymentSource(subscriptionID, customerVaultID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	subID := strings.TrimSpace(subscriptionID)
	if subID == "" {
		return errors.New("subscription ID is required")
	}
	vaultID := strings.TrimSpace(customerVaultID)
	if vaultID == "" {
		return errors.New("customer vault ID is required")
	}

	body := map[string]any{
		"payment_details": &v5PaymentDetails{CustomerVaultID: vaultID},
	}
	if err := c.sendV5Request(http.MethodPatch, "/subscriptions/"+url.PathEscape(subID), body, nil); err != nil {
		return fmt.Errorf("failed to update subscription payment source: %w", err)
	}
	return nil
}

// ErrProviderReadOnly is returned by every NMI mutation when the provider is
// read-only (mode=readonly, #346). A reactive operation that needed the write
// has genuinely failed and must surface as an error.
var ErrProviderReadOnly = errors.New("nmi: provider writes are blocked (mode=readonly)")

// DeleteRecurringSubscription cancels a subscription via
// DELETE /v5/subscriptions/{id}. A 404 surfaces as ErrV5NotFound — callers on
// the certainty path treat "already gone" explicitly, never silently.
func (c *NMIClient) DeleteRecurringSubscription(subscriptionID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	subID := strings.TrimSpace(subscriptionID)
	if subID == "" {
		return errors.New("subscriptionID is required")
	}
	if err := c.sendV5Request(http.MethodDelete, "/subscriptions/"+url.PathEscape(subID), nil, nil); err != nil {
		return fmt.Errorf("failed to delete subscription: %w", err)
	}
	return nil
}

// AttemptManualRebill stays on classic Direct Post DELIBERATELY (#663):
// recurring=rebill_subscription (charge the subscription NOW, against its own
// schedule state) has no v5 equivalent of any kind.
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
	return &ManualRebillResponse{
		Success:      false,
		ErrorMessage: errorMessage,
		ResponseCode: parseMobiusResponseCode(output),
	}, nil
}

// AddRecurringPlan creates a new NMI Recurring Plan via POST /v5/plans. NMI
// plan amounts are dollars; OpenRails stores integer cents, converted at this
// wire boundary. dayFrequency is the billing interval in days; planPayments is
// the total number of payments (0 = bill forever). Frequency and payments are
// immutable once a plan is created.
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

	body := v5PlanCreateRequest{
		PlanID:       planID,
		PlanName:     planName,
		PlanAmount:   centsJSONAmount(planAmountCents),
		PlanPayments: planPayments,
		DayFrequency: dayFrequency,
	}
	if err := c.sendV5Request(http.MethodPost, "/plans", body, nil); err != nil {
		return fmt.Errorf("failed to add recurring plan: %w", err)
	}
	return nil
}

// EditRecurringPlan updates the mutable fields of an existing NMI Recurring
// Plan via PATCH /v5/plans/{id}. NMI only permits the plan name and amount to
// change; frequency and payment count are immutable once a plan exists.
func (c *NMIClient) EditRecurringPlan(planID, planName string, planAmountCents int64) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(planID) == "" {
		return errors.New("planID is required")
	}

	body := v5PlanUpdateRequest{
		PlanName:   strings.TrimSpace(planName),
		PlanAmount: centsJSONAmount(planAmountCents),
	}
	if err := c.sendV5Request(http.MethodPatch, "/plans/"+url.PathEscape(strings.TrimSpace(planID)), body, nil); err != nil {
		return fmt.Errorf("failed to edit recurring plan: %w", err)
	}
	return nil
}

// RecurringPlanDetail is the parsed view of a single NMI recurring plan
// returned by GetRecurringPlanDetailByID. Found=false means no plan matched
// the id. DayFrequency is the billing interval in days; it is 0 when the plan
// is month-based (parity with the classic recurring_plans report).
type RecurringPlanDetail struct {
	Found        bool
	Name         string
	AmountCents  int64
	DayFrequency int
}

// GetRecurringPlanByID performs a strongly-consistent lookup of a single
// recurring plan by its operator-chosen plan_id.
func (c *NMIClient) GetRecurringPlanByID(planID string) (found bool, name string, amountCents int64, err error) {
	detail, err := c.GetRecurringPlanDetailByID(planID)
	if err != nil {
		return false, "", 0, err
	}
	return detail.Found, detail.Name, detail.AmountCents, nil
}

// GetRecurringPlanDetailByID fetches one plan via GET /v5/plans/{id} so
// callers validating an operator-supplied link can confirm the linked plan
// matches the OpenRails price's money terms, not just that it exists.
func (c *NMIClient) GetRecurringPlanDetailByID(planID string) (RecurringPlanDetail, error) {
	if err := c.checkConfiguration(); err != nil {
		return RecurringPlanDetail{}, err
	}
	trimmed := strings.TrimSpace(planID)
	if trimmed == "" {
		return RecurringPlanDetail{}, errors.New("planID is required")
	}

	var plan V5Plan
	err := c.sendV5Request(http.MethodGet, "/plans/"+url.PathEscape(trimmed), nil, &plan)
	if errors.Is(err, ErrV5NotFound) {
		return RecurringPlanDetail{}, nil
	}
	if err != nil {
		return RecurringPlanDetail{}, err
	}

	cents, convErr := v5AmountToCents(plan.PlanAmount)
	if convErr != nil {
		return RecurringPlanDetail{Found: true, Name: plan.PlanName}, fmt.Errorf("failed to parse plan amount %q: %w", plan.PlanAmount, convErr)
	}
	// day_frequency is "0"/empty for month-based plans; DayFrequency stays 0.
	dayFreq, _ := strconv.Atoi(strings.TrimSpace(plan.DayFrequency))
	return RecurringPlanDetail{Found: true, Name: plan.PlanName, AmountCents: cents, DayFrequency: dayFreq}, nil
}

// SearchTransactions stays on the classic Query API (query.php) DELIBERATELY
// (#663): v5 payments has no list/search endpoint (only GET by known id), and
// v4's transaction report requires a partner-portal key. This is the bulk
// reconcile pull and the order-id evidence probes' read path.
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
	if filter.OrderID != "" {
		values.Set("order_id", filter.OrderID)
	}
	if filter.PageNumber > 0 {
		values.Set("page_number", fmt.Sprintf("%d", filter.PageNumber))
	}
	if filter.ResultLimit > 0 {
		values.Set("result_limit", fmt.Sprintf("%d", filter.ResultLimit))
	}

	return c.sendQueryRequest(values)
}
