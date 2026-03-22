package ccbill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/config"
)

// GenerateFlexFormURLParams contains parameters for generating CCBill FlexForm URLs for subscription payments
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
	defaultCurrencyCode = "840" // USD
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

	q := c.baseFlexFormQuery(params.Username, params.Email, params.FormName)
	q.Set("password", params.Password)
	q.Set("customer_fname", params.CustomerFName)
	q.Set("customer_lname", params.CustomerLName)
	q.Set("address1", params.Address1)
	q.Set("city", params.City)
	q.Set("state", params.State)
	q.Set("zipcode", params.ZipCode)
	q.Set("country", params.Country)

	return c.flexFormResponse(params.FlexID, q), nil
}

func (c *CCBillClient) computeSignature(query url.Values) string {
	hash := sha256.Sum256([]byte(c.createSignatureInput(query)))
	return hex.EncodeToString(hash[:])
}

func (c *CCBillClient) VerifyCallbackSignature(params url.Values) bool {
	signature := params.Get("X-signature")

	hash := sha256.Sum256([]byte(c.createSignatureInput(params)))
	return hex.EncodeToString(hash[:]) == strings.ToLower(signature)
}

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

	q := c.baseFlexFormQuery(params.Username, params.Email, params.FormName)
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

func (c *CCBillClient) baseFlexFormQuery(username, email, formName string) url.Values {
	q := url.Values{
		"clientAccnum": {c.config.ClientAccNum},
		"clientSubacc": {c.config.ClientSubAcc},
		"formName":     {formName},
		"language":     {defaultLanguage},
		"currencyCode": {defaultCurrencyCode},
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
