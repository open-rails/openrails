package basistheory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Ephemeral detokenizing proxy (POST /proxy + BT-PROXY-URL): the caller
// composes the FULL destination request — vault-agnostic by design (#297).
//
// Outcome discrimination (doc-verified, load-bearing):
//   - BT-PROXY-DESTINATION-STATUS header present  => the destination ANSWERED;
//     ProxyForm returns (*ProxyResult, nil) and the caller parses the body.
//   - header absent + RFC-7807 {"proxy_error": …}  => BT failed PRE-FORWARD
//     (bad expression, detokenization failure, auth, >20 tokens): a clean
//     *ProxyError — an engine error, NEVER a decline; safe to retry after fix.
//   - 408 / 5xx without the header / transport failure after send => AMBIGUOUS:
//     BT may have forwarded before dying. TransportAmbiguousError — verify by
//     orderid (#674), never blind-retry, never treat as a decline.
//
// The proxy does NOT support BT-IDEMPOTENCY-KEY (doc-verified).

const (
	proxyURLHeader               = "BT-PROXY-URL"
	ProxyDestinationStatusHeader = "BT-PROXY-DESTINATION-STATUS"
)

// ProxyResult is a destination-answered proxy outcome.
type ProxyResult struct {
	// DestinationStatus is the destination's HTTP status from
	// BT-PROXY-DESTINATION-STATUS. Always > 0 on a returned ProxyResult.
	DestinationStatus int
	// Body is the destination's response body, verbatim.
	Body []byte
}

// ProxyError is BT's pre-forward proxy failure ({"proxy_error": …}). The
// destination was never reached: not a decline, not ambiguous.
type ProxyError struct {
	Status int
	Title  string
	Detail string
	Errors map[string][]string
	Raw    []byte
}

func (e *ProxyError) Error() string {
	msg := strings.TrimSpace(e.Title)
	if d := strings.TrimSpace(e.Detail); d != "" {
		if msg != "" {
			msg += ": "
		}
		msg += d
	}
	if msg == "" {
		msg = strings.TrimSpace(string(e.Raw))
	}
	return fmt.Sprintf("basistheory: proxy failed pre-forward (status %d) %s", e.Status, msg)
}

// IsBTProxyError reports whether err is a clean BT pre-forward proxy failure.
func IsBTProxyError(err error) (*ProxyError, bool) {
	var pe *ProxyError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// TransportAmbiguousError marks a proxy call whose destination MUTATION MAY
// HAVE EXECUTED with no usable answer (mirror of nmi.TransportAmbiguousError).
type TransportAmbiguousError struct{ Err error }

func (e *TransportAmbiguousError) Error() string {
	return "basistheory: proxy outcome unknown (may have forwarded): " + e.Err.Error()
}
func (e *TransportAmbiguousError) Unwrap() error { return e.Err }

func ambiguous(err error) error {
	if err == nil {
		return nil
	}
	return &TransportAmbiguousError{Err: err}
}

// IsTransportAmbiguous reports whether err carries a TransportAmbiguousError.
func IsTransportAmbiguous(err error) bool {
	var t *TransportAmbiguousError
	return errors.As(err, &t)
}

// ProxyForm invokes the ephemeral proxy with a form-urlencoded body (BT's own
// documented NMI DirectPost shape). destinationURL must be HTTPS in production
// (BT enforces it); form values may contain detokenization expressions.
func (c *Client) ProxyForm(ctx context.Context, destinationURL string, form url.Values) (*ProxyResult, error) {
	if c == nil {
		return nil, errors.New("basistheory: client not initialized")
	}
	destinationURL = strings.TrimSpace(destinationURL)
	if destinationURL == "" {
		return nil, errors.New("basistheory: proxy destination url is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/proxy", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set(apiKeyHeader, c.apiKey)
	req.Header.Set(proxyURLHeader, destinationURL)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, ErrProviderReadOnly) {
			return nil, ErrProviderReadOnly
		}
		// Sent (or possibly sent) and no answer: ambiguous by doctrine (#674).
		return nil, ambiguous(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, ambiguous(fmt.Errorf("read proxy response: %w", err))
	}
	return classifyProxyResponse(resp.StatusCode, resp.Header.Get(ProxyDestinationStatusHeader), body)
}

// classifyProxyResponse implements the three-way outcome discrimination.
// Split out pure for the fixture-matrix unit test.
func classifyProxyResponse(httpStatus int, destStatusHeader string, body []byte) (*ProxyResult, error) {
	if ds := strings.TrimSpace(destStatusHeader); ds != "" {
		n, err := strconv.Atoi(ds)
		if err != nil || n <= 0 {
			return nil, ambiguous(fmt.Errorf("unparseable %s header %q", ProxyDestinationStatusHeader, ds))
		}
		return &ProxyResult{DestinationStatus: n, Body: body}, nil
	}
	switch {
	case httpStatus == http.StatusRequestTimeout || httpStatus >= 500:
		// Destination timeout (fixed 25s) or BT-side failure that may have
		// happened after forwarding.
		return nil, ambiguous(fmt.Errorf("proxy returned %d with no destination status", httpStatus))
	default:
		// 4xx (or unexpected 2xx) without a destination status: BT answered
		// cleanly without forwarding.
		var envelope struct {
			ProxyError struct {
				Status int                 `json:"status"`
				Title  string              `json:"title"`
				Detail string              `json:"detail"`
				Errors map[string][]string `json:"errors"`
			} `json:"proxy_error"`
		}
		if json.Unmarshal(body, &envelope) == nil && (envelope.ProxyError.Status != 0 || envelope.ProxyError.Title != "" || len(envelope.ProxyError.Errors) > 0) {
			status := envelope.ProxyError.Status
			if status == 0 {
				status = httpStatus
			}
			return nil, &ProxyError{
				Status: status,
				Title:  envelope.ProxyError.Title,
				Detail: envelope.ProxyError.Detail,
				Errors: envelope.ProxyError.Errors,
				Raw:    body,
			}
		}
		return nil, &ProxyError{Status: httpStatus, Raw: body}
	}
}
