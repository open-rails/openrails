package nmi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const (
	// Production endpoints (default, can be overridden in config)
	DefaultDirectPostURL = "https://secure.networkmerchants.com/api/transact.php"
	DefaultQueryAPIURL   = "https://secure.nmi.com/api/query.php"

	// Sandbox endpoints (hardcoded, used when test_mode=true)
	SandboxDirectPostURL = "https://sandbox.nmi.com/api/transact.php"
	SandboxQueryAPIURL   = "https://sandbox.nmi.com/api/query.php"
)

type NMIClient struct {
	providerName  string
	config        *config.NMIProviderSettings
	SecurityKey   string
	WebhookSecret string
	DirectPostURL string
	QueryURL      string
	TestMode      bool // true = sandbox endpoints, false = production endpoints
}

type CreateCustomerVaultData struct {
	// Prefer using PaymentToken from Collect.js. If provided, cc fields are ignored.
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
	// StartDate is optional. If set, the subscription won't charge until this date.
	// Format: YYYYMMDD (e.g., "20251220")
	// When set, the first charge happens ON this date (not before).
	StartDate string
}

type QueryFilter struct {
	StartDate   string
	EndDate     string
	Condition   string
	ActionType  string
	PageNumber  int
	ResultLimit int
}

type CreateCustomerVaultResponse struct {
	CustomerVaultID string
}

type AddSubscriptionResponse struct {
	Type           string
	SubscriptionID string
	TransactionID  string
	Authcode       string
}

// SaleParams contains parameters for a one-time sale transaction
type SaleParams struct {
	CustomerVaultID  string
	Amount           int64  // Amount in cents
	Currency         string // e.g., "usd"
	OrderDescription string
	OrderID          string // Optional order reference
}

// SaleResponse contains the response from a sale transaction
type SaleResponse struct {
	TransactionID string
	Authcode      string
	ResponseText  string
}

// RefundParams contains parameters for refunding a settled transaction
type RefundParams struct {
	TransactionID string // Original transaction ID to refund
	Amount        int64  // Amount in cents (0 for full refund)
}

