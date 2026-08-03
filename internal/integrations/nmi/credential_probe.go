package nmi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ProbeCredentials verifies the security key through a bounded, read-only
// transaction query. It never creates or changes provider state.
func (c *NMIClient) ProbeCredentials(ctx context.Context) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	raw, err := c.sendQueryRequestWithContext(ctx, url.Values{
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
		return errors.New("nmi credential probe: provider rejected credentials")
	}
	return nil
}
