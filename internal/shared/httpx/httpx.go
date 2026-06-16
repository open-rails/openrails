// Package httpx holds small, dependency-free helpers for talking to external
// HTTP endpoints safely. It is a stdlib-only leaf package so it can be shared by
// any integration without risking an import cycle.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DefaultMaxResponseBytes is a conservative cap for JSON responses from external
// providers whose payloads are small, fixed-shape documents (FX quotes, price
// oracle reads, captcha siteverify results). 1 MiB is far larger than any
// legitimate response while still bounding memory use.
const DefaultMaxResponseBytes int64 = 1 << 20 // 1 MiB

// ErrResponseTooLarge is returned when an upstream body exceeds the cap.
var ErrResponseTooLarge = errors.New("httpx: response body exceeds maximum allowed size")

// DecodeJSONLimited reads at most maxBytes from r, then JSON-decodes the result
// into v. It exists so callers never hand an unbounded io.Reader (such as
// http.Response.Body) straight to json.NewDecoder: a compromised, MITM'd, or
// simply buggy upstream could otherwise stream an arbitrarily large or
// never-ending body and exhaust process memory.
//
// If maxBytes <= 0, DefaultMaxResponseBytes is used. When the body is larger
// than the limit, ErrResponseTooLarge is returned rather than a partial decode.
func DecodeJSONLimited(r io.Reader, maxBytes int64, v any) error {
	if r == nil {
		return errors.New("httpx: nil reader")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}

	// Read one extra byte so we can distinguish "exactly at the limit" from
	// "over the limit" without trusting Content-Length.
	limited := io.LimitReader(r, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("httpx: read response: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return ErrResponseTooLarge
	}

	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("httpx: decode json: %w", err)
	}
	return nil
}
