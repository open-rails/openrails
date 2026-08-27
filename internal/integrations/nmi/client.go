package nmi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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
	SecurityKey   string
	WebhookSecret string
	// DirectPostURL survives #663 for the two classic-only recurring ops
	// (add_subscription, rebill_subscription); everything else is v5 JSON.
	DirectPostURL string
	// QueryURL survives #663 for transaction SEARCH only (v5 has no
	// payments list/search; v4's report endpoint is partner-key-only).
	QueryURL  string
	V5BaseURL string
	TestMode  bool
	// ReadOnly blocks EVERY mutation — classic direct-post AND v5 non-GET —
	// with ErrProviderReadOnly; reads stay available. Set when mode=readonly
	// (#346) at client build.
	ReadOnly bool
	// httpClient bounds every gateway call with a timeout so a slow/hung NMI
	// endpoint fails fast instead of blocking the request forever (#363/#367).
	// The default http.DefaultClient used by http.PostForm has NO timeout.
	httpClient *http.Client
}

// Per-request deadlines. Split on the SAME axis the read-only guard and the
// ambiguity classifier already use — mutation vs read — because the cost of a
// timeout differs entirely between the two:
//
//   - A MUTATION that times out is an UNKNOWN outcome (TransportAmbiguousError):
//     it may have moved money, so the caller must go verify at the gateway
//     instead of retrying. Cutting this bound short manufactures ambiguity and
//     buys an expensive verify round-trip, so it stays generous — a card
//     authorization crossing issuer networks legitimately takes double-digit
//     seconds.
//   - A READ that times out is unambiguous: nothing happened, ask again next
//     cycle. Here the scarce resource is the worker slot, not the answer.
//     ProviderRefreshWorker pages through whole rosters, so N stalled pages
//     cost 10s*N rather than 25s*N.
//
// These bound one round-trip. The CALLER's context still wins when it is
// shorter or already cancelled — that is the point of the ctx plumbing: a
// cancelled job aborts an in-flight call instead of burning the full bound.
const (
	nmiMutationTimeout = 25 * time.Second
	nmiReadTimeout     = 10 * time.Second
)

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

var nmiResponseMessages = map[int]string{
	100: "Transaction was approved.",
	200: "Transaction was declined by rail.",
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
	400: "Transaction error returned by rail.",
	410: "Invalid merchant configuration.",
	411: "Merchant account is inactive.",
	420: "Communication error.",
	421: "Communication error with issuer.",
	430: "Duplicate transaction at rail.",
	440: "Rail format error.",
	441: "Invalid transaction information.",
	460: "Rail feature not available.",
	461: "Unsupported card type.",
}

var nmiLocalizationIDs = map[int]string{
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
	430: "duplicate_transaction_at_rail",
	440: "rail_format_error",
	441: "invalid_transaction_information",
	460: "rail_feature_not_available",
	461: "unsupported_card_type",
}

func nmiLocalizationID(code int) string {
	return nmiLocalizationIDs[code]
}

func nmiResponseDetail(code int) string {
	return nmiResponseMessages[code]
}

func NewClient(provider string, cfg *config.NMIProviderSettings, testMode bool) (*NMIClient, error) {
	if cfg == nil {
		return nil, errors.New("nmi provider configuration is required")
	}

	// No construction-time warn for a missing webhook secret: clients are built
	// for catalog ops and credential probes where it is irrelevant (the arm PUT
	// probe fires before the secret write lands, #845). The webhook path itself
	// errors loudly when verification runs without a secret.
	webhookSecret := strings.TrimSpace(cfg.WebhookSecret)

	securityKey := strings.TrimSpace(cfg.SecurityKey)
	if !testMode && securityKey == "" {
		return nil, fmt.Errorf("nmi provider '%s' security key is required in production mode", provider)
	}
	if testMode && securityKey == "" {
		log.WithField("provider", provider).Warn("NMI security_key not configured - NMI API calls will be disabled")
	}

	directPostURL := DefaultDirectPostURL
	queryURL := DefaultQueryAPIURL
	v5BaseURL := DefaultV5BaseURL
	if testMode {
		directPostURL = SandboxDirectPostURL
		queryURL = SandboxQueryAPIURL
		v5BaseURL = SandboxV5BaseURL
	}

	log.WithFields(log.Fields{
		"provider":    provider,
		"test_mode":   testMode,
		"v5":          v5BaseURL,
		"direct_post": directPostURL,
		"query":       queryURL,
	}).Info("NMI endpoint selection")

	return &NMIClient{
		providerName:  provider,
		SecurityKey:   securityKey,
		WebhookSecret: webhookSecret,
		DirectPostURL: directPostURL,
		QueryURL:      queryURL,
		V5BaseURL:     v5BaseURL,
		TestMode:      testMode,
		httpClient: &http.Client{
			// Backstop only; the real bound is the per-request context
			// deadline (nmiMutationTimeout / nmiReadTimeout).
			Timeout: nmiMutationTimeout,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          20,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}, nil
}

// client returns the configured timeout-bounded HTTP client, falling back to a
// sane default if a client was constructed without NewClient.
func (c *NMIClient) client() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return &http.Client{Timeout: nmiMutationTimeout}
}

