package nmi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
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
	// ScheduleOnly creates the accepted recurring schedule without a sale.
	// Amount must be zero; no transaction or stored-credential proof is implied.
	ScheduleOnly bool
	CardUserData
	PlanID          string
	CustomerVaultID string
	// BillingID binds the subscription to one stored card in the vault; ""
	// uses the vault's priority-1 entry.
	BillingID    string
	Email        string
	Currency     string
	PaymentToken string
	// Amount is the enrollment first charge in minor units, rendered by
	// WireAmount like the plan amount so the two never disagree.
	Amount     moneyutil.Cents
	OrderID    string
	PONumber   string
	CustomerID string
	StartDate  string
	// StoredCredential is the enrollment charge's recurring initial CIT
	// (initiated_by=customer, stored_credential_indicator=stored; this lane
	// always sends billing_method=recurring). Required; nil when ScheduleOnly.
	StoredCredential *StoredCredential
}

type QueryFilter struct {
	StartDate  string
	EndDate    string
	Condition  string
	ActionType string
	// OrderID filters by the transaction's order_id — the correlation handle
	// OpenRails stamps on rebills/sales, used by the intent verifier to answer
	// "did this charge land?" via reads.
	OrderID string
	// OrderDescription filters exact durable merchant-provided invoice identity.
	OrderDescription string
	// SubscriptionID filters to the sales NMI's recurring engine made for one
	// schedule, whatever order reference the schedule carries.
	SubscriptionID string
	// CustomerVaultID filters to one vault's transactions.
	CustomerVaultID string
	// TransactionID filters to transaction ids (a comma list).
	TransactionID string
	PageNumber    int
	ResultLimit   int
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
	// StoredCredential is the recurring MIT: initiated_by=merchant,
	// stored_credential_indicator=used, initial_transaction_id and
	// billing_method=recurring. Required.
	StoredCredential *StoredCredential
}

type ManualRebillResponse struct {
	// Declined is the gateway's explicit response=2, distinct from response=3 errors.
	Declined      bool
	Success       bool
	TransactionID string
	ErrorMessage  string
	// ResponseCode is the NMI/rail response_code for a declined rebill
	// (0 when approved or unavailable). Used by dunning to distinguish hard
	// declines (stop retries) from soft declines (keep retrying).
	ResponseCode int
	// AVSResponse and CVVResponse are the gateway's verification letters.
	AVSResponse, CVVResponse string
}

