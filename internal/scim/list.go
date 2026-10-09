package scim

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// ListResponse is a page of Users (RFC 7644 §3.4.2).
type ListResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int64    `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []User   `json:"Resources"`
}

// filterExpr is the one filter form served: an attribute, eq, and a string.
var filterExpr = regexp.MustCompile(`(?i)^\s*([a-z.:0-9]+)\s+eq\s+("(?:[^"\\]|\\.)*")\s*$`)

// filter is a list's equality filter, at most one attribute.
type filter struct {
	customerID *uuid.UUID
	userName   *string
	email      *string
	// none matches nothing: an id that is no customer id.
	none bool
}

func parseFilter(raw string) (filter, *Error) {
	var f filter
	if strings.TrimSpace(raw) == "" {
		return f, nil
	}
	m := filterExpr.FindStringSubmatch(raw)
	if m == nil {
		return f, errorf(http.StatusBadRequest, "invalidFilter", `the filter must be one of externalId, id, userName or emails.value eq "value"`)
	}
	var value string
	if err := json.Unmarshal([]byte(m[2]), &value); err != nil {
		return f, errorf(http.StatusBadRequest, "invalidFilter", "the filter's value is not a string")
	}
	attr := strings.TrimPrefix(strings.ToLower(m[1]), strings.ToLower(UserSchema)+":")
	switch attr {
	case "externalid", "id":
		id, ok := customerID(value)
		if !ok {
			f.none = true
			return f, nil
		}
		f.customerID = &id
	case "username":
		f.userName = &value
	case "emails.value", "emails":
		f.email = &value
	default:
		return f, errorf(http.StatusBadRequest, "invalidFilter", "the filter attribute %s is not supported", m[1])
	}
	return f, nil
}

// ListUsers serves GET /Users: the Users a SCIM client holds, oldest first,
// by startIndex (from 1) and count.
func (s *Server) ListUsers() http.Handler {
	return s.serve(func(ctx context.Context, w http.ResponseWriter, r *http.Request, mid billing.MerchantID) {
		q := r.URL.Query()
		f, e := parseFilter(q.Get("filter"))
		if e != nil {
			writeError(w, e)
			return
		}
		start, e := intParam(q.Get("startIndex"), 1)
		if e != nil {
			writeError(w, e)
			return
		}
		count, e := intParam(q.Get("count"), MaxResults)
		if e != nil {
			writeError(w, e)
			return
		}
		start, count = max(start, 1), min(max(count, 0), MaxResults)
		out := ListResponse{Schemas: []string{ListResponseSchema}, StartIndex: start, Resources: []User{}}
		if !f.none {
			if err := s.list(ctx, mid, f, start, count, baseURL(r, "Users"), &out); err != nil {
				log.WithContext(ctx).WithError(err).Error("scim: list failed")
				writeError(w, errInternal)
				return
			}
		}
		out.ItemsPerPage = len(out.Resources)
		writeJSON(w, http.StatusOK, out)
	})
}

func (s *Server) list(ctx context.Context, mid billing.MerchantID, f filter, start, count int, base string, out *ListResponse) error {
	q := s.DB.Gen(ctx)
	total, err := q.CountProvisionedContacts(ctx, gen.CountProvisionedContactsParams{MerchantID: mid.UUID(), CustomerID: f.customerID, UserName: f.userName, Email: f.email})
	if err != nil {
		return err
	}
	out.TotalResults = total
	if count == 0 || int64(start) > total {
		return nil
	}
	rows, err := q.ListProvisionedContacts(ctx, gen.ListProvisionedContactsParams{
		MerchantID: mid.UUID(), CustomerID: f.customerID, UserName: f.userName, Email: f.email, RowOffset: int32(start - 1), RowLimit: int32(count),
	})
	if err != nil {
		return err
	}
	for _, row := range rows {
		out.Resources = append(out.Resources, userResource(row, base))
	}
	return nil
}

func intParam(raw string, fallback int) (int, *Error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, errorf(http.StatusBadRequest, "invalidValue", "startIndex and count must be integers")
	}
	return n, nil
}
