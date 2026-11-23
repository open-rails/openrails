package handlers

import (
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

type batchID interface {
	comparable
	IsZero() bool
}

// batchItems refuses a batch that is empty or longer than limit.
func batchItems(r *httprequest.Request, n, limit int) bool {
	if n == 0 || n > limit {
		r.APIError(api.Coded(billing.CodeInvalidParam, fmt.Sprintf("items must hold 1 to %d items", limit)).WithParam("items"))
		return false
	}
	return true
}

// bindList reads a list route's query into params, and its page bounded
// (Request.Page) into page, a field of params.
func bindList[T any](r *httprequest.Request, params *T, page *billing.PageRequest) bool {
	if !r.BindQuery(params) {
		return false
	}
	var ok bool
	*page, ok = r.Page()
	return ok
}

// listIDs is a List's ids filter as the mount decoded and bounded it: the
// named ids without repeats, or nil when the request names none.
func listIDs[T batchID](r *httprequest.Request, ids []T) ([]T, bool) {
	return uniqueIDs(r, "ids", ids)
}

// uniqueIDs checks the id list of parameter name: at most
// billing.MaxBatchItems ids, none empty, without repeats; nil when the
// request names none. A refusal is 400 invalid_query.
func uniqueIDs[T batchID](r *httprequest.Request, name string, ids []T) ([]T, bool) {
	if len(ids) == 0 {
		return nil, true
	}
	if len(ids) > billing.MaxBatchItems {
		r.APIError(api.Coded(billing.CodeInvalidQuery, fmt.Sprintf("%s names at most %d ids", name, billing.MaxBatchItems)).WithParam(name))
		return nil, false
	}
	out := make([]T, 0, len(ids))
	seen := make(map[T]bool, len(ids))
	for _, id := range ids {
		if id.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidQuery, name+" holds an empty id").WithParam(name))
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, true
}
