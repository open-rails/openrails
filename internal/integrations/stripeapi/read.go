package stripeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// maxRead bounds one read's body.
const maxRead = 4 << 20

// Get reads one Stripe resource with a PSP's secret key through client, a
// read-only client for every books read, and decodes it into into. Any
// answer but 200 is an error naming the path and status.
func Get(ctx context.Context, client *http.Client, secretKey, path string, query url.Values, into any) error {
	target := APIBase + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRead))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stripe: GET %s answered %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("stripe: parse %s: %w", path, err)
	}
	return nil
}