// newRequest builds the ONE kind of outbound request this package makes: one
// carrying the caller's context, bounded by a deadline chosen from the
// mutation/read axis. Every NMI byte leaves through here (via sendDirectRequest,
// sendQueryRequest or sendV5Request) — there is no other request constructor in
// the package, and the `noctx` linter keeps it that way.
//
// The returned cancel MUST be called by the caller (defer) once the response
// body is fully read; cancelling earlier aborts the body read.
func (c *NMIClient) newRequest(ctx context.Context, method, url string, body io.Reader, mutating bool) (*http.Request, context.CancelFunc, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	bound := nmiReadTimeout
	if mutating {
		bound = nmiMutationTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, bound)
	req, err := http.NewRequestWithContext(rctx, method, url, body)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return req, cancel, nil
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
		LocalizationID: nmiLocalizationID(responseCode),
		Detail:         nmiResponseDetail(responseCode),
		RawResponse:    rawResponse,
	}
}

func newSaleError(rawResponse string, output url.Values) error {
	message := output.Get("response_message")
	if message == "" {
		message = output.Get("responsetext")
	}
	if message == "" {
		message = rawResponse
	}
	message = fmt.Sprintf("sale failed: %s", message)

	responseCode := parseMobiusResponseCode(output)

	return &CustomerVaultError{
		Message:        message,
		ResponseCode:   responseCode,
		LocalizationID: nmiLocalizationID(responseCode),
		Detail:         nmiResponseDetail(responseCode),
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
		// A 200 body arrived but is unreadable: the mutation likely executed.
		return nil, ambiguous(fmt.Errorf("failed to parse response: %s", response))
	}
	// url.ParseQuery accepts arbitrary text as a key, so parsing alone does not
	// prove that NMI returned a usable mutation result. Without the gateway's
	// response discriminator, the charge may have executed and must be verified
	// rather than treated as a clean decline.
	switch strings.TrimSpace(output.Get("response")) {
	case "1", "2", "3":
		return output, nil
	default:
		return nil, ambiguous(fmt.Errorf("direct response carried no valid response code"))
	}
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

func (c *NMIClient) sendDirectRequest(ctx context.Context, data url.Values) (_ string, err error) {
	requestType := strings.TrimSpace(data.Get("type"))

	if c.ReadOnly {
		log.WithFields(log.Fields{
			"provider":     c.providerName,
			"request_type": requestType,
		}).Warn("NMI direct request blocked: provider is read-only (mode=readonly)")
		return "", ErrProviderReadOnly
	}

	// Every direct-post request is a MUTATION: any failure past this point may
	// have executed at the gateway, so it is wrapped transport-ambiguous (#674).
	req, cancel, err := c.newRequest(ctx, http.MethodPost, c.DirectPostURL, strings.NewReader(data.Encode()), true)
	if err != nil {
		return "", fmt.Errorf("build direct request: %w", err)
	}
	defer cancel()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client().Do(req)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"provider":     c.providerName,
			"request_type": requestType,
		}).Warn("NMI direct request failed")
		return "", ambiguous(fmt.Errorf("failed to send request: %w", err))
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
		return "", ambiguous(fmt.Errorf("unexpected status code: %d", resp.StatusCode))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", ambiguous(fmt.Errorf("failed to read response: %w", err))
	}

	return string(body), nil
}

// sendQueryRequest is the transaction SEARCH survivor: a POST on the wire, but
// semantically a READ (no gateway state changes), so it takes the read bound
// and its failures are never wrapped ambiguous.
func (c *NMIClient) sendQueryRequest(ctx context.Context, data url.Values) (_ string, err error) {
	req, cancel, err := c.newRequest(ctx, http.MethodPost, c.QueryURL, strings.NewReader(data.Encode()), false)
	if err != nil {
		return "", fmt.Errorf("build query request: %w", err)
	}
	defer cancel()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client().Do(req)
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
