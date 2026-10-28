package nmi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrCredentialsRejected: the gateway answered and refused the security key
// (a v5 401 or 403, or a Query API authentication error_response).
var ErrCredentialsRejected = errors.New("provider rejected credentials")

// queryError is a Query API error_response as an error: one naming
// authentication is the gateway refusing the security key.
func queryError(report, msg string) error {
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "authentic") || strings.Contains(lower, "security key") {
		return fmt.Errorf("%s error_response: %s: %w", report, msg, ErrCredentialsRejected)
	}
	return fmt.Errorf("%s error_response: %s", report, msg)
}

// ProbeCredentials verifies the security key through a bounded, read-only
// transaction query. It never creates or changes provider state.
func (c *NMIClient) ProbeCredentials(ctx context.Context) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	raw, err := c.sendQueryRequest(ctx, url.Values{
		"report_type":  {"transaction"},
		"result_limit": {"1"},
		"security_key": {c.SecurityKey},
	})
	if err != nil {
		return fmt.Errorf("nmi credential probe: %w", err)
	}
	var response struct {
		XMLName       xml.Name `xml:"nm_response"`
		ErrorResponse string   `xml:"error_response"`
	}
	if err := xml.Unmarshal([]byte(raw), &response); err != nil {
		return fmt.Errorf("nmi credential probe: parse response: %w", err)
	}
	if strings.TrimSpace(response.ErrorResponse) != "" {
		return fmt.Errorf("nmi credential probe: %w", ErrCredentialsRejected)
	}
	return nil
}
