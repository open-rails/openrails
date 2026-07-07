package nmi

import (
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
	CardUserData
	PlanID          string
	CustomerVaultID string
	// BillingID binds the subscription to ONE stored card in the vault (#682
	// shared-vault support); "" uses the vault's priority-1 entry — always
	// correct under the one-vault-per-card minting policy.
	BillingID    string
	Email        string
	Currency     string
	PaymentToken string
	// Amount is DECIMAL MAJOR UNITS (dollars, typed #671) — NMI classic
	// recurring takes a decimal amount string.
	Amount     moneyutil.MajorUnits
	OrderID    string
	PONumber   string
	CustomerID string
	StartDate  string
	// StoredCredential carries the CIT credential-on-file fields (#297) for the
	// enrollment charge — the portal's recurring initial CIT "MUST INCLUDE"
	// billing_method=recurring + initiated_by=customer +
	// stored_credential_indicator=stored (billing_method is already always sent
	// by this lane). nil preserves the legacy shape.
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
	// StoredCredential carries the recurring-MIT credential-on-file fields
	// (#297): initiated_by=merchant + stored_credential_indicator=used +
	// initial_transaction_id=<recurring sequence anchor> + billing_method=
	// recurring. nil preserves the legacy shape.
	StoredCredential *StoredCredential
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
	if err := data.StoredCredential.validate(); err != nil {
		return nil, err
	}

	amtStr := strconv.FormatFloat(float64(data.Amount), 'f', 2, 64)
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
	if trimmed := strings.TrimSpace(data.BillingID); trimmed != "" && data.CustomerVaultID != "" {
		values.Set("billing_id", trimmed)
	}
	if trimmed := strings.TrimSpace(data.StartDate); trimmed != "" {
		values.Set("start_date", trimmed)
	}
	// billing_method=recurring is already stamped above; applyToForm re-setting
	// it for a recurring-agreement StoredCredential is idempotent.
	data.StoredCredential.applyToForm(values)

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

// UpdateRecurringSubscription stays on classic Direct Post DELIBERATELY
// (#663): PATCH /v5/subscriptions/{id} is documented but the live gateway
// answers E_ROUTE_NOT_FOUND (verified 2026-07-01) — subscription updates have
// no working v5 route.
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

// UpdateSubscriptionPaymentSource stays on classic Direct Post DELIBERATELY
// (#663): the v5 subscription-update route does not exist on the live gateway
// (see UpdateRecurringSubscription).
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
	if err := params.StoredCredential.validate(); err != nil {
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
	params.StoredCredential.applyToForm(values)

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
func (c *NMIClient) AddRecurringPlan(planID, planName string, planAmountCents moneyutil.Cents, dayFrequency, planPayments int) error {
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

// EditRecurringPlan stays on classic Direct Post DELIBERATELY (#663): the
// documented PATCH /v5/plans/{id} answers E_ROUTE_NOT_FOUND on the live
// gateway (verified 2026-07-01). NMI only permits the plan name and amount to
// change; frequency and payment count are immutable once a plan exists.
// planAmountCents is converted from cents to a dollar string for NMI.
func (c *NMIClient) EditRecurringPlan(planID, planName string, planAmountCents moneyutil.Cents) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if strings.TrimSpace(planID) == "" {
		return errors.New("planID is required")
	}

	// current_plan_id identifies the plan being edited (live-verified
	// 2026-07-01; sending plan_id instead answers "Invalid Recurring Plan ID"
	// — the pre-#663 code did exactly that, so plan edits had NEVER worked
	// against the live gateway).
	values := url.Values{
		"recurring":       {"edit_plan"},
		"security_key":    {c.SecurityKey},
		"current_plan_id": {planID},
		"plan_amount":     {centsToDollarString(planAmountCents)},
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

// centsToDollarString converts integer cents into a fixed two-decimal dollar
// string as required by NMI's classic plan_amount parameter.
func centsToDollarString(cents moneyutil.Cents) string {
	return string(centsJSONAmount(cents))
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
