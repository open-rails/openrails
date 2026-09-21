package hyperswitch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// ProxyNMI sends one permanent, merchant-owned payment method through the
// operator's exact qualified route. Form values are secret wrapped so neither
// the PSP key nor a future confidential field enters debug output.
func (c *Client) ProxyNMI(ctx context.Context, destination, methodID string, form map[string]Secret) (*nmi.SaleResponse, error) {
	if c == nil || !safeIdentifier(methodID) || len(form) == 0 {
		return nil, ErrBinding
	}
	if c.readOnly {
		return nil, ErrReadOnly
	}
	if err := c.CheckProxyContract(ctx, destination); err != nil {
		return nil, err
	}
	request := struct {
		Body        map[string]Secret `json:"request_body"`
		Request     string            `json:"request_encoding"`
		Response    string            `json:"response_encoding"`
		Destination string            `json:"destination_url"`
		Headers     map[string]string `json:"headers"`
		Method      string            `json:"method"`
		Token       string            `json:"token"`
		TokenType   string            `json:"token_type"`
	}{form, "form_urlencoded", "form_urlencoded", destination, map[string]string{}, http.MethodPost, methodID, "payment_method_id"}
	var response struct {
		Body    map[string]string          `json:"response"`
		Status  int                        `json:"status_code"`
		Headers map[string]json.RawMessage `json:"response_headers"`
	}
	if err := c.call(ctx, http.MethodPost, "/v2/proxy", request, &response); err != nil {
		// Even a proxy 4xx is not a provider nonexecution receipt. Once this
		// request was attempted, no status or missing response permits retry.
		return nil, ErrUnknown
	}
	if response.Status != http.StatusOK || len(response.Headers) != 0 {
		return nil, ErrUnknown
	}
	for key, value := range response.Body {
		switch key {
		case "response", "response_code", "responsetext":
		case "transactionid", "authcode":
			if !safeNMIIdentifier(value, string(form["security_key"])) {
				return nil, ErrUnknown
			}
		default:
			return nil, ErrUnknown
		}
	}
	code, err := strconv.Atoi(response.Body["response_code"])
	if err != nil || len(response.Body["response_code"]) != 3 {
		return nil, ErrUnknown
	}
	switch response.Body["response"] {
	case "1":
		if code != 100 || response.Body["transactionid"] == "" || response.Body["responsetext"] != "Approved" {
			return nil, ErrUnknown
		}
	case "2":
		if code < 200 || code > 299 || response.Body["responsetext"] != "Declined" {
			return nil, ErrUnknown
		}
	case "3":
		if code < 300 || code > 499 || response.Body["responsetext"] != "Gateway error" {
			return nil, ErrUnknown
		}
	default:
		return nil, ErrUnknown
	}
	// Reuse NMI's existing refusal/localization taxonomy, but feed it only
	// the bounded sanitized fields above, never an arbitrary provider body.
	values := make(url.Values, len(response.Body))
	for key, value := range response.Body {
		values.Set(key, value)
	}
	return nmi.ParseSaleResponse(values.Encode())
}

func safeNMIIdentifier(value, providerKey string) bool {
	if len(value) > 128 || cardguard.ContainsPAN(value) || (providerKey != "" && strings.Contains(value, providerKey)) {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