// RefundResponse contains the response from a refund transaction
type RefundResponse struct {
	TransactionID string // The refund transaction ID
	ResponseText  string
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

type CustomerVaultError struct {
	Message        string
	ResponseCode   int
	LocalizationID string
	Detail         string
	RawResponse    string
}

func (e *CustomerVaultError) Error() string {
	extras := []string{}
	if e.Detail != "" {
		extras = append(extras, e.Detail)
	}
	if e.ResponseCode != 0 {
		extras = append(extras, fmt.Sprintf("code: %d", e.ResponseCode))
	}
	if e.LocalizationID != "" {
		extras = append(extras, fmt.Sprintf("localization_id: %s", e.LocalizationID))
	}
	if len(extras) == 0 {
		return e.Message
	}
	return fmt.Sprintf("%s (%s)", e.Message, strings.Join(extras, ", "))
}

var mobiusResponseMessages = map[int]string{
	100: "Transaction was approved.",
	200: "Transaction was declined by processor.",
	201: "Do not honor.",
	202: "Insufficient funds.",
	203: "Over limit.",
	204: "Transaction not allowed.",
	220: "Incorrect payment information.",
	221: "No such card issuer.",
	222: "No card number on file with issuer.",
	223: "Expired card.",
	224: "Invalid expiration date.",
	225: "Invalid card security code.",
	226: "Invalid PIN.",
	240: "Call issuer for further information.",
	250: "Pick up card.",
	251: "Lost card.",
	252: "Stolen card.",
	253: "Fraudulent card.",
	260: "Declined with further instructions available. (See response text)",
	261: "Declined-Stop all recurring payments.",
	262: "Declined-Stop this recurring program.",
	263: "Declined-Update cardholder data available.",
	264: "Declined-Retry in a few days.",
	300: "Transaction was rejected by gateway.",
	400: "Transaction error returned by processor.",
	410: "Invalid merchant configuration.",
	411: "Merchant account is inactive.",
	420: "Communication error.",
	421: "Communication error with issuer.",
	430: "Duplicate transaction at processor.",
	440: "Processor format error.",
	441: "Invalid transaction information.",
	460: "Processor feature not available.",
	461: "Unsupported card type.",
}

var mobiusLocalizationIDs = map[int]string{
	100: "transaction_was_approved",
	200: "transaction_was_declined_by_processor",
	201: "do_not_honor",
	202: "insufficient_funds",
	203: "over_limit",
	204: "transaction_not_allowed",
	220: "incorrect_payment_information",
	221: "no_such_card_issuer",
	222: "no_card_number_on_file_with_issuer",
	223: "expired_card",
	224: "invalid_expiration_date",
	225: "invalid_card_security_code",
	226: "invalid_pin",
	240: "call_issuer_for_further_information",
	250: "pick_up_card",
	251: "lost_card",
	252: "stolen_card",
	253: "fraudulent_card",
	260: "declined_with_further_instructions_available_see_response_text",
	261: "declined_stop_all_recurring_payments",
	262: "declined_stop_this_recurring_program",
	263: "declined_update_cardholder_data_available",
	264: "declined_retry_in_a_few_days",
	300: "transaction_was_rejected_by_gateway",
	400: "transaction_error_returned_by_processor",
	410: "invalid_merchant_configuration",
	411: "merchant_account_is_inactive",
	420: "communication_error",
	421: "communication_error_with_issuer",
	430: "duplicate_transaction_at_processor",
	440: "processor_format_error",
	441: "invalid_transaction_information",
	460: "processor_feature_not_available",
	461: "unsupported_card_type",
}

func mobiusLocalizationID(code int) string {
	return mobiusLocalizationIDs[code]
}

func mobiusResponseDetail(code int) string {
	return mobiusResponseMessages[code]
}

// NewClient creates a new NMI client for a specific provider.
// testMode: when true, uses sandbox.nmi.com endpoints; when false, uses production endpoints.
// Note: The testMode param should come from config.IsTestMode().
func NewClient(provider string, cfg *config.NMIProviderSettings, testMode bool) (*NMIClient, error) {
	if cfg == nil {
		return nil, errors.New("nmi provider configuration is required")
	}

	webhookSecret := strings.TrimSpace(cfg.WebhookSecret)
	if webhookSecret == "" {
		log.WithField("provider", provider).Warn("NMI webhook secret not configured - webhooks will be rejected")
	}

	securityKey := strings.TrimSpace(cfg.SecurityKey)

	// In production mode (testMode=false), security key is required
	if !testMode && securityKey == "" {
		return nil, fmt.Errorf("nmi provider '%s' security key is required in production mode", provider)
	}

	// In test mode, warn if no security key (client will be non-functional)
	if testMode && securityKey == "" {
		log.WithField("provider", provider).Warn("NMI security_key not configured - NMI API calls will be disabled")
	}

	// Endpoint selection:
	// - Prefer explicit config overrides in BOTH test and prod.
	// - Otherwise: test mode falls back to sandbox.nmi.com; prod falls back to secure.* defaults.
	directPostURL := strings.TrimSpace(cfg.DirectPostURL)
	queryURL := strings.TrimSpace(cfg.QueryURL)

	if directPostURL == "" {
		if testMode {
			directPostURL = SandboxDirectPostURL
		} else {
			directPostURL = DefaultDirectPostURL
		}
	}
	if queryURL == "" {
		if testMode {
			queryURL = SandboxQueryAPIURL
		} else {
			queryURL = DefaultQueryAPIURL
		}
	}

	log.WithFields(log.Fields{
		"provider":    provider,
		"test_mode":   testMode,
		"direct_post": directPostURL,
		"query":       queryURL,
	}).Info("NMI endpoint selection")

	return &NMIClient{
		providerName:  provider,
		config:        cfg,
		SecurityKey:   securityKey,
		WebhookSecret: webhookSecret,
		DirectPostURL: directPostURL,
		QueryURL:      queryURL,
		TestMode:      testMode,
	}, nil
}

// Config returns the NMI configuration
func (c *NMIClient) Config() *config.NMIProviderSettings {
	return c.config
}

// isConfigured returns true if the client has valid configuration
func (c *NMIClient) isConfigured() bool {
	return c.SecurityKey != ""
}

// checkConfiguration returns an error if the client is not configured properly
func (c *NMIClient) checkConfiguration() error {
	if !c.isConfigured() {
		return fmt.Errorf("nmi provider '%s' payment processing is not configured - this feature is disabled in development mode", c.providerName)
	}
	return nil
}

func newCustomerVaultError(rawResponse string, output url.Values) error {
	message := output.Get("response_message")
	if message == "" {
		message = output.Get("responsetext")
	}
	if message == "" {
		message = rawResponse
	}
	message = fmt.Sprintf("failed to create customer vault: %s", message)

	responseCode := parseMobiusResponseCode(output)

	return &CustomerVaultError{
		Message:        message,
		ResponseCode:   responseCode,
		LocalizationID: mobiusLocalizationID(responseCode),
		Detail:         mobiusResponseDetail(responseCode),
		RawResponse:    rawResponse,
	}
}

func newAddSubscriptionError(rawResponse string, output url.Values) error {
	message := output.Get("response_message")
	if message == "" {
		message = output.Get("responsetext")
	}
	if message == "" {
		message = rawResponse
	}
	message = fmt.Sprintf("failed to add subscription: %s", message)

	responseCode := parseMobiusResponseCode(output)

	return &CustomerVaultError{
		Message:        message,
		ResponseCode:   responseCode,
		LocalizationID: mobiusLocalizationID(responseCode),
		Detail:         mobiusResponseDetail(responseCode),
		RawResponse:    rawResponse,
	}
}

func parseMobiusResponseCode(output url.Values) int {
	codeStr := strings.TrimSpace(output.Get("response_code"))
	if codeStr == "2" {
		codeStr = "200"
	}

	code, _ := strconv.Atoi(codeStr)
	if code == 0 && strings.TrimSpace(output.Get("response")) == "2" {
		return 200
	}
	return code
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

	output, err := url.ParseQuery(response)
	if err != nil {
		return nil, fmt.Errorf("failed to parse create customer vault response: %w", err)
	}

	if output.Get("response") != "1" {
		return nil, newCustomerVaultError(response, output)
	}

	vaultID := output.Get("customer_vault_id")
	if vaultID == "" {
		return nil, fmt.Errorf("failed to create customer vault: %s", output.Get("response_message"))
	}

	return &CreateCustomerVaultResponse{CustomerVaultID: vaultID}, nil
}

func (c *NMIClient) UpdateCustomerVault(data UpdateCustomerVaultData) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}

	if data.CustomerVaultID == "" {
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

	if !strings.Contains(response, "response=1") {
		return fmt.Errorf("failed to update customer vault: %s", response)
	}

	return nil
}

