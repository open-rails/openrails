package billing

import (
	"fmt"
	"net/http"
)

// MaxBatchItems bounds one batch: the items or ids of a batch write or lookup,
// and the ids a List names. A longer batch is refused, never split.
const MaxBatchItems = 100

// batchSize refuses a batch whose field holds n items, outside 1 to limit:
// 400 invalid_param naming the field.
func batchSize(field string, n, limit int) error {
	if n >= 1 && n <= limit {
		return nil
	}
	return &StatusError{Status: http.StatusBadRequest, ErrorDetails: ErrorDetails{
		Type: ErrorTypeInvalidRequest, Code: CodeInvalidParam, Param: &field,
		Message: fmt.Sprintf("%s must hold 1 to %d items", field, limit),
	}}
}
