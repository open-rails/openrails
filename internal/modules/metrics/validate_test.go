package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func juneRange() *QueryRange { return &QueryRange{From: "2026-06-01", To: "2026-06-30"} }

func findErr(t *testing.T, ve *ValidationError, code string) FieldError {
	t.Helper()
	require.NotNil(t, ve, "want %q error", code)
	for _, e := range ve.Errors {
		if e.Code == code {
			return e
		}
	}
	t.Fatalf("no %q error in %+v", code, ve.Errors)
	return FieldError{}
}

// Every rejection is corrective: a code, and where possible a did-you-mean or valid list.
func TestValidateRejectsWithCorrectiveErrors(t *testing.T) {
	over := MaxLimit + 1
	now := time.Date(2026, 7, 3, 15, 30, 0, 0, time.UTC)
	cases := []struct {
		name       string
		q          Query
		code       string
		param      string
		didYouMean string
		valid      []string // subset
		notValid   string
		message    string
	}{
		{name: "unknown measure hides internal components", q: Query{Measures: []string{"net_revenu"}},
			code: "unknown_measure", didYouMean: "net_revenue", valid: []string{"mrr"}, notValid: "payment_attempts"},
		{name: "unknown dimension", q: Query{Measures: []string{"net_revenue"}, By: []string{"rial"}},
			code: "unknown_dimension", didYouMean: "rail"},
		{name: "dimension not supported by measure", q: Query{Measures: []string{"usage_revenue"}, By: []string{"card_brand"}},
			code: "dimension_not_allowed", valid: []string{"payer"}, message: "usage_revenue"},
		{name: "filter must be honoured by every measure", q: Query{Measures: []string{"admission_denials"}, Filters: map[string][]string{"rail": {"nmi"}}},
			code: "dimension_not_allowed"},
		{name: "money measure mixed with a currencyless measure", q: Query{Measures: []string{"net_revenue", "payers_at_depletion_risk"}},
			code: "dimension_not_allowed", message: "payers_at_depletion_risk"},
		{name: "grain enum", q: Query{Measures: []string{"net_revenue"}, By: []string{"time"}, Grain: "fortnight"},
			code: "invalid_grain", valid: Grains},
		{name: "bucket clamp", q: Query{Measures: []string{"net_revenue"}, By: []string{"time"}, Grain: "day", Range: &QueryRange{From: "2024-01-01", To: "2026-06-30"}},
			code: "range_too_many_buckets", message: "coarsen"},
		{name: "limit clamp", q: Query{Measures: []string{"net_revenue"}, Limit: &over}, code: "invalid_limit"},
		{name: "compare enum", q: Query{Measures: []string{"net_revenue"}, Compare: "previous_perod"},
			code: "invalid_compare", didYouMean: "previous_period"},
		{name: "order must reference a requested measure", q: Query{Measures: []string{"net_revenue"}, Order: []OrderTerm{{Measure: "mrr", Dir: "desc"}}},
			code: "invalid_order"},
		{name: "filter enum value", q: Query{Measures: []string{"payment_count"}, Filters: map[string][]string{"attempt_kind": {"renewel"}}},
			code: "invalid_filter_value", didYouMean: "renewal", valid: []string{"initial", "renewal", "unknown"}},
		{name: "malformed relative range", q: Query{Measures: []string{"cancellations"}, Range: &QueryRange{Last: "seven days"}},
			code: "invalid_range", param: "range.last"},
		{name: "zero-length relative range", q: Query{Measures: []string{"cancellations"}, Range: &QueryRange{Last: "0d"}},
			code: "invalid_range"},
		{name: "last excludes from/to", q: Query{Measures: []string{"cancellations"}, Range: &QueryRange{Last: "7d", From: "2026-01-01", To: "2026-02-01"}},
			code: "invalid_range", message: "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.q
			if q.Range == nil {
				q.Range = juneRange()
			}
			_, ve := ValidateAt(&q, now)
			fe := findErr(t, ve, tc.code)
			if tc.param != "" {
				require.Equal(t, tc.param, fe.Param)
			}
			if tc.didYouMean != "" {
				require.Equal(t, tc.didYouMean, fe.DidYouMean)
			}
			for _, v := range tc.valid {
				require.Contains(t, fe.Valid, v)
			}
			if tc.notValid != "" {
				require.NotContains(t, fe.Valid, tc.notValid)
			}
			if tc.message != "" {
				require.Contains(t, fe.Message, tc.message)
			}
		})
	}
}

