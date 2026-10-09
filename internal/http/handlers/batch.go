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

// batchIDs refuses an id list that is empty, longer than limit or holds a
// zero id, and drops repeats: a batch names 1 to limit distinct targets.
func batchIDs[T batchID](r *httprequest.Request, ids []T, limit int, param string) ([]T, bool) {
	if len(ids) == 0 || len(ids) > limit {
		r.APIError(api.Coded(billing.CodeInvalidParam, fmt.Sprintf("%s must hold 1 to %d ids", param, limit)).WithParam(param))
		return nil, false
	}
	out := make([]T, 0, len(ids))
	seen := make(map[T]bool, len(ids))
	for _, id := range ids {
		if id.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidParam, param+" must be nonzero ids").WithParam(param))
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, true
}

// batchItems refuses a batch that is empty or longer than limit.
func batchItems(r *httprequest.Request, n, limit int) bool {
	if n == 0 || n > limit {
		r.APIError(api.Coded(billing.CodeInvalidParam, fmt.Sprintf("items must hold 1 to %d items", limit)).WithParam("items"))
		return false
	}
	return true
}
