package routes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/modules/idempotency"
)

// appResponse is a programmatic write's recorded answer and the request it
// answered.
type appResponse struct {
	Fingerprint string `json:"fingerprint"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
}

// appRequestReplay runs a programmatic write once per Idempotency-Key: a
// retry answers the recorded response, and the key sent with another request
// is refused. A 5xx or 429 is not recorded, so its retry runs again.
func (e *Env) appRequestReplay(route Route) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *httprequest.Request) {
			key := strings.TrimSpace(r.Header("Idempotency-Key"))
			if key == "" || len(key) > middleware.MaxIdempotencyKeyBytes {
				r.AbortCode("idempotency_key_required", fmt.Sprintf("Idempotency-Key header is required (at most %d bytes)", middleware.MaxIdempotencyKeyBytes))
				return
			}
			if e.Runtime == nil || e.Runtime.AppRequests == nil {
				r.AbortCode(billing.CodeServiceUnavailable, "request replay unavailable")
				return
			}
			var body []byte
			if r.Request.Body != nil {
				var err error
				if body, err = io.ReadAll(r.Request.Body); err != nil {
					r.AbortCode(billing.CodeInvalidRequestBody, "could not read request body")
					return
				}
				r.Request.Body = io.NopCloser(bytes.NewReader(body))
			}
			sum := sha256.Sum256(append([]byte(route.Key()+"\n"+r.Request.URL.RawQuery+"\n"), body...))
			fingerprint := hex.EncodeToString(sum[:])
			ctx := r.Request.Context()
			claim, record, err := e.Runtime.AppRequests.Begin(ctx, route.Key(), key)
			if err != nil {
				r.InternalError("request replay unavailable", err)
				return
			}
			if claim == nil {
				replayAppResponse(r, record, fingerprint)
				return
			}
			held, release := claim.Hold(ctx)
			defer release()
			r.Request = r.Request.WithContext(held)
			recorded := appResponse{Fingerprint: fingerprint, Status: http.StatusInternalServerError}
			r.Through(func(h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
					h.ServeHTTP(rec, req)
					recorded.Status, recorded.ContentType, recorded.Body = rec.status, rec.Header().Get("Content-Type"), rec.body.String()
				})
			}, next)
			done := context.WithoutCancel(ctx)
			if recorded.Status >= http.StatusInternalServerError || recorded.Status == http.StatusTooManyRequests {
				_ = claim.Fail(done, fmt.Errorf("answered %d", recorded.Status))
				return
			}
			result, err := json.Marshal(recorded)
			if err == nil {
				err = claim.Complete(done, result)
			}
			if err != nil && !errors.Is(err, idempotency.ErrClaimLost) {
				_ = claim.Fail(done, err)
			}
		}
	}
}

// replayAppResponse answers a key's recorded response, or refuses one still
// running or recorded for another request.
func replayAppResponse(r *httprequest.Request, record *idempotency.Record, fingerprint string) {
	if record == nil || record.Status != idempotency.StatusSucceeded {
		r.AbortCode("idempotency_key_in_progress", "")
		return
	}
	var stored appResponse
	if err := json.Unmarshal(record.Result, &stored); err != nil {
		r.InternalError("recorded response unreadable", err)
		return
	}
	if stored.Fingerprint != fingerprint {
		r.AbortCode(billing.CodeIdempotencyKeyReused, "Idempotency-Key was sent with another request")
		return
	}
	httprequest.FromHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if stored.ContentType != "" {
			w.Header().Set("Content-Type", stored.ContentType)
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(stored.Status)
		_, _ = io.WriteString(w, stored.Body)
	}))(r)
}

// responseRecorder passes a response through and keeps a copy.
type responseRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *responseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
