package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func codes(ve *ValidationError) []string {
	out := make([]string, 0, len(ve.Errors))
	for _, e := range ve.Errors {
		out = append(out, e.Code)
	}
	return out
}

func findErr(t *testing.T, ve *ValidationError, code string) FieldError {
	t.Helper()
	for _, e := range ve.Errors {
		if e.Code == code {
			return e
		}
	}
	t.Fatalf("no %q error in %v", code, codes(ve))
	return FieldError{}
}

func juneRange() *QueryRange { return &QueryRange{From: "2026-06-01", To: "2026-06-30"} }

func TestValidate_UnknownMeasureDidYouMean(t *testing.T) {
	_, ve := Validate(&Query{Measures: []string{"net_revenu"}, Range: juneRange()})
	require.NotNil(t, ve)
	fe := findErr(t, ve, "unknown_measure")
	require.Equal(t, "net_revenue", fe.DidYouMean)
	require.Contains(t, fe.Valid, "mrr")
	require.NotContains(t, fe.Valid, "payment_attempts") // internal components stay hidden
}

func TestValidate_UnknownDimension(t *testing.T) {
	_, ve := Validate(&Query{Measures: []string{"net_revenue"}, By: []string{"rial"}, Range: juneRange()})
	require.NotNil(t, ve)
	fe := findErr(t, ve, "unknown_dimension")
	require.Equal(t, "rail", fe.DidYouMean)
}

func TestValidate_DimensionNotAllowedPerMeasure(t *testing.T) {
	_, ve := Validate(&Query{Measures: []string{"usage_revenue"}, By: []string{"card_brand"}, Range: juneRange()})
	require.NotNil(t, ve)
	fe := findErr(t, ve, "dimension_not_allowed")
	require.Contains(t, fe.Message, "usage_revenue")
	require.Contains(t, fe.Valid, "payer") // corrective: the measure's supported dims
}

func TestValidate_FilterMustBeHonoredByEveryMeasure(t *testing.T) {
	_, ve := Validate(&Query{
		Measures: []string{"admission_denials"},
		Range:    juneRange(),
		Filters:  map[string][]string{"rail": {"nmi"}},
	})
	require.NotNil(t, ve)
	findErr(t, ve, "dimension_not_allowed")
}

func TestValidate_GrainEnum(t *testing.T) {
	_, ve := Validate(&Query{Measures: []string{"net_revenue"}, By: []string{"time"}, Grain: "fortnight", Range: juneRange()})
	require.NotNil(t, ve)
	fe := findErr(t, ve, "invalid_grain")
	require.Equal(t, Grains, fe.Valid)
}

func TestValidate_BucketClamp(t *testing.T) {
	_, ve := Validate(&Query{Measures: []string{"net_revenue"}, By: []string{"time"}, Grain: "day",
		Range: &QueryRange{From: "2024-01-01", To: "2026-06-30"}})
	require.NotNil(t, ve)
	fe := findErr(t, ve, "range_too_many_buckets")
	require.Contains(t, fe.Message, "coarsen")
}

func TestValidate_LimitClamp(t *testing.T) {
	over := MaxLimit + 1
	_, ve := Validate(&Query{Measures: []string{"net_revenue"}, Range: juneRange(), Limit: &over})
	require.NotNil(t, ve)
	findErr(t, ve, "invalid_limit")
}

func TestValidate_ImplicitCurrency(t *testing.T) {
	plan, ve := Validate(&Query{Measures: []string{"net_revenue"}, By: []string{"time"}, Range: juneRange()})
	require.Nil(t, ve)
	require.True(t, plan.ImplicitCurrency)
	require.Contains(t, plan.Dims, "currency")

	// A single-currency filter pins the ledger: no implicit group-by.
	plan, ve = Validate(&Query{Measures: []string{"net_revenue"}, Range: juneRange(),
		Filters: map[string][]string{"currency": {"usd"}}})
	require.Nil(t, ve)
	require.False(t, plan.ImplicitCurrency)
	require.NotContains(t, plan.Dims, "currency")

	// Two currencies in the filter still group implicitly.
	plan, ve = Validate(&Query{Measures: []string{"net_revenue"}, Range: juneRange(),
		Filters: map[string][]string{"currency": {"usd", "eur"}}})
	require.Nil(t, ve)
	require.True(t, plan.ImplicitCurrency)
}

