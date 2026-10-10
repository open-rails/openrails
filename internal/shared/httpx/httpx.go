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

// DefaultMaxResponseBytes caps the small fixed-shape JSON responses of external
// providers (FX quotes, oracle reads, captcha siteverify).
const DefaultMaxResponseBytes int64 = 1 << 20 // 1 MiB

// ErrResponseTooLarge is returned when an upstream body exceeds the cap.
var ErrResponseTooLarge = errors.New("httpx: response body exceeds maximum allowed size")

// DecodeJSONLimited JSON-decodes at most maxBytes of r into v, so an upstream
// cannot exhaust memory with an unbounded body. maxBytes <= 0 means
// DefaultMaxResponseBytes; a larger body is ErrResponseTooLarge, never a
// partial decode.
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
