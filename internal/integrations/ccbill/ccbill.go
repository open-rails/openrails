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

	FlexID   string `json:"flex_id"`
	FormName string `json:"form_name"`
	UserID   string `json:"user_id"`
	PriceID  string `json:"price_id"`
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
	defaultIFrameWidth  = "100%"
	defaultIFrameHeight = "600px"
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

// GenerateFlexFormURL creates a CCBill FlexForm URL with subscription parameters for iFrame embedding
func (c *CCBillClient) GenerateFlexFormURL(params *GenerateFlexFormURLParams) (*FlexFormResponse, error) {
	if params.Username == "" || params.Email == "" {
		return nil, fmt.Errorf("username and email are required")
	}
	if params.FormName == "" {
		return nil, fmt.Errorf("form name is required")
	}
	if params.FlexID == "" {
		return nil, fmt.Errorf("flex_id is required")
	}

	// Build FlexForm URL parameters
	q := url.Values{
		"clientAccnum": {c.config.ClientAccNum},
		"clientSubacc": {c.config.ClientSubAcc},
		"formName":     {params.FormName},
		"language":     {defaultLanguage},
		"currencyCode": {defaultCurrencyCode},

		// Customer information
		"email":          {params.Email},
		"username":       {params.Username},
		"password":       {params.Password},
		"customer_fname": {params.CustomerFName},
		"customer_lname": {params.CustomerLName},
		"address1":       {params.Address1},
		"city":           {params.City},
		"state":          {params.State},
		"zipcode":        {params.ZipCode},
		"country":        {params.Country},
	}

	// Generate signature if salt is configured
	if c.config.Salt != "" {
		sigInput := url.Values{"username": {params.Username}}
		q.Set("signature", c.generateCCBillSignature(sigInput))
	}

	flexFormURL := fmt.Sprintf("%s/%s?%s", c.flexFormBaseURL, params.FlexID, q.Encode())

	return &FlexFormResponse{RedirectURL: flexFormURL}, nil
}

func (c *CCBillClient) generateCCBillSignature(query url.Values) string {
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

func (c *CCBillClient) Config() *config.CCBillConfig {
	return c.config
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
	if params.Username == "" || params.Email == "" {
		return nil, fmt.Errorf("username and email are required")
	}
	if params.FormName == "" {
		return nil, fmt.Errorf("form name (target price) is required")
	}
	if params.FlexID == "" {
		return nil, fmt.Errorf("flex_id (target price) is required")
	}
	if params.OriginalSubscriptionID == "" {
		return nil, fmt.Errorf("original_subscription_id is required")
	}

	// Build FlexForm URL parameters for upgrade
	// CCBill upgrade forms require the original subscription ID to identify what to upgrade
	q := url.Values{
		"clientAccnum":           {c.config.ClientAccNum},
		"clientSubacc":           {c.config.ClientSubAcc},
		"formName":               {params.FormName},
		"language":               {defaultLanguage},
		"currencyCode":           {defaultCurrencyCode},
		"email":                  {params.Email},
		"username":               {params.Username},
		"originalSubscriptionId": {params.OriginalSubscriptionID},
	}

	// Generate signature if salt is configured
	if c.config.Salt != "" {
		sigInput := url.Values{"username": {params.Username}}
		q.Set("signature", c.generateCCBillSignature(sigInput))
	}

	flexFormURL := fmt.Sprintf("%s/%s?%s", c.flexFormBaseURL, params.FlexID, q.Encode())

	return &FlexFormResponse{RedirectURL: flexFormURL}, nil
}
