package metrics

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Registry invariants: the compiler trusts these, so they are pinned here.
func TestRegistry_Invariants(t *testing.T) {
	for i := range Measures {
		m := &Measures[i]
		if m.Class == ClassRatio {
			num, den := measureByName[m.Num], measureByName[m.Den]
			require.NotNil(t, num, "%s: numerator %q missing", m.Name, m.Num)
			require.NotNil(t, den, "%s: denominator %q missing", m.Name, m.Den)
			// A ratio's dims must be supported by every leaf component.
			for _, leaf := range m.components() {
				for _, d := range m.Dims {
					require.True(t, leaf.allowsDim(d), "%s: dim %q not supported by component %q", m.Name, d, leaf.Name)
				}
			}
		} else {
			spec, ok := families[m.Family]
			require.True(t, ok, "%s: family %q undeclared", m.Name, m.Family)
			for _, d := range m.Dims {
				require.NotNil(t, dimensionByName[d], "%s: dim %q not in dimension registry", m.Name, d)
				if m.Family == FamDepletion {
					continue
				}
				_, ok := spec.DimExprs[d]
				require.True(t, ok, "%s: family %s lacks SQL expr for dim %q", m.Name, m.Family, d)
			}
			if m.Family != FamDepletion {
				require.NotEmpty(t, m.Expr, "%s: missing SQL expr", m.Name)
			}
		}
		if !m.Internal {
			require.NotEmpty(t, m.Description, "%s: missing description", m.Name)
			require.NotEmpty(t, m.Formula, "%s: missing formula", m.Name)
			require.NotEmpty(t, m.Unit, "%s: missing unit", m.Name)
		}
	}
	// Money measures must allow the implicit currency group-by.
	for i := range Measures {
		m := &Measures[i]
		if m.Money {
			require.True(t, m.allowsDim("currency"), "%s: money measure must allow currency", m.Name)
		}
	}
}

// The CORE tier is required IN FULL — pin the public vocabulary so an
// accidental trim fails loudly.
func TestRegistry_CoreTierComplete(t *testing.T) {
	want := []string{
		"gross_revenue", "net_revenue", "refunds", "chargebacks", "credits_sold", "usage_revenue",
		"payment_count", "payment_failures", "new_subscriptions", "cancellations", "chargeback_count",
		"refund_count", "usage_units", "admission_denials",
		"unique_failed_customers", "unique_rebilled_customers", "active_payers",
		"churn_rate", "approval_rate", "chargeback_rate", "recovery_rate", "credit_utilization",
		"repeat_topup_rate", "realized_revenue_per_customer", "avg_membership_duration_days",
		"mrr", "subscriptions", "billable_subscriptions", "entitled_customers",
		"payers_at_depletion_risk", "outstanding_credit_liability", "outstanding_owed",
	}
	got := PublicMeasureNames()
	for _, w := range want {
		require.Contains(t, got, w)
	}
	require.Len(t, got, len(want))
}

func TestSchema_CarriesExamplesAndCaveats(t *testing.T) {
	doc := Schema()
	require.NotEmpty(t, doc.Derived)
	require.NotEmpty(t, doc.Deferred)
	require.NotEmpty(t, doc.Caveats)
	require.GreaterOrEqual(t, len(doc.Examples), 5)
	// The golden example.
	found := false
	for _, ex := range doc.Examples {
		if strings.Contains(ex.Intent, "cancelled per day") {
			found = true
			require.Equal(t, []string{"cancellations"}, ex.Query.Measures)
			require.Equal(t, "day", ex.Query.Grain)
		}
	}
	require.True(t, found, "golden cancellations-per-day example missing")
	// Every example must validate against the registry it documents.
	for _, ex := range doc.Examples {
		_, ve := Validate(&ex.Query)
		require.Nil(t, ve, "example %q does not validate: %v", ex.Intent, ve)
	}
}

// Client values must never end up in SQL text — only in args.
func TestCompile_FilterValuesAreBindParams(t *testing.T) {
	plan, ve := Validate(&Query{
		Measures: []string{"net_revenue"},
		By:       []string{"time", "rail"},
		Grain:    "week",
		Range:    juneRange(),
		Filters:  map[string][]string{"rail": {"nmi'; DROP TABLE openrails.payments;--"}},
	})
	require.Nil(t, ve)
	stmts, err := compile(plan, uuid.New())
	require.NoError(t, err)
	require.Len(t, stmts, 1)
	require.NotContains(t, stmts[0].sql, "DROP TABLE")
	require.Contains(t, stmts[0].sql, "= ANY($")
}

// One statement per source family regardless of measure count.
func TestCompile_OneStatementPerFamily(t *testing.T) {
	plan, ve := Validate(&Query{
		Measures: []string{"gross_revenue", "net_revenue", "refunds", "payment_count", "mrr", "subscriptions"},
		By:       []string{"time"},
		Grain:    "month",
		Range:    &QueryRange{From: "2026-04-01", To: "2026-06-30"},
		Filters:  map[string][]string{"currency": {"usd"}},
	})
	require.Nil(t, ve)
	stmts, err := compile(plan, uuid.New())
	require.NoError(t, err)
	require.Len(t, stmts, 2) // payments flow + subs snapshot
}
