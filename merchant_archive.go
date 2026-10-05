package openrails

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/archivewire"
)

const merchantBillingArchivePath = "/v1/merchant/billing-archive"

// ExportBillingArchive writes the bound merchant's versioned billing archive.
// Stop source writers for the final cutover export. Authentication identities,
// provider credentials and live jobs are not portable billing records.
//
// The writer may contain incomplete bytes when an error is returned; publish a
// file only after success. The Client verifies the archive's integrity footer,
// and uses the same credential, merchant binding and timeout as other methods.
func (c *Client) ExportBillingArchive(ctx context.Context, dst io.Writer, requestOptions ...RequestOption) error {
	if dst == nil {
		return invalidErr("archive writer is required")
	}
	return c.withHTTPResponse(ctx, http.MethodGet, merchantBillingArchivePath, nil, http.Header{"Accept": {"application/x-ndjson"}}, func(resp *http.Response) error {
		if err := archiveResponseError(resp); err != nil {
			return err
		}
		if _, err := archivewire.CopyVerified(dst, resp.Body); err != nil {
			return fmt.Errorf("%w: incomplete or invalid billing archive: %w", billing.ErrUnreachable, err)
		}
		return nil
	}, requestOptions...)
}

// ImportBillingArchive atomically restores an archive into an empty destination
// with the same merchant UUID. Provision destination authority separately and
// keep destination workers stopped through restoration and reconfiguration.
// No provider call is made. A lost response can be retried with the same archive.
func (c *Client) ImportBillingArchive(ctx context.Context, src io.Reader, requestOptions ...RequestOption) (*billing.BillingArchiveImport, error) {
	if src == nil {
		return nil, invalidErr("archive reader is required")
	}
	var result billing.BillingArchiveImport
	headers := http.Header{"Content-Type": {"application/x-ndjson"}}
	err := c.withHTTPResponse(ctx, http.MethodPost, merchantBillingArchivePath, src, headers, func(resp *http.Response) error {
		if err := archiveResponseError(resp); err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil {
			return fmt.Errorf("%w: read archive receipt: %w", billing.ErrUnreachable, err)
		}
		if len(raw) > 1<<20 {
			return fmt.Errorf("%w: archive receipt exceeds 1 MiB", billing.ErrUnreachable)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		if err := decoder.Decode(&result); err != nil {
			return fmt.Errorf("%w: decode archive receipt: %w", billing.ErrUnreachable, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("%w: archive receipt must contain one JSON value", billing.ErrUnreachable)
		}
		return nil
	}, requestOptions...)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func archiveResponseError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: read archive refusal: %w", billing.ErrUnreachable, err)
	}
	failure := statusErrorFromBody(resp.StatusCode, raw).(*billing.StatusError)
	if failure.RequestID == "" {
		failure.RequestID = resp.Header.Get("X-Request-ID")
	}
	failure.RetryAfter = resp.Header.Get("Retry-After")
	return failure
}
