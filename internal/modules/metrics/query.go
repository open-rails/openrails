package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
)

// Grains accepted for the time dimension; auto is day up to 45 days, week up
// to 26 weeks, then month.
var Grains = []string{"auto", "day", "week", "month", "quarter", "year"}

// Compares are the comparison windows: the preceding window of equal length,
// or the same window a year before.
var Compares = []string{"previous_period", "previous_year"}

const (
	// MaxBuckets caps a time series (range / grain).
	MaxBuckets = 400
	// MaxLimit caps returned rows.
	MaxLimit = 1000
	// DefaultLimit applies when the query omits limit.
	DefaultLimit = 1000
)

// Query is the POST /v1/metrics/query body.
type Query = billing.QueryMetricsParams

// QueryRange bounds the query window. Date-only values are inclusive days in
// the merchant's accounting timezone; RFC3339 timestamps are taken verbatim as
// [from, to). Last ("7d", "12w", "6m", "1y") is a trailing window ending today
// there, exclusive with from/to, so a saved widget stays current.
type QueryRange = billing.MetricsRange

// OrderTerm orders result rows by a requested measure or dimension.
type OrderTerm = billing.MetricsOrder

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

// parseRangeTime parses a range endpoint: a date in zone or RFC3339, as a
// UTC instant.
func parseRangeTime(s string, dayEnd bool, zone *time.Location) (time.Time, bool) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		y, m, d := t.Date()
		if dayEnd {
			d++
		}
		return calendarutil.DayStart(y, m, d, zone).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
