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
)

const (
	DefaultDirectPostURL = "https://secure.networkmerchants.com/api/transact.php"
	DefaultQueryAPIURL   = "https://secure.nmi.com/api/query.php"

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
	TestMode      bool
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

func NewClient(provider string, cfg *config.NMIProviderSettings, testMode bool) (*NMIClient, error) {
	if cfg == nil {
		return nil, errors.New("nmi provider configuration is required")
	}

	webhookSecret := strings.TrimSpace(cfg.WebhookSecret)
	if webhookSecret == "" {
		log.WithField("provider", provider).Warn("NMI webhook secret not configured - webhooks will be rejected")
	}

	securityKey := strings.TrimSpace(cfg.SecurityKey)
	if !testMode && securityKey == "" {
		return nil, fmt.Errorf("nmi provider '%s' security key is required in production mode", provider)
	}
	if testMode && securityKey == "" {
		log.WithField("provider", provider).Warn("NMI security_key not configured - NMI API calls will be disabled")
	}

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

func (c *NMIClient) isConfigured() bool {
	return c.SecurityKey != ""
}

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

func parseDirectResponse(response string) (url.Values, error) {
	output, err := url.ParseQuery(response)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %s", response)
	}
	return output, nil
}

func isDirectResponseApproved(output url.Values) bool {
	return strings.TrimSpace(output.Get("response")) == "1"
}

func responseText(output url.Values, fallback string) string {
	if text := strings.TrimSpace(output.Get("responsetext")); text != "" {
		return text
	}
	if text := strings.TrimSpace(output.Get("response_message")); text != "" {
		return text
	}
	return fallback
}

func (c *NMIClient) sendDirectRequest(data url.Values) (_ string, err error) {
	requestType := strings.TrimSpace(data.Get("type"))

	resp, err := http.PostForm(c.DirectPostURL, data)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"provider":     c.providerName,
			"request_type": requestType,
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
