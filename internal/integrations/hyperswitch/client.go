// Package hyperswitch is the token-only adapter to the operator's fixed custody
// deployment. It never requests raw card data and never submits a card number.
package hyperswitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/cardguard"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotFound    = errors.New("hyperswitch resource not found")
	ErrUnavailable = errors.New("hyperswitch request unavailable or unqualified")
	ErrUnknown     = errors.New("hyperswitch mutation outcome unknown")
	ErrBinding     = errors.New("hyperswitch resource binding mismatch")
	ErrReadOnly    = errors.New("hyperswitch provider writes are disabled")
)

// Secret is intentionally redacted from fmt/debug output. JSON decoding is
// permitted only into this private adapter, not into provider-evidence blobs.
type Secret string

func (Secret) String() string   { return "[redacted]" }
func (Secret) GoString() string { return "[redacted]" }

type Config struct {
	BaseURL, MerchantID, ProfileID string
	APIKey                         Secret
	ReadOnly                       bool
}
type Client struct {
	base                  string
	merchantID, profileID string
	key                   Secret
	readOnly              bool
	http                  *http.Client
}

func (*Client) String() string { return "HyperSwitch scoped client" }

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || cfg.MerchantID == "" || cfg.ProfileID == "" || cfg.APIKey == "" {
		return nil, ErrBinding
	}
	return &Client{base: strings.TrimRight(cfg.BaseURL, "/"), merchantID: cfg.MerchantID, profileID: cfg.ProfileID, key: cfg.APIKey, readOnly: cfg.ReadOnly, http: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) call(ctx context.Context, method, path string, input, output any) error {
	if c.readOnly && method != http.MethodGet {
		return ErrReadOnly
	}
	var data []byte
	var err error
	if input != nil {
		data, err = json.Marshal(input)
		if err != nil {
			return ErrBinding
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return ErrBinding
	}
	request.Header.Set("Authorization", "api-key="+string(c.key))
	request.Header.Set("x-profile-id", c.profileID)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		if method != http.MethodGet {
			return ErrUnknown
		}
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if method != http.MethodGet && response.StatusCode >= 500 {
			return ErrUnknown
		}
		return ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(raw) > 65536 {
		if method != http.MethodGet {
			return ErrUnknown
		}
		return ErrUnavailable
	}
	if err = json.Unmarshal(raw, output); err != nil {
		if method != http.MethodGet {
			return ErrUnknown
		}
		return ErrUnavailable
	}
	return nil
}

type Customer struct {
	ID        string `json:"id"`
	Reference string `json:"merchant_reference_id"`
}

// EnsureCustomer recovers by the exact authenticated merchant reference even
// after a lost create reply or a concurrent creator. There is no list fallback.
func (c *Client) EnsureCustomer(ctx context.Context, reference, name, email string) (Customer, error) {
	if reference == "" || name == "" || email == "" {
		return Customer{}, ErrBinding
	}
	read := func() (Customer, error) {
		var out Customer
		err := c.call(ctx, http.MethodGet, "/v2/customers/reference/"+url.PathEscape(reference), nil, &out)
		if err == nil && (out.ID == "" || out.Reference != reference) {
			err = ErrBinding
		}
		return out, err
	}
	out, err := read()
	if err == nil || !errors.Is(err, ErrNotFound) {
		return out, err
	}
	err = c.call(ctx, http.MethodPost, "/v2/customers", map[string]string{"merchant_reference_id": reference, "name": name, "email": email}, &out)
	if err != nil {
		recovered, readErr := read()
		if readErr == nil {
			return recovered, nil
		}
		return Customer{}, err
	}
	if out.ID == "" || out.Reference != reference {
		return Customer{}, ErrBinding
	}
	return out, nil
}

type Instant struct{ time.Time }

func (i *Instant) UnmarshalJSON(raw []byte) error {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ErrBinding
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil { // The pinned vendor's PrimitiveDateTime serializer is UTC without an offset.
		parsed, err = time.ParseInLocation("2006-01-02T15:04:05.999999999", value, time.UTC)
	}
	if err != nil || parsed.IsZero() {
		return ErrBinding
	}
	i.Time = parsed.UTC()
	return nil
}

type AssociatedMethod struct {
	Token struct {
		Type string `json:"type"`
		Data string `json:"data"`
	} `json:"payment_method_token"`
}
type Session struct {
	ID                string             `json:"id"`
	CustomerID        string             `json:"customer_id"`
	StorageType       string             `json:"storage_type"`
	ExpiresAt         Instant            `json:"expires_at"`
	ClientSecret      Secret             `json:"client_secret"`
	SDKAuthorization  Secret             `json:"sdk_authorization"`
	AssociatedMethods []AssociatedMethod `json:"associated_payment_methods"`
}

func (Session) String() string   { return "HyperSwitch capture session" }
func (Session) GoString() string { return "HyperSwitch capture session" }
func (s Session) OwnsToken(token string) bool {
	return token != "" && len(s.AssociatedMethods) == 1 && s.AssociatedMethods[0].Token.Type == "payment_method_session_token" && s.AssociatedMethods[0].Token.Data == token
}
func (c *Client) CreateSession(ctx context.Context, customer string) (Session, error) {
	if customer == "" {
		return Session{}, ErrBinding
	}
	var out Session
	err := c.call(ctx, http.MethodPost, "/v2/payment-method-sessions", map[string]any{"customer_id": customer, "storage_type": "persistent", "expires_in": 900}, &out)
	if err == nil && (out.ID == "" || out.CustomerID != customer || out.StorageType != "persistent" || out.ClientSecret == "" || out.SDKAuthorization == "" || out.ExpiresAt.IsZero()) {
		err = ErrBinding
	}
	return out, err
}
func (c *Client) GetSession(ctx context.Context, id, customer string) (Session, error) {
	if id == "" || customer == "" {
		return Session{}, ErrBinding
	}
	var out Session
	err := c.call(ctx, http.MethodGet, "/v2/payment-method-sessions/"+url.PathEscape(id), nil, &out)
	if err == nil && (out.ID != id || out.CustomerID != customer || out.StorageType != "persistent" || out.ExpiresAt.IsZero()) {
		err = ErrBinding
	}
	// GET deliberately returns a redacted marker; it cannot refresh/reissue the
	// accepted session secret. Never mistake it for a usable browser credential.
	out.ClientSecret = ""
	out.SDKAuthorization = ""
	return out, err
}

type forbiddenRaw struct{}

func (*forbiddenRaw) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return ErrBinding
}

