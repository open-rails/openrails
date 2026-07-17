package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/pkg/merchant"
)

// This file adds the client-facing Idempotency-Key HTTP replay middleware
// (#579): the standard Idempotency-Key header lets a retried mutating request
// replay the ORIGINAL response instead of re-running the handler. It is a pure
// opt-in additive layer on top of the existing money-level idempotency guards
// (checkout service, admin-refund DB dedup) — those are never removed or
// weakened by this.

const (
	idempotencyHeaderCanonical = "Idempotency-Key"
	maxCapturedResponseBytes   = 1 << 20 // 1 MiB — matches DefaultMaxBodyBytes
)

// IdempotencyKeyFromRequest returns the client-supplied idempotency key from the
// standard Idempotency-Key header (#579).
func IdempotencyKeyFromRequest(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(idempotencyHeaderCanonical))
}

type capturedResponse struct {
	Fingerprint string `json:"fp"`
	Status      int    `json:"status"`
	ContentType string `json:"ct,omitempty"`
	Body        []byte `json:"body,omitempty"` // json encodes []byte as base64
}

type captureWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
	over   bool // response exceeded maxCapturedResponseBytes; do not cache
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if !c.over {
		if c.buf.Len()+len(b) > maxCapturedResponseBytes {
			c.over = true
			c.buf.Reset()
		} else {
			c.buf.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

func isMutatingMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// IdempotencyHTTP replays the captured response for a repeated mutating request
// that carries the same Idempotency-Key, scoped per merchant (#579). It engages
// ONLY on mutating methods that carry the header; every other request passes
// through unchanged, so this is a pure opt-in for clients. It layers on top of
// any route-level/business-level idempotency, never replacing it.
func IdempotencyHTTP(svc *idempotency.IdempotencyService) HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if svc == nil || !isMutatingMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			key := IdempotencyKeyFromRequest(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Buffer the body to fingerprint it, then restore it for the handler.
			var body []byte
			if r.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(r.Body, maxCapturedResponseBytes+1))
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			mid, _ := merchant.FromContext(r.Context())
			// target includes the query string so two requests differing only in
			// query (same path+body+key) are distinct operations, not replays (#579).
			target := r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			operation := "http:" + r.Method + " " + target
			storeKey := mid.String() + ":" + hashIdempotencyKey(key)
			fp := fingerprint(r.Method, target, body)
			ctx := r.Context()

			rec, exists, err := svc.Begin(ctx, operation, storeKey)
			if err != nil {
				// Fail OPEN: the store being unavailable must not break the API.
				// Money-critical routes retain their own dedup underneath.
				log.WithError(err).Warn("idempotency: Begin failed; processing without replay (#579)")
				next.ServeHTTP(w, r)
				return
			}

			if exists {
				switch rec.Status {
				case idempotency.IdempotencyStatusPending:
					writeIdempotencyError(w, http.StatusConflict, "idempotency_in_progress",
						"a request with this Idempotency-Key is still being processed")
					return
				case idempotency.IdempotencyStatusSuccess:
					var captured capturedResponse
					if json.Unmarshal(rec.Result, &captured) == nil {
						if captured.Fingerprint != fp {
							writeIdempotencyError(w, http.StatusConflict, "idempotency_key_reuse",
								"Idempotency-Key was reused with a different request payload")
							return
						}
						replayCaptured(w, captured)
						return
					}
					// Unreadable cache — fall through and re-run.
				case idempotency.IdempotencyStatusFailed:
					// Prior attempt failed with no cached response — allow a retry.
				}
				next.ServeHTTP(w, r)
				return
			}

			// We own the pending record. Guard against the handler panicking or
			// otherwise unwinding without resolving the record: an unresolved
			// pending record would 409-lock this key for the full 24h TTL, so a
			// deferred Fail releases it unless a terminal resolution ran (#579).
			resolved := false
			defer func() {
				if !resolved {
					_ = svc.Fail(ctx, operation, storeKey, fmt.Errorf("handler did not complete"))
				}
			}()

			cw := &captureWriter{ResponseWriter: w}
			next.ServeHTTP(cw, r)
			status := cw.status
			if status == 0 {
				status = http.StatusOK
			}
			if status >= 500 || cw.over {
				// Do not cache server errors or oversized bodies; a retry re-runs.
				_ = svc.Fail(ctx, operation, storeKey, fmt.Errorf("status %d not cached", status))
				resolved = true
				return
			}
			payload, mErr := json.Marshal(capturedResponse{
				Fingerprint: fp,
				Status:      status,
				ContentType: cw.Header().Get("Content-Type"),
				Body:        cw.buf.Bytes(),
			})
			if mErr != nil {
				_ = svc.Fail(ctx, operation, storeKey, mErr)
				resolved = true
				return
			}
			if cErr := svc.Complete(ctx, operation, storeKey, payload); cErr != nil {
				log.WithError(cErr).Warn("idempotency: Complete failed; response was still returned (#579)")
			} else {
				resolved = true
			}
		})
	}
}

func hashIdempotencyKey(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

// fingerprint hashes the request identity. target is path plus query string (#579)
// so a differing query does not collide with a cached response for the same path.
func fingerprint(method, target string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte("\n"))
	h.Write([]byte(target))
	h.Write([]byte("\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func replayCaptured(w http.ResponseWriter, captured capturedResponse) {
	if captured.ContentType != "" {
		w.Header().Set("Content-Type", captured.ContentType)
	}
	w.Header().Set("Idempotent-Replayed", "true")
	status := captured.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(captured.Body)
}

func writeIdempotencyError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","code":%q,"message":%q}}`, code, msg)
}