// AddRecurringSubscription uses Classic Direct Post. Paid enrollment explicitly
// combines sale and schedule; ScheduleOnly omits transaction fields entirely.
// A future start_date controls the recurring schedule, never the sale amount.
// https://docs.nmi.com/reference/subscriptions-management
// https://docs.nmi.com/reference/transactions-processing
func (c *NMIClient) AddRecurringSubscription(ctx context.Context, data RecurringPaymentData) (*AddSubscriptionResponse, error) {
	if err := moneyutil.RequireFiatCurrency(data.Currency); err != nil {
		return nil, err
	}
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	if data.PlanID == "" {
		return nil, errors.New("PlanID is required")
	}
	if data.CustomerVaultID == "" && data.PaymentToken == "" {
		return nil, errors.New("either customer vault or payment token is required")
	}
	if data.ScheduleOnly {
		if data.Amount != 0 || data.StoredCredential != nil {
			return nil, errors.New("schedule-only enrollment cannot carry sale or credential-on-file fields")
		}
	} else if err := data.StoredCredential.Validate(); err != nil {
		return nil, err
	}

	values := url.Values{
		"email":             {data.Email},
		"plan_id":           {data.PlanID},
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
	if trimmed := strings.TrimSpace(data.BillingID); trimmed != "" && data.CustomerVaultID != "" {
		values.Set("billing_id", trimmed)
	}
	if trimmed := strings.TrimSpace(data.StartDate); trimmed != "" {
		values.Set("start_date", trimmed)
	}
	if !data.ScheduleOnly {
		amount, err := WireAmount(data.Amount, data.Currency)
		if err != nil {
			return nil, err
		}
		values.Set("type", "sale")
		values.Set("amount", amount)
		values.Set("billing_method", "recurring")
		data.StoredCredential.ApplyToForm(values)
	}

	response, err := c.sendDirectRequest(ctx, values)
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

// UpdateRecurringSubscription uses classic Direct Post: the documented PATCH
// /v5/subscriptions/{id} answers E_ROUTE_NOT_FOUND on the live gateway.
func (c *NMIClient) UpdateRecurringSubscription(ctx context.Context, subscriptionID, planAmount string, planPayments int) (string, error) {
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

	response, err := c.sendDirectRequest(ctx, values)
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

// UpdateRecurringSubscriptionPlan moves a schedule onto another named plan
// (Direct Post recurring=update_subscription with plan_id). NMI ignores
// plan_amount on a schedule attached to a named plan; switching plans is the
// only in-place change it applies there, and it keeps the next billing date.
func (c *NMIClient) UpdateRecurringSubscriptionPlan(ctx context.Context, subscriptionID, planID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(subscriptionID) == "" || strings.TrimSpace(planID) == "" {
		return errors.New("missing required fields: subscriptionID, planID")
	}
	values := url.Values{
		"recurring":       {"update_subscription"},
		"security_key":    {c.SecurityKey},
		"subscription_id": {subscriptionID},
		"plan_id":         {planID},
	}
	response, err := c.sendDirectRequest(ctx, values)
	if err != nil {
		return err
	}
	output, err := parseDirectResponse(response)
	if err != nil {
		return err
	}
	if !isDirectResponseApproved(output) {
		return fmt.Errorf("failed to update subscription plan: %s", responseText(output, response))
	}
	return nil
}

// NamedPlan reports whether the schedule is attached to a named NMI plan (a
// custom-amount schedule carries a plan object with an empty plan_name).
func (s V5Subscription) NamedPlan() bool {
	return s.Plan != nil && strings.TrimSpace(s.Plan.PlanName) != "" && strings.TrimSpace(s.Plan.ID) != ""
}

// UpdateSubscriptionPaymentSource uses classic Direct Post: v5 has no live
// subscription-update route (see UpdateRecurringSubscription).
func (c *NMIClient) UpdateSubscriptionPaymentSource(ctx context.Context, subscriptionID, customerVaultID string) error {
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

	response, err := c.sendDirectRequest(ctx, values)
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

// ErrProviderReadOnly is returned by every NMI mutation when the provider is
// read-only. A reactive operation that needed the write has failed and must
// surface as an error.
var ErrProviderReadOnly = errors.New("nmi: provider writes are blocked (mode=readonly)")

// DeleteRecurringSubscription cancels a subscription via
// DELETE /v5/subscriptions/{id}. A 404 surfaces as ErrV5NotFound — callers on
// the certainty path treat "already gone" explicitly, never silently.
func (c *NMIClient) DeleteRecurringSubscription(ctx context.Context, subscriptionID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	subID := strings.TrimSpace(subscriptionID)
	if subID == "" {
		return errors.New("subscriptionID is required")
	}
	if err := c.sendV5Request(ctx, http.MethodDelete, "/subscriptions/"+url.PathEscape(subID), nil, nil); err != nil {
		return fmt.Errorf("failed to delete subscription: %w", err)
	}
	return nil
}

// AttemptManualRebill uses classic Direct Post: recurring=rebill_subscription
// (charge the schedule now, against its own state) has no v5 equivalent.
func (c *NMIClient) AttemptManualRebill(ctx context.Context, params ManualRebillParams) (*ManualRebillResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return &ManualRebillResponse{Success: false, ErrorMessage: err.Error()}, err
	}
	if params.VaultID == "" || params.BillingID == "" || params.SubscriptionID == "" {
		err := errors.New("vault ID, billing ID, and subscription ID are required")
		return &ManualRebillResponse{Success: false, ErrorMessage: err.Error()}, err
	}
	if err := params.StoredCredential.Validate(); err != nil {
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
	params.StoredCredential.ApplyToForm(values)

	response, err := c.sendDirectRequest(ctx, values)
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
			err := ambiguous(errors.New("approved manual rebill missing transaction id"))
			return &ManualRebillResponse{Success: false, ErrorMessage: err.Error()}, err
		}
		return &ManualRebillResponse{Success: true, TransactionID: transactionID, AVSResponse: strings.TrimSpace(output.Get("avsresponse")), CVVResponse: strings.TrimSpace(output.Get("cvvresponse"))}, nil
	}

	rejection := newSaleError(response, output)
	result := &ManualRebillResponse{
		Success:       false,
		Declined:      !RequiresVerification(rejection),
		TransactionID: strings.TrimSpace(output.Get("transactionid")),
		ErrorMessage:  rejection.Error(),
		ResponseCode:  parseMobiusResponseCode(output),
		AVSResponse:   strings.TrimSpace(output.Get("avsresponse")),
		CVVResponse:   strings.TrimSpace(output.Get("cvvresponse")),
	}
	if RequiresVerification(rejection) {
		return result, rejection
	}
	return result, nil
}

// AddRecurringPlan creates an NMI recurring plan via POST /v5/plans; the amount
// is rendered by WireAmount. dayFrequency is the interval in days, planPayments
// the total payments (0 = forever); both are immutable once the plan exists.
func (c *NMIClient) AddRecurringPlan(ctx context.Context, planID, planName string, planAmountCents moneyutil.Cents, currency string, dayFrequency, planPayments int) error {
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

	amount, err := WireAmount(planAmountCents, currency)
	if err != nil {
		return err
	}

	body := v5PlanCreateRequest{
		PlanID:       planID,
		PlanName:     planName,
		PlanAmount:   json.RawMessage(amount),
		PlanPayments: planPayments,
		DayFrequency: dayFrequency,
	}
	if err := c.sendV5Request(ctx, http.MethodPost, "/plans", body, nil); err != nil {
		return fmt.Errorf("failed to add recurring plan: %w", err)
	}
	return nil
}

// EditRecurringPlan uses classic Direct Post: the documented PATCH
// /v5/plans/{id} answers E_ROUTE_NOT_FOUND on the live gateway. NMI lets only
// the plan name and amount change.
func (c *NMIClient) EditRecurringPlan(ctx context.Context, planID, planName string, planAmountCents moneyutil.Cents, currency string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(planID) == "" {
		return errors.New("planID is required")
	}

	amount, err := WireAmount(planAmountCents, currency)
	if err != nil {
		return err
	}

	// current_plan_id names the plan to edit (live-verified); plan_id answers
	// "Invalid Recurring Plan ID".
	values := url.Values{
		"recurring":       {"edit_plan"},
		"security_key":    {c.SecurityKey},
		"current_plan_id": {planID},
		"plan_amount":     {amount},
	}
	if name := strings.TrimSpace(planName); name != "" {
		values.Set("plan_name", name)
	}

	response, err := c.sendDirectRequest(ctx, values)
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

// centsToDollarString renders cents as a fixed two-decimal dollar string.
func centsToDollarString(cents moneyutil.Cents) string {
	return string(centsJSONAmount(cents))
}

// RecurringPlanDetail is one NMI recurring plan from GetRecurringPlanDetailByID.
// Found=false means no plan matched the id. DayFrequency is the interval in
// days, 0 for a month-based plan.
type RecurringPlanDetail struct {
	ID           string
	Payments     *int
	Found        bool
	Name         string
	AmountCents  int64
	DayFrequency int
}

// GetRecurringPlanByID performs a strongly-consistent lookup of a single
// recurring plan by its operator-chosen plan_id.
func (c *NMIClient) GetRecurringPlanByID(ctx context.Context, planID, currency string) (found bool, name string, amountCents int64, err error) {
	detail, err := c.GetRecurringPlanDetailByID(ctx, planID, currency)
	if err != nil {
		return false, "", 0, err
	}
	return detail.Found, detail.Name, detail.AmountCents, nil
}

// GetRecurringPlanDetailByID fetches one plan via GET /v5/plans/{id} so
// callers validating an operator-supplied link can confirm the linked plan
// matches the OpenRails price's money terms, not just that it exists.
func (c *NMIClient) GetRecurringPlanDetailByID(ctx context.Context, planID, currency string) (RecurringPlanDetail, error) {
	if err := c.checkConfiguration(); err != nil {
		return RecurringPlanDetail{}, err
	}
	trimmed := strings.TrimSpace(planID)
	if trimmed == "" {
		return RecurringPlanDetail{}, errors.New("planID is required")
	}

	var plan V5Plan
	err := c.sendV5Request(ctx, http.MethodGet, "/plans/"+url.PathEscape(trimmed), nil, &plan)
	if errors.Is(err, ErrV5NotFound) {
		return RecurringPlanDetail{}, nil
	}
	if err != nil {
		return RecurringPlanDetail{}, err
	}

	minor, err := moneyutil.DecimalToRailMinor(currency, plan.PlanAmount)
	if err != nil {
		return RecurringPlanDetail{Found: true, Name: plan.PlanName}, fmt.Errorf("nmi plan amount: %w", err)
	}
	// day_frequency is "0"/empty for month-based plans; DayFrequency stays 0.
	dayFreq, _ := strconv.Atoi(strings.TrimSpace(plan.DayFrequency))
	var payments *int
	if text := strings.TrimSpace(plan.PlanPayments); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 0 {
			return RecurringPlanDetail{}, fmt.Errorf("NMI plan has an invalid payment count")
		}
		payments = &parsed
	}
	return RecurringPlanDetail{Found: true, ID: plan.ID, Name: plan.PlanName, AmountCents: int64(minor), DayFrequency: dayFreq, Payments: payments}, nil
}

// SearchTransactions uses the classic Query API: v5 has no payments search
// (only GET by id) and v4's transaction report needs a partner key. It is the
// bulk reconcile pull and the order-id probes' read path.
func (c *NMIClient) SearchTransactions(ctx context.Context, filter QueryFilter) (string, error) {
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
	if filter.SubscriptionID != "" {
		values.Set("subscription_id", filter.SubscriptionID)
	}
	if filter.OrderID != "" {
		values.Set("order_id", filter.OrderID)
	}
	if filter.OrderDescription != "" {
		values.Set("order_description", filter.OrderDescription)
	}
	if filter.CustomerVaultID != "" {
		values.Set("customer_vault_id", filter.CustomerVaultID)
	}
	if filter.TransactionID != "" {
		values.Set("transaction_id", filter.TransactionID)
	}
	if filter.PageNumber > 0 {
		values.Set("page_number", fmt.Sprintf("%d", filter.PageNumber))
	}
	if filter.ResultLimit > 0 {
		values.Set("result_limit", fmt.Sprintf("%d", filter.ResultLimit))
	}

	return c.sendQueryRequest(ctx, values)
}
