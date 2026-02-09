package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// readJSONSingleOrArrayRaw reads the request body and returns:
// - a single item (isBatch=false) if the JSON is an object
// - multiple items (isBatch=true) if the JSON is an array
//
// For batch mode we return RawMessages so handlers can validate and execute each item
// independently (partial success is allowed).
func readJSONSingleOrArrayRaw(r *httprequest.Request) (items []json.RawMessage, isBatch bool, err error) {
	if r == nil || r.Request == nil || r.Request.Body == nil {
		return nil, false, fmt.Errorf("invalid request")
	}
	body, err := io.ReadAll(r.Request.Body)
	if err != nil {
		return nil, false, err
	}
	// Reset body so other middleware/debug tooling could still read it if needed.
	r.Request.Body = io.NopCloser(bytes.NewReader(body))

	b := bytes.TrimSpace(body)
	if len(b) == 0 {
		return nil, false, fmt.Errorf("empty body")
	}
	if b[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(b, &arr); err != nil {
			return nil, true, err
		}
		return arr, true, nil
	}
	return []json.RawMessage{json.RawMessage(b)}, false, nil
}
