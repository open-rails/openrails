package ccbill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/config"
)

// GenerateFlexFormURLParams contains parameters for generating CCBill FlexForm
// URLs for subscription payments. Address1, City, and State are optional and
// are omitted from the provider URL when empty.
type GenerateFlexFormURLParams struct {
	Username      string `json:"username"`
	Email         string `json:"email"`
	Password      string `json:"password"`
	CustomerFName string `json:"customer_fname"`
	CustomerLName string `json:"customer_lname"`
	Address1      string `json:"address1"`
	City          string `json:"city"`
	State         string `json:"state"`
	ZipCode       string `json:"zipcode"`
	Country       string `json:"country"`
	FlexID        string `json:"flex_id"`
	FormName      string `json:"form_name"`
	ReservationID string `json:"reservation_id"`
	// Currency is the ISO-4217 alpha-3 currency of the PRICE being sold (e.g.
	// "eur"). Required — it decides the `currencyCode` CCBill bills in (#819).
	Currency string `json:"currency"`
}

// FlexFormResponse contains the hosted checkout URL for CCBill.
type FlexFormResponse struct {
	RedirectURL string `json:"redirect_url"`
}

type CCBillClient struct {
	config          *config.CCBillConfig
	flexFormBaseURL string
}

func requireConfig(cfg *config.CCBillConfig) *config.CCBillConfig {
	if cfg == nil {
		panic("ccbill config is required")
	}
	return cfg
}

const (
	sandboxFlexFormBase = "https://sandbox-api.ccbill.com/wap-frontflex/flexforms"
	prodFlexFormBase    = "https://api.ccbill.com/wap-frontflex/flexforms"
	defaultLanguage     = "English"
)

// NewClient creates a new CCBill client.
// testMode: when true, uses sandbox-api.ccbill.com; when false, uses api.ccbill.com.
// Note: The testMode param should come from config.IsTestMode().
func NewClient(cfg *config.CCBillConfig, testMode bool) *CCBillClient {
	cfg = requireConfig(cfg)

	baseURL := prodFlexFormBase
	if testMode {
		baseURL = sandboxFlexFormBase
	}

	return &CCBillClient{
		config:          cfg,
		flexFormBaseURL: strings.TrimRight(baseURL, "/"),
	}
}

// GenerateFlexFormURL creates a CCBill FlexForm URL with subscription parameters for iFrame embedding.
func (c *CCBillClient) GenerateFlexFormURL(params *GenerateFlexFormURLParams) (*FlexFormResponse, error) {
	if err := validateFlexFormIdentity(params.Username, params.Email, params.FormName, params.FlexID); err != nil {
		return nil, err
	}
	currencyCode, err := CurrencyCode(params.Currency)
	if err != nil {
		return nil, err
	}

	q := c.baseFlexFormQuery(params.Username, params.Email, params.FormName, currencyCode)
	q.Set("password", params.Password)
	q.Set("customer_fname", params.CustomerFName)
	q.Set("customer_lname", params.CustomerLName)
	setOptional(q, "address1", params.Address1)
	setOptional(q, "city", params.City)
	setOptional(q, "state", params.State)
	q.Set("zipcode", strings.TrimSpace(params.ZipCode))
	q.Set("country", strings.TrimSpace(params.Country))
	if reservationID := strings.TrimSpace(params.ReservationID); reservationID != "" {
		q.Set("reservationId", reservationID)
	}

	return c.flexFormResponse(params.FlexID, q), nil
}

func setOptional(query url.Values, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		query.Set(key, value)
	}
}

func (c *CCBillClient) computeSignature(query url.Values) string {
	hash := sha256.Sum256([]byte(c.createSignatureInput(query)))
	return hex.EncodeToString(hash[:])
}

// createSignatureInput is the OUTBOUND FlexForm signature input only. It binds
// nothing but the username, and the resulting value is handed to the customer's
// browser in the redirect URL — it is not, and can never be, an inbound
// callback authenticity check (SEC-19 deleted the VerifyCallbackSignature that
// pretended otherwise). Inbound CCBill callbacks authenticate by source IP plus
// the armed clientAccnum/clientSubacc match.
func (c *CCBillClient) createSignatureInput(params url.Values) string {
	return params.Get("username") + c.config.Salt
}

// GenerateUpgradeFlexFormURLParams contains parameters for generating CCBill upgrade FlexForm URLs
type GenerateUpgradeFlexFormURLParams struct {
	// Customer identity
	Username string `json:"username"`
	Email    string `json:"email"`

	// The new pricing tier to upgrade to
	FlexID   string `json:"flex_id"`
	FormName string `json:"form_name"`
	// Currency is the ISO-4217 alpha-3 currency of the TARGET price (#819).
	Currency string `json:"currency"`

	// The existing CCBill subscription ID to upgrade
	OriginalSubscriptionID string `json:"original_subscription_id"`
}

// GenerateUpgradeFlexFormURL creates a CCBill FlexForm URL for upgrading an existing subscription
// This allows users to change their subscription tier (upgrade or downgrade)
func (c *CCBillClient) GenerateUpgradeFlexFormURL(params *GenerateUpgradeFlexFormURLParams) (*FlexFormResponse, error) {
	if err := validateFlexFormIdentity(params.Username, params.Email, params.FormName, params.FlexID); err != nil {
		return nil, err
	}
	if params.OriginalSubscriptionID == "" {
		return nil, fmt.Errorf("original_subscription_id is required")
	}
	currencyCode, err := CurrencyCode(params.Currency)
	if err != nil {
		return nil, err
	}

	q := c.baseFlexFormQuery(params.Username, params.Email, params.FormName, currencyCode)
	q.Set("originalSubscriptionId", params.OriginalSubscriptionID)

	return c.flexFormResponse(params.FlexID, q), nil
}

func validateFlexFormIdentity(username, email, formName, flexID string) error {
	if username == "" || email == "" {
		return fmt.Errorf("username and email are required")
	}
	if formName == "" {
		return fmt.Errorf("form name is required")
	}
	if flexID == "" {
		return fmt.Errorf("flex_id is required")
	}
	return nil
}

// baseFlexFormQuery takes the ISO-4217 NUMERIC currencyCode from CurrencyCode —
// never a literal, so the billed currency is always the price's currency.
func (c *CCBillClient) baseFlexFormQuery(username, email, formName, currencyCode string) url.Values {
	q := url.Values{
		"clientAccnum": {c.config.ClientAccNum},
		"clientSubacc": {c.config.ClientSubAcc},
		"formName":     {formName},
		"language":     {defaultLanguage},
		"currencyCode": {currencyCode},
		"email":        {email},
		"username":     {username},
	}
	if c.config.Salt != "" {
		q.Set("signature", c.computeSignature(url.Values{"username": {username}}))
	}
	return q
}

func (c *CCBillClient) flexFormResponse(flexID string, query url.Values) *FlexFormResponse {
	return &FlexFormResponse{RedirectURL: fmt.Sprintf("%s/%s?%s", c.flexFormBaseURL, flexID, query.Encode())}
}
