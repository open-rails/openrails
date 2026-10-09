// Package scim is OpenRails' SCIM 2.0 service provider (RFC 7643, RFC 7644):
// a merchant's directory pushes its users, which OpenRails keeps as its
// customers' contacts. Users only, by the User schema's core attributes;
// externalId is the host's subject UUID, which is the customer id and the
// SCIM id.
package scim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
)

// The schema and message URNs of RFC 7643 and RFC 7644.
const (
	UserSchema                  = "urn:ietf:params:scim:schemas:core:2.0:User"
	ServiceProviderConfigSchema = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	ResourceTypeSchema          = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	SchemaSchema                = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	ListResponseSchema          = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	PatchOpSchema               = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	BulkRequestSchema           = "urn:ietf:params:scim:api:messages:2.0:BulkRequest"
	BulkResponseSchema          = "urn:ietf:params:scim:api:messages:2.0:BulkResponse"
	ErrorSchema                 = "urn:ietf:params:scim:api:messages:2.0:Error"
)

// MediaType is SCIM's content type; application/json is accepted too.
const MediaType = "application/scim+json"

// Limits the service provider advertises.
const (
	// MaxOperations bounds one Bulk request's operations.
	MaxOperations = 1000
	// MaxPayloadSize bounds one Bulk request's body, in bytes.
	MaxPayloadSize = 1 << 20
	// MaxResults bounds one list response.
	MaxResults = 200
)

// Server serves the SCIM routes. Authenticate resolves the merchant a request
// acts for, or refuses it; every route needs it, discovery included.
type Server struct {
	DB           *db.DB
	Now          func() time.Time
	Authenticate func(*http.Request) (billing.MerchantID, error)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Error is a SCIM error response (RFC 7644 §3.12).
type Error struct {
	Status   int
	ScimType string
	Detail   string
}

func (e *Error) Error() string { return fmt.Sprintf("scim %d %s: %s", e.Status, e.ScimType, e.Detail) }

// ErrorBody is an error's response body, its status a string as the RFC
// shows.
type ErrorBody struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

func (e *Error) body() ErrorBody {
	return ErrorBody{[]string{ErrorSchema}, strconv.Itoa(e.Status), e.ScimType, e.Detail}
}

func errorf(status int, scimType, format string, args ...any) *Error {
	return &Error{Status: status, ScimType: scimType, Detail: fmt.Sprintf(format, args...)}
}

func badValue(format string, args ...any) *Error {
	return errorf(http.StatusBadRequest, "invalidValue", format, args...)
}

var (
	errNotFound = errorf(http.StatusNotFound, "", "no such User")
	errInternal = errorf(http.StatusInternalServerError, "", "the request could not be completed")
)

// ErrUnauthorized refuses a request without a credential the merchant
// accepts.
var ErrUnauthorized = errors.New("scim: no valid credential")

// writeJSON answers status with body as application/scim+json.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", MediaType+"; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// WriteRefusal answers a refusal made before the SCIM server ran, such as
// the host's Auth refusing a credential, as a SCIM error with its status.
func WriteRefusal(w http.ResponseWriter, status int, detail string) {
	writeError(w, &Error{Status: status, Detail: detail})
}

func writeError(w http.ResponseWriter, e *Error) {
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
	}
	writeJSON(w, e.Status, e.body())
}

// handler serves one route for the merchant the request authenticates as.
type handler func(ctx context.Context, w http.ResponseWriter, r *http.Request, mid billing.MerchantID)

func (s *Server) serve(h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s == nil || s.Authenticate == nil || s.DB == nil {
			writeError(w, errorf(http.StatusServiceUnavailable, "", "provisioning is not available"))
			return
		}
		mid, err := s.Authenticate(r)
		if err != nil {
			if !errors.Is(err, ErrUnauthorized) {
				log.WithContext(r.Context()).WithError(err).Warn("scim: authentication failed")
			}
			writeError(w, errorf(http.StatusUnauthorized, "", "a provisioning token or an application credential is required"))
			return
		}
		h(merchant.WithID(r.Context(), mid), w, r, mid)
	})
}

// baseURL is the absolute URL of the SCIM root the request reached: its path
// up to the route's resource segment.
func baseURL(r *http.Request, resource string) string {
	path := r.URL.Path
	if i := strings.LastIndex(path, "/"+resource); i >= 0 {
		path = path[:i]
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}