// An LLM caller fixes every mistake in one round trip.
func TestValidateReportsAllErrorsAtOnce(t *testing.T) {
	_, ve := Validate(&Query{Measures: []string{"net_revenu"}, By: []string{"time", "attempt_kin"}, Grain: "fortnight", Range: juneRange()})
	require.NotNil(t, ve)
	var got []string
	for _, e := range ve.Errors {
		got = append(got, e.Code)
	}
	require.ElementsMatch(t, []string{"unknown_measure", "unknown_dimension", "invalid_grain"}, got)
}

func TestDecodeQueryRejectsUnknownKeysAndMalformedJSON(t *testing.T) {
	_, ve := DecodeQuery(strings.NewReader(`{"measures":["net_revenue"],"filter":{"rail":["nmi"]}}`))
	fe := findErr(t, ve, "unknown_body_key")
	require.Equal(t, "filter", fe.Param)
	require.Equal(t, "filters", fe.DidYouMean)

	_, ve = DecodeQuery(strings.NewReader(`{"measures": [`))
	findErr(t, ve, "invalid_body")
}

// Money measures group by currency unless a single-currency filter pins the ledger.
func TestImplicitCurrencyGrouping(t *testing.T) {
	for _, tc := range []struct {
		currencies []string
		implicit   bool
	}{{nil, true}, {[]string{"usd"}, false}, {[]string{"usd", "eur"}, true}} {
		q := &Query{Measures: []string{"net_revenue"}, By: []string{"time"}, Range: juneRange()}
		if tc.currencies != nil {
			q.Filters = map[string][]string{"currency": tc.currencies}
		}
		plan, ve := Validate(q)
		require.Nil(t, ve)
		require.Equal(t, tc.implicit, plan.ImplicitCurrency, tc.currencies)
		if tc.implicit {
			require.Contains(t, plan.Dims, "currency")
		} else {
			require.NotContains(t, plan.Dims, "currency")
		}
	}
}

// Ranges are inclusive UTC calendar days; relative ranges end today (#741 saved widgets stay current).
func TestRangeResolution(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

	plan, ve := Validate(&Query{Measures: []string{"payment_count"}, Range: juneRange()})
	require.Nil(t, ve)
	require.Equal(t, day(2026, 6, 1), plan.From)
	require.Equal(t, day(2026, 7, 1), plan.To, "to-date is inclusive")

	now := time.Date(2026, 7, 3, 15, 30, 0, 0, time.UTC)
	plan, ve = ValidateAt(&Query{Measures: []string{"cancellations"}, By: []string{"time"}, Grain: "day", Range: &QueryRange{Last: "7d"}}, now)
	require.Nil(t, ve)
	require.Equal(t, day(2026, 6, 27), plan.From)
	require.Equal(t, day(2026, 7, 4), plan.To)
	require.Len(t, plan.Buckets, 7, "past 7 days includes today")

	for last, from := range map[string]time.Time{"12w": day(2026, 4, 11), "6m": day(2026, 1, 4), "1y": day(2025, 7, 4)} {
		plan, ve := ValidateAt(&Query{Measures: []string{"cancellations"}, Range: &QueryRange{Last: last}}, now)
		require.Nil(t, ve, last)
		require.Equal(t, from, plan.From, last)
	}

	// Week buckets start Monday, matching Postgres date_trunc('week').
	from, _ := parseRangeTime("2026-06-03", false)
	to, _ := parseRangeTime("2026-06-16", true)
	require.Equal(t, []time.Time{day(2026, 6, 1), day(2026, 6, 8), day(2026, 6, 15)}, bucketLabels(from, to, "week"))
}
