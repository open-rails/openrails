package metrics

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The compiler trusts these registry facts without re-checking them.
func TestRegistryIsSelfConsistent(t *testing.T) {
	for i := range Measures {
		m := &Measures[i]
		if m.Class == ClassRatio {
			require.NotNil(t, measureByName[m.Num], "%s: numerator %q", m.Name, m.Num)
			require.NotNil(t, measureByName[m.Den], "%s: denominator %q", m.Name, m.Den)
			for _, leaf := range m.components() {
				for _, d := range m.Dims {
					require.True(t, leaf.allowsDim(d), "%s: dim %q unsupported by %q", m.Name, d, leaf.Name)
				}
			}
		} else {
			spec, ok := families[m.Family]
			require.True(t, ok, "%s: family %q undeclared", m.Name, m.Family)
			for _, d := range m.Dims {
				require.NotNil(t, dimensionByName[d], "%s: dim %q", m.Name, d)
				if m.Family != FamDepletion {
					_, ok := spec.DimExprs[d]
					require.True(t, ok, "%s: family %s lacks expr for %q", m.Name, m.Family, d)
				}
			}
			if m.Family != FamDepletion {
				require.NotEmpty(t, m.Expr, m.Name)
			}
		}
		if !m.Internal {
			require.NotEmpty(t, m.Description, m.Name)
			require.NotEmpty(t, m.Formula, m.Name)
			require.NotEmpty(t, m.Unit, m.Name)
		}
		if m.Money {
			require.True(t, m.allowsDim("currency"), "%s: money measure must allow the implicit currency group-by", m.Name)
			if m.Class != ClassRatio {
				require.Equal(t, UnitMoney, m.Unit, m.Name)
			}
		}
		if m.Unit == UnitMoney {
			require.True(t, m.Money, m.Name)
		}
	}
}

// The public vocabulary is an API contract: an accidental trim or addition fails here.
func TestPublicMeasureVocabulary(t *testing.T) {
	require.ElementsMatch(t, []string{
		"gross_revenue", "net_revenue", "refunds", "chargebacks", "credits_sold", "usage_revenue",
		"payment_count", "payment_failures", "new_subscriptions", "cancellations", "chargeback_count",
		"refund_count", "usage_units", "admission_denials",
		"unique_failed_customers", "unique_rebilled_customers", "active_payers",
		"churn_rate", "approval_rate", "chargeback_rate", "recovery_rate", "credit_utilization",
		"repeat_topup_rate", "realized_revenue_per_customer", "avg_membership_duration_days",
		"mrr", "subscriptions", "billable_subscriptions", "entitled_customers",
		"payers_at_depletion_risk", "outstanding_credit_liability", "outstanding_owed",
		"webhook_silence_age_seconds", "webhook_rejects", "webhook_drift_events",
	}, PublicMeasureNames())
}

func TestSchemaExamplesAllValidate(t *testing.T) {
	doc := Schema()
	require.NotEmpty(t, doc.Deferred)
	require.NotEmpty(t, doc.Caveats)
	require.GreaterOrEqual(t, len(doc.Examples), 5)
	payload, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NotContains(t, string(payload), `"derived"`, "internal components stay out of the schema")
	golden := false
	for _, ex := range doc.Examples {
		_, ve := Validate(&ex.Query)
		require.Nil(t, ve, "example %q: %v", ex.Intent, ve)
		if strings.Contains(ex.Intent, "cancelled per day") {
			golden = true
			require.Equal(t, []string{"cancellations"}, ex.Query.Measures)
			require.Equal(t, "day", ex.Query.Grain)
		}
	}
	require.True(t, golden)
}

func TestCompile(t *testing.T) {
	t.Run("filter values are bind params, never SQL text", func(t *testing.T) {
		plan, ve := Validate(&Query{
			Measures: []string{"net_revenue"}, By: []string{"time", "rail"}, Grain: "week", Range: juneRange(),
			Filters: map[string][]string{"rail": {"nmi'; DROP TABLE openrails.payments;--"}},
		})
		require.Nil(t, ve)
		stmts, err := compile(plan, uuid.New())
		require.NoError(t, err)
		require.Len(t, stmts, 1)
		require.NotContains(t, stmts[0].sql, "DROP TABLE")
		require.Contains(t, stmts[0].sql, "= ANY($")
	})
	t.Run("one statement per source family", func(t *testing.T) {
		plan, ve := Validate(&Query{
			Measures: []string{"gross_revenue", "net_revenue", "refunds", "payment_count", "mrr", "subscriptions"},
			By:       []string{"time"}, Grain: "month",
			Range:   &QueryRange{From: "2026-04-01", To: "2026-06-30"},
			Filters: map[string][]string{"currency": {"usd"}},
		})
		require.Nil(t, ve)
		stmts, err := compile(plan, uuid.New())
		require.NoError(t, err)
		require.Len(t, stmts, 2, "payments flow + subscriptions snapshot")
	})
}
