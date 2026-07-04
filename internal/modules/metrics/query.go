// Package metrics is the #733 merchant analytics engine: a fixed in-code
// registry of measures/dimensions and a compiler that turns a JSON query into
// one parameterized SQL statement per source family, executed under MerchantTx
// (RLS-pinned). No client string ever becomes SQL text — names resolve through
// the registry allowlist, values ride as bind parameters.
package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Grains accepted for the time dimension.
var Grains = []string{"day", "week", "month", "quarter", "year"}

const (
	// MaxBuckets caps a time series (range / grain).
	MaxBuckets = 400
	// MaxLimit caps returned rows.
	MaxLimit = 1000
	// DefaultLimit applies when the query omits limit.
	DefaultLimit = 1000
)

// Query is the POST /v1/merchant/metrics/query body.
type Query struct {
	Measures []string            `json:"measures"`
	By       []string            `json:"by"`
	Grain    string              `json:"grain"`
	Range    *QueryRange         `json:"range"`
	Filters  map[string][]string `json:"filters"`
	Order    []OrderTerm         `json:"order"`
	Limit    *int                `json:"limit"`
	Compare  string              `json:"compare"`
}

// QueryRange bounds the query window. Date-only values are UTC calendar days:
// from = day start (inclusive), to = day end (inclusive, i.e. to+1d exclusive).
// RFC3339 timestamps are taken verbatim as [from, to).
type QueryRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// OrderTerm orders result rows by a requested measure or dimension.
type OrderTerm struct {
	Measure   string `json:"measure,omitempty"`
	Dimension string `json:"dimension,omitempty"`
	Dir       string `json:"dir,omitempty"` // asc|desc (default asc)
}

// DecodeQuery strictly decodes a query body: unknown JSON keys are an error
// (LLM-legibility: a typo'd key must fail loudly, never be silently ignored).
func DecodeQuery(r io.Reader) (*Query, *ValidationError) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, singleError(FieldError{Code: "invalid_body", Param: "body", Message: fmt.Sprintf("could not read request body: %v", err)})
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var q Query
	if err := dec.Decode(&q); err != nil {
		return nil, singleError(decodeFieldError(err))
	}
	// Trailing garbage after the JSON object is also a malformed body.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, singleError(FieldError{Code: "invalid_body", Param: "body", Message: "request body must be a single JSON object"})
	}
	return &q, nil
}

func decodeFieldError(err error) FieldError {
	msg := err.Error()
	if strings.Contains(msg, "unknown field") {
		// json: unknown field "filter"
		field := msg
		if i := strings.Index(msg, `"`); i >= 0 {
			field = strings.Trim(msg[i:], `"`)
		}
		fe := FieldError{
			Code:    "unknown_body_key",
			Param:   field,
			Message: fmt.Sprintf("unknown key %q in query body; valid keys: measures, by, grain, range, filters, order, limit, compare", field),
			Valid:   []string{"measures", "by", "grain", "range", "filters", "order", "limit", "compare"},
		}
		if s, ok := suggest(field, fe.Valid); ok {
			fe.DidYouMean = s
			fe.Message = fmt.Sprintf("unknown key %q in query body; did you mean %q? valid keys: measures, by, grain, range, filters, order, limit, compare", field, s)
		}
		return fe
	}
	return FieldError{Code: "invalid_body", Param: "body", Message: fmt.Sprintf("malformed JSON body: %v", err)}
}

// parseRangeTime parses a range endpoint: date-only or RFC3339.
func parseRangeTime(s string, dayEnd bool) (time.Time, bool) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		t = t.UTC()
		if dayEnd {
			t = t.AddDate(0, 0, 1)
		}
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