func TestValidate_MoneyPlusCurrencylessMeasureRejected(t *testing.T) {
	// payers_at_depletion_risk supports no dims; mixing it with a money measure
	// (implicit currency group-by) must 400 with a pointed message.
	_, ve := Validate(&Query{Measures: []string{"net_revenue", "payers_at_depletion_risk"}, Range: juneRange()})
	require.NotNil(t, ve)
	fe := findErr(t, ve, "dimension_not_allowed")
	require.Contains(t, fe.Message, "payers_at_depletion_risk")
}

func TestValidate_CompareEnum(t *testing.T) {
	_, ve := Validate(&Query{Measures: []string{"net_revenue"}, Range: juneRange(), Compare: "previous_perod"})
	require.NotNil(t, ve)
	fe := findErr(t, ve, "invalid_compare")
	require.Equal(t, "previous_period", fe.DidYouMean)
}

func TestValidate_OrderMustReferenceRequested(t *testing.T) {
	_, ve := Validate(&Query{Measures: []string{"net_revenue"}, Range: juneRange(),
		Order: []OrderTerm{{Measure: "mrr", Dir: "desc"}}})
	require.NotNil(t, ve)
	findErr(t, ve, "invalid_order")
}

func TestValidate_FilterEnumValues(t *testing.T) {
	_, ve := Validate(&Query{Measures: []string{"payment_count"}, Range: juneRange(),
		Filters: map[string][]string{"attempt_kind": {"renewel"}}})
	require.NotNil(t, ve)
	fe := findErr(t, ve, "invalid_filter_value")
	require.Equal(t, "renewal", fe.DidYouMean)
	require.Equal(t, []string{"initial", "renewal", "unknown"}, fe.Valid)
}

// LLM-legibility acceptance: three distinct mistakes come back as three
// corrective errors in ONE response.
func TestValidate_AllErrorsAtOnce(t *testing.T) {
	_, ve := Validate(&Query{
		Measures: []string{"net_revenu"},
		By:       []string{"time", "attempt_kin"},
		Grain:    "fortnight",
		Range:    juneRange(),
	})
	require.NotNil(t, ve)
	got := codes(ve)
	require.Len(t, got, 3, "all three mistakes reported at once, got %v", got)
	require.Contains(t, got, "unknown_measure")
	require.Contains(t, got, "unknown_dimension")
	require.Contains(t, got, "invalid_grain")
}

func TestDecodeQuery_UnknownBodyKey(t *testing.T) {
	_, ve := DecodeQuery(strings.NewReader(`{"measures":["net_revenue"],"filter":{"rail":["nmi"]}}`))
	require.NotNil(t, ve)
	fe := findErr(t, ve, "unknown_body_key")
	require.Equal(t, "filter", fe.Param)
	require.Equal(t, "filters", fe.DidYouMean)
}

func TestDecodeQuery_MalformedJSON(t *testing.T) {
	_, ve := DecodeQuery(strings.NewReader(`{"measures": [`))
	require.NotNil(t, ve)
	findErr(t, ve, "invalid_body")
}

func TestBucketLabels_MatchDateTruncWeeks(t *testing.T) {
	// 2026-06-03 is a Wednesday; the week bucket starts Monday 2026-06-01.
	from, _ := parseRangeTime("2026-06-03", false)
	to, _ := parseRangeTime("2026-06-16", true)
	labels := bucketLabels(from, to, "week")
	require.Equal(t, []time.Time{
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
	}, labels)
}

func TestValidate_DateRangeInclusiveDays(t *testing.T) {
	plan, ve := Validate(&Query{Measures: []string{"payment_count"}, Range: juneRange()})
	require.Nil(t, ve)
	require.Equal(t, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), plan.From)
	// to-date is inclusive: the window extends to the END of 2026-06-30.
	require.Equal(t, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), plan.To)
}