func (c *NMIClient) DeleteCustomerVault(data DeleteCustomerVaultData) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}

	if data.CustomerVaultID == "" {
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

	if !strings.Contains(response, "response=1") {
		return fmt.Errorf("failed to delete customer vault: %s", response)
	}

	return nil
}

// RunSale executes a one-time sale against a stored customer vault (payment method).
// This is used for one-time purchases that don't create a recurring subscription.
func (c *NMIClient) RunSale(params SaleParams) (*SaleResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}

	if params.CustomerVaultID == "" {
		return nil, errors.New("customer vault ID is required")
	}

	if params.Amount <= 0 {
		return nil, errors.New("amount must be greater than 0")
	}

	// Convert cents to dollars for NMI API (NMI expects decimal format)
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

	output, err := url.ParseQuery(response)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %s", response)
	}

	if output.Get("response") != "1" {
		return nil, fmt.Errorf("sale failed: %s", output.Get("responsetext"))
	}

	return &SaleResponse{
		TransactionID: output.Get("transactionid"),
		Authcode:      output.Get("authcode"),
		ResponseText:  output.Get("responsetext"),
	}, nil
}

// Refund issues a refund for a previously settled transaction.
// If Amount is 0, the full original amount is refunded.
// Note: Unsettled transactions should be voided instead (use Void method).
func (c *NMIClient) Refund(params RefundParams) (*RefundResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}

	if params.TransactionID == "" {
		return nil, errors.New("transaction ID is required")
	}

	values := url.Values{
		"type":          {"refund"},
		"security_key":  {c.SecurityKey},
		"transactionid": {params.TransactionID},
	}

	// If amount is specified, convert cents to dollars for NMI API
	if params.Amount > 0 {
		amountStr := moneyutil.FormatCentsDecimal(params.Amount)
		values.Set("amount", amountStr)
	}

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return nil, err
	}

	output, err := url.ParseQuery(response)
	if err != nil {
		return nil, fmt.Errorf("failed to parse refund response: %s", response)
	}

	if output.Get("response") != "1" {
		responseCode := parseMobiusResponseCode(output)
		return nil, &CustomerVaultError{
			Message:        fmt.Sprintf("refund failed: %s", output.Get("responsetext")),
			ResponseCode:   responseCode,
			LocalizationID: mobiusLocalizationID(responseCode),
			Detail:         mobiusResponseDetail(responseCode),
			RawResponse:    response,
		}
	}

	return &RefundResponse{
		TransactionID: output.Get("transactionid"),
		ResponseText:  output.Get("responsetext"),
	}, nil
}

