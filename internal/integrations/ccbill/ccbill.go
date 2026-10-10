package ccbill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/config"
)

// GenerateFlexFormURLParams are the inputs of a FlexForm subscription URL.
// Empty Address1, City and State are omitted from it.
type GenerateFlexFormURLParams struct {
	Username      string `json:"username"`
	Email         string `json:"email"`
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
	// Currency is the ISO-4217 alpha-3 currency of the price sold (e.g. "eur").
	// Required: it decides the `currencyCode` CCBill bills in.
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

// ErrMissingSalt refuses a FlexForm client without its signing salt.
var ErrMissingSalt = errors.New("ccbill salt is required to sign FlexForm links")

// NewClient creates a CCBill client; testMode (config.IsTestMode) selects
// sandbox-api.ccbill.com over api.ccbill.com.
func NewClient(cfg *config.CCBillConfig, testMode bool) (*CCBillClient, error) {
	cfg = requireConfig(cfg)
	if strings.TrimSpace(cfg.Salt) == "" {
		return nil, ErrMissingSalt
	}

	baseURL := prodFlexFormBase
	if testMode {
		baseURL = sandboxFlexFormBase
	}

	return &CCBillClient{
		config:          cfg,
		flexFormBaseURL: strings.TrimRight(baseURL, "/"),
	}, nil
}

// GenerateFlexFormURL builds a signed FlexForm subscription checkout URL.
func (c *CCBillClient) GenerateFlexFormURL(params *GenerateFlexFormURLParams) (*FlexFormResponse, error) {
	if err := validateFlexFormIdentity(params.Username, params.Email, params.FormName, params.FlexID); err != nil {
		return nil, err
	}
	currencyCode, err := CurrencyCode(params.Currency)
	if err != nil {
		return nil, err
	}

	q := c.baseFlexFormQuery(params.Username, params.Email, params.FormName, currencyCode)
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

// createSignatureInput is the outbound FlexForm signature input only: it binds
// just the username and goes to the browser, so it can never authenticate an
// inbound callback. Callbacks authenticate by source IP plus the armed
// clientAccnum/clientSubacc match.
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
	// Currency is the ISO-4217 alpha-3 currency of the target price.
	Currency string `json:"currency"`

	// The existing CCBill subscription ID to upgrade
	OriginalSubscriptionID string `json:"original_subscription_id"`
}

// GenerateUpgradeFlexFormURL builds a FlexForm URL that moves an existing
// subscription to another tier (up or down).
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
	q.Set("signature", c.computeSignature(url.Values{"username": {username}}))
	return q
}

func (c *CCBillClient) flexFormResponse(flexID string, query url.Values) *FlexFormResponse {
	return &FlexFormResponse{RedirectURL: fmt.Sprintf("%s/%s?%s", c.flexFormBaseURL, flexID, query.Encode())}
}