type Method struct {
	ID          string       `json:"id"`
	MerchantID  string       `json:"merchant_id"`
	CustomerID  string       `json:"customer_id"`
	StorageType string       `json:"storage_type"`
	Raw         forbiddenRaw `json:"raw_payment_method_data"`
	Data        struct {
		Card *struct {
			Last4 string       `json:"last4_digits"`
			Month string       `json:"expiry_month"`
			Year  string       `json:"expiry_year"`
			Brand string       `json:"card_network"`
			Raw   forbiddenRaw `json:"card_number"`
		} `json:"card"`
	} `json:"payment_method_data"`
}

func (c *Client) GetMethod(ctx context.Context, token, customer string) (Method, error) {
	if token == "" || customer == "" {
		return Method{}, ErrBinding
	}
	var out Method
	err := c.call(ctx, http.MethodGet, "/v2/payment-methods/"+url.PathEscape(token)+"?fetch_raw_detail=false&force_sync=false", nil, &out)
	if err == nil && (!safeIdentifier(out.ID) || out.MerchantID != c.merchantID || out.CustomerID != customer || out.StorageType != "persistent" || !out.validMaskedCard()) {
		err = ErrBinding
	}
	return out, err
}

// Identifiers are opaque; only bounded printable text without card material is
// allowed to cross this masked metadata boundary.
func safeIdentifier(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value || cardguard.ContainsPAN(value) {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
func (m Method) validMaskedCard() bool {
	card := m.Data.Card
	if card == nil || len(card.Last4) != 4 || len(card.Month) < 1 || len(card.Month) > 2 || len(card.Year) != 4 {
		return false
	}
	for _, value := range []string{card.Last4, card.Month, card.Year} {
		for _, r := range value {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	month, _ := strconv.Atoi(card.Month)
	year, _ := strconv.Atoi(card.Year)
	return month >= 1 && month <= 12 && year >= 2000 && safeIdentifier(card.Brand)
}
func (m Method) MaskedExpiry() string {
	month, _ := strconv.Atoi(m.Data.Card.Month)
	return fmt.Sprintf("%02d/%s", month, m.Data.Card.Year[2:])
}

// The single patched deployment contract covers the SDK resource binding and
// proxy boundaries. Capture must not issue authority against stock v2 either.
type proxyContract struct {
	Contract         string `json:"contract"`
	Strict           bool   `json:"strict"`
	MaxResponseBytes int    `json:"max_response_bytes"`
	Routes           []struct {
		Destination string `json:"destination_url"`
		Method      string `json:"method"`
		Profile     string `json:"response_profile"`
	} `json:"routes"`
}

func (c *Client) readProxyContract(ctx context.Context) (proxyContract, error) {
	var contract proxyContract
	if err := c.call(ctx, http.MethodGet, "/v2/proxy", nil, &contract); err != nil {
		return contract, ErrUnavailable
	}
	if contract.Contract != "openrails-nmi-form-v1" || !contract.Strict || contract.MaxResponseBytes != 65536 || len(contract.Routes) == 0 {
		return contract, ErrUnavailable
	}
	for _, route := range contract.Routes {
		target, err := url.Parse(route.Destination)
		if err != nil || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Scheme != "https" && target.Scheme != "http") || route.Method != http.MethodPost || route.Profile != "nmi_classic" {
			return contract, ErrUnavailable
		}
	}
	return contract, nil
}
func (c *Client) CheckCaptureContract(ctx context.Context) error {
	_, err := c.readProxyContract(ctx)
	return err
}

// CheckProxyContract additionally binds the later payment to the exact route.
func (c *Client) CheckProxyContract(ctx context.Context, destination string) error {
	contract, err := c.readProxyContract(ctx)
	if err != nil {
		return err
	}
	target, err := url.Parse(destination)
	if err != nil || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return ErrBinding
	}
	for _, route := range contract.Routes {
		if route.Destination == target.String() {
			return nil
		}
	}
	return ErrUnavailable
}