// Void cancels an unsettled transaction (same day, before batch closes).
// For settled transactions, use Refund instead.
func (c *NMIClient) Void(transactionID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}

	if transactionID == "" {
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

	output, err := url.ParseQuery(response)
	if err != nil {
		return fmt.Errorf("failed to parse void response: %s", response)
	}

	if output.Get("response") != "1" {
		return fmt.Errorf("void failed: %s", output.Get("responsetext"))
	}

	return nil
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

	// Optional: delayed start date for the subscription
	// When set, the first charge happens ON this date (not before)
	if trimmed := strings.TrimSpace(data.StartDate); trimmed != "" {
		values.Set("start_date", trimmed)
	}

	response, err := c.sendDirectRequest(values)
	if err != nil {
		return nil, err
	}

	output, err := url.ParseQuery(response)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %s", response)
	}

	if output.Get("response") != "1" {
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

	if subscriptionID == "" || planAmount == "" {
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

	if !strings.Contains(response, "response=1") {
		return "", fmt.Errorf("failed to update subscription: %s", response)
	}

	return response, nil
}

// UpdateSubscriptionPaymentSource changes which customer vault (payment method) a subscription uses.
// This allows users to switch to a different stored card for their recurring subscription.
// NMI API: recurring=update_subscription, subscription_id=X, customer_vault_id=Y
func (c *NMIClient) UpdateSubscriptionPaymentSource(subscriptionID, customerVaultID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}

	if subscriptionID == "" {
		return errors.New("subscription ID is required")
	}
	if customerVaultID == "" {
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

	output, err := url.ParseQuery(response)
	if err != nil {
		return fmt.Errorf("failed to parse response: %s", response)
	}

	if output.Get("response") != "1" {
		return fmt.Errorf("failed to update subscription payment source: %s", output.Get("responsetext"))
	}

	return nil
}

func (c *NMIClient) DeleteRecurringSubscription(subscriptionID string) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}

	if subscriptionID == "" {
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

	if !strings.Contains(response, "response=1") {
		return fmt.Errorf("failed to delete subscription: %s", response)
	}

	return nil
}

