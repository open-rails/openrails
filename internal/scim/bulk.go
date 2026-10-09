package scim

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// BulkResponse reports each operation of a Bulk request (RFC 7644 §3.7).
type BulkResponse struct {
	Schemas    []string        `json:"schemas"`
	Operations []BulkOperation `json:"Operations"`
}

// BulkOperation is one operation's outcome: a location on success, the
// error as response on failure.
type BulkOperation struct {
	Method   string `json:"method"`
	BulkID   string `json:"bulkId,omitempty"`
	Location string `json:"location,omitempty"`
	Status   string `json:"status"`
	Response any    `json:"response,omitempty"`
}

// Bulk serves POST /Bulk: each operation on Users applies on its own, in
// order, until failOnErrors errors have occurred.
func (s *Server) Bulk() http.Handler {
	return s.serve(func(ctx context.Context, w http.ResponseWriter, r *http.Request, mid billing.MerchantID) {
		raw, e := readBody(w, r)
		if e != nil {
			writeError(w, e)
			return
		}
		a, e := decodeObject(raw)
		if e != nil {
			writeError(w, e)
			return
		}
		if !namesSchema(a, BulkRequestSchema) {
			writeError(w, errorf(http.StatusBadRequest, "invalidSyntax", "schemas must name %s", BulkRequestSchema))
			return
		}
		list, ok := a["operations"].([]any)
		if !ok {
			writeError(w, errorf(http.StatusBadRequest, "invalidSyntax", "Operations must be an array"))
			return
		}
		if len(list) > MaxOperations {
			writeError(w, errorf(http.StatusRequestEntityTooLarge, "tooLarge", "a Bulk request holds at most %d operations", MaxOperations))
			return
		}
		failOnErrors := 0
		if v, ok := a["failonerrors"]; ok && v != nil {
			n, ok := v.(float64)
			if !ok || n < 1 || n != float64(int(n)) {
				writeError(w, badValue("failOnErrors must be a positive integer"))
				return
			}
			failOnErrors = int(n)
		}
		base := baseURL(r, "Bulk")
		created := map[string]string{}
		out := BulkResponse{Schemas: []string{BulkResponseSchema}, Operations: []BulkOperation{}}
		failed := 0
		for _, item := range list {
			if failOnErrors > 0 && failed >= failOnErrors {
				break
			}
			result := s.bulkOperation(ctx, mid, item, base, created)
			if status, _ := strconv.Atoi(result.Status); status >= 400 {
				failed++
			}
			out.Operations = append(out.Operations, result)
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func namesSchema(a attrs, schema string) bool {
	list, _ := a["schemas"].([]any)
	for _, s := range list {
		if v, _ := s.(string); strings.EqualFold(v, schema) {
			return true
		}
	}
	return false
}

func (s *Server) bulkOperation(ctx context.Context, mid billing.MerchantID, item any, base string, created map[string]string) BulkOperation {
	obj, ok := item.(map[string]any)
	if !ok {
		return failure(BulkOperation{}, errorf(http.StatusBadRequest, "invalidSyntax", "each operation must be an object"))
	}
	o := lower(obj)
	method, _, _ := o.str("method")
	bulkID, _, _ := o.str("bulkid")
	path, _, _ := o.str("path")
	op := BulkOperation{Method: strings.ToUpper(method), BulkID: bulkID}
	data := func() (attrs, *Error) {
		raw, ok := o["data"].(map[string]any)
		if !ok {
			return nil, errorf(http.StatusBadRequest, "invalidSyntax", "%s needs data", op.Method)
		}
		return lower(raw), nil
	}
	resource, id, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !strings.EqualFold(resource, "Users") {
		return failure(op, errorf(http.StatusBadRequest, "invalidPath", "a Bulk operation's path is /Users or /Users/{id}"))
	}
	if ref, isRef := strings.CutPrefix(id, "bulkId:"); isRef {
		if id, ok = created[ref]; !ok {
			return failure(op, errorf(http.StatusConflict, "invalidValue", "bulkId %s names no User created before it", ref))
		}
	}
	switch op.Method {
	case http.MethodPost:
		if id != "" {
			return failure(op, errorf(http.StatusBadRequest, "invalidPath", "POST creates at /Users"))
		}
		if bulkID == "" {
			return failure(op, badValue("a POST operation needs a bulkId"))
		}
		in, e := data()
		if e != nil {
			return failure(op, e)
		}
		status, body := s.createFrom(ctx, mid, in, base)
		if user, ok := body.(User); ok {
			created[bulkID] = user.ID
		}
		return outcome(op, status, body)
	case http.MethodPut, http.MethodPatch:
		if id == "" {
			return failure(op, errorf(http.StatusBadRequest, "invalidPath", "%s needs /Users/{id}", op.Method))
		}
		in, e := data()
		if e != nil {
			return failure(op, e)
		}
		var status int
		var body any
		if op.Method == http.MethodPut {
			status, body = s.replaceFrom(ctx, mid, id, in, base)
		} else {
			status, body = s.patchFrom(ctx, mid, id, in, base)
		}
		return outcome(op, status, body)
	case http.MethodDelete:
		if id == "" {
			return failure(op, errorf(http.StatusBadRequest, "invalidPath", "DELETE needs /Users/{id}"))
		}
		if e := s.deleteUser(ctx, mid, id); e != nil {
			return failure(op, e)
		}
		op.Status, op.Location = strconv.Itoa(http.StatusNoContent), base+"/Users/"+id
		return op
	}
	return failure(op, errorf(http.StatusBadRequest, "invalidSyntax", "method must be POST, PUT, PATCH or DELETE"))
}

func outcome(op BulkOperation, status int, body any) BulkOperation {
	if e, ok := body.(*Error); ok {
		return failure(op, e)
	}
	op.Status = strconv.Itoa(status)
	op.Location = body.(User).Meta.Location
	return op
}

func failure(op BulkOperation, e *Error) BulkOperation {
	op.Status = strconv.Itoa(e.Status)
	op.Response = e.body()
	return op
}
