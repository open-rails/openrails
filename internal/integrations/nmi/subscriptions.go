package nmi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

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

// RecurringPlanDetail is one NMI recurring plan from GetRecurringPlanDetailByID.
// Found=false means no plan matched the id. DayFrequency is the interval in
// days, 0 for a month-based plan, which bills every MonthFrequency months.
type RecurringPlanDetail struct {
	ID             string
	Payments       *int
	Found          bool
	Name           string
	AmountCents    int64
	DayFrequency   int
	MonthFrequency int
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

// GetRecurringPlanDetailByID reads one plan via GET /v5/plans/{id}: the terms
// a provider price takes from NMI.
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
	monthFreq, _ := strconv.Atoi(strings.TrimSpace(plan.MonthFrequency))
	var payments *int
	if text := strings.TrimSpace(plan.PlanPayments); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 0 {
			return RecurringPlanDetail{}, fmt.Errorf("NMI plan has an invalid payment count")
		}
		payments = &parsed
	}
	return RecurringPlanDetail{Found: true, ID: plan.ID, Name: plan.PlanName, AmountCents: int64(minor), DayFrequency: dayFreq, MonthFrequency: monthFreq, Payments: payments}, nil
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