func (c *NMIClient) AttemptManualRebill(params ManualRebillParams) (*ManualRebillResponse, error) {
	if err := c.checkConfiguration(); err != nil {
		return &ManualRebillResponse{
			Success:      false,
			ErrorMessage: err.Error(),
		}, err
	}

	if params.VaultID == "" || params.BillingID == "" || params.SubscriptionID == "" {
		err := errors.New("vault ID, billing ID, and subscription ID are required")
		return &ManualRebillResponse{
			Success:      false,
			ErrorMessage: err.Error(),
		}, err
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
		return &ManualRebillResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("request failed: %s", err.Error()),
		}, err
	}

	output, err := url.ParseQuery(response)
	if err != nil {
		return &ManualRebillResponse{
			Success:      false,
			ErrorMessage: fmt.Sprintf("failed to parse response: %s", response),
		}, err
	}

	if output.Get("response") == "1" {
		return &ManualRebillResponse{
			Success:       true,
			TransactionID: output.Get("transactionid"),
		}, nil
	}

	// Rebill failed
	errorMessage := output.Get("responsetext")
	if errorMessage == "" {
		errorMessage = "Unknown error"
	}

	return &ManualRebillResponse{
		Success:      false,
		ErrorMessage: errorMessage,
	}, nil
}

func (c *NMIClient) GetTransactionDetails(transactionID string) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}

	if transactionID == "" {
		return "", errors.New("transactionID is required")
	}

	values := url.Values{
		"Servicert_type": {"transaction"},
		"security_key":   {c.SecurityKey},
		"transaction_id": {transactionID},
	}

	response, err := c.sendQueryRequest(values)
	if err != nil {
		return "", err
	}

	return response, nil
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

	response, err := c.sendQueryRequest(values)
	if err != nil {
		return "", err
	}

	return response, nil
}

func (c *NMIClient) GetSubscriptionData(subscriptionID string) (string, error) {
	return c.QueryRecurringSubscriptions(RecurringQueryParams{
		SubscriptionID: subscriptionID,
	})
}

func (c *NMIClient) GetRecurringPlanData() (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}

	values := url.Values{
		"Servicert_type": {"recurring_plans"},
		"security_key":   {c.SecurityKey},
	}

	response, err := c.sendQueryRequest(values)
	if err != nil {
		return "", err
	}

	return response, nil
}

type RecurringQueryParams struct {
	SubscriptionID string
	ResultLimit    int
	PageNumber     int
	ResultOrder    string
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

	response, err := c.sendQueryRequest(values)
	if err != nil {
		return "", err
	}

	return response, nil
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

	response, err := c.sendQueryRequest(values)
	if err != nil {
		return "", err
	}

	return response, nil
}
func (c *NMIClient) sendDirectRequest(data url.Values) (_ string, err error) {
	action := strings.TrimSpace(data.Get("action"))
	requestType := strings.TrimSpace(data.Get("type"))

	resp, err := http.PostForm(c.DirectPostURL, data)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"provider":     c.providerName,
			"request_type": requestType,
			"action":       action,
		}).Warn("NMI direct request failed")
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer func() {
		cerr := resp.Body.Close()
		if err == nil {
			err = cerr
		}
	}()

	if resp.StatusCode != http.StatusOK {
		log.WithFields(log.Fields{
			"provider":     c.providerName,
			"request_type": requestType,
			"action":       action,
			"status_code":  resp.StatusCode,
		}).Warn("NMI direct request returned non-200 status")
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(body), nil
}

func (c *NMIClient) sendQueryRequest(data url.Values) (_ string, err error) {
	resp, err := http.PostForm(c.QueryURL, data)
	if err != nil {
		return "", fmt.Errorf("failed to send query request: %w", err)
	}
	defer func() {
		cerr := resp.Body.Close()
		if err == nil {
			err = cerr
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read query response: %w", err)
	}

	return string(body), nil
}

func (c *NMIClient) GetWebhookSecret() string {
	return c.WebhookSecret
}
