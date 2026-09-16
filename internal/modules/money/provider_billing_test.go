package money

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

func TestPrepareProviderBillingObservationRefusalsAndOverflow(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	base := ProviderBillingObservationInput{
		OperationID:     "op-1",
		ObservationID:   "obs-1",
		NormalizedQuery: "podId=provider-resource-1",
		QueryStart:      now.Add(-time.Hour),
		QueryEnd:        now,
		RawBody:         []byte(`[]`),
		Lifecycle: ProviderBillingLifecycleEvidence{
			Provider: "provider", ProviderResourceID: "provider-resource-1",
			ProviderLifetimeStart: now.Add(-time.Hour), ProviderLifetimeEnd: now.Add(-time.Minute),
			ProviderAbsentAt: now, ProviderAbsenceReference: "absence:1",
			BillingStopReference: "billing-stop:1", WindowsClosedAt: now,
			WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{"absent":true}`),
		},
	}

	for _, kind := range []string{"schema_ambiguity", "submicro_amount", "amount_overflow"} {
		in := base
		in.Refusal = &ProviderBillingObservationRefusal{Kind: ProviderBillingEvidenceRefusalKind(kind)}
		require.NoError(t, validateProviderBillingInput(in))
		prepared, err := prepareProviderBillingObservation(in)
		require.NoError(t, err)
		require.Equal(t, kind, *prepared.refusalKind)
		require.True(t, prepared.rawAvailable)

		in.RawBody = nil
		require.ErrorContains(t, validateProviderBillingInput(in), "requires exact bounded raw body")
	}

	tooLarge := base
	tooLarge.RawBody = nil
	tooLarge.Refusal = &ProviderBillingObservationRefusal{Kind: ProviderBillingRefusalResponseTooLarge}
	require.NoError(t, validateProviderBillingInput(tooLarge))
	prepared, err := prepareProviderBillingObservation(tooLarge)
	require.NoError(t, err)
	require.False(t, prepared.rawAvailable)
	tooLarge.RawBody = []byte("partial")
	require.ErrorContains(t, validateProviderBillingInput(tooLarge), "cannot retain partial raw body")

	overflow := base
	overflow.Records = []ProviderBillingRecord{
		{ProviderResourceID: base.Lifecycle.ProviderResourceID, BucketStart: now.Add(-time.Hour), AmountUSDMicros: math.MaxInt64, TimeBilledMS: 1},
		{ProviderResourceID: base.Lifecycle.ProviderResourceID, BucketStart: now.Add(-time.Minute), AmountUSDMicros: 1, TimeBilledMS: 1},
	}
	_, err = prepareProviderBillingObservation(overflow)
	require.ErrorContains(t, err, "total USD micros overflow")

	negative := base
	negative.Records = []ProviderBillingRecord{{
		ProviderResourceID: base.Lifecycle.ProviderResourceID,
		BucketStart:        now.Add(-time.Hour), AmountUSDMicros: -1, TimeBilledMS: 1,
	}}
	prepared, err = prepareProviderBillingObservation(negative)
	require.NoError(t, err)
	require.True(t, prepared.hasNegative)

	nanosecondTime := base
	nanosecondTime.QueryEnd = nanosecondTime.QueryEnd.Add(time.Nanosecond)
	require.ErrorContains(t, validateProviderBillingInput(nanosecondTime), "PostgreSQL-exact microsecond precision")

	zeroLifetime := base
	zeroLifetime.Lifecycle.ProviderLifetimeEnd = zeroLifetime.Lifecycle.ProviderLifetimeStart
	require.ErrorContains(t, validateProviderBillingInput(zeroLifetime), "must be after provider_lifetime_start")
}

func TestProviderBillingObservationEnvelopeIsTransportNeutral(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	in := ProviderBillingObservationInput{
		OperationID: "op-1", ObservationID: "obs-1", NormalizedQuery: "podId=pod-1",
		QueryStart: now.Add(-time.Hour), QueryEnd: now,
		Lifecycle: ProviderBillingLifecycleEvidence{
			Provider: "provider", ProviderResourceID: "pod-1",
			ProviderLifetimeStart: now.Add(-time.Hour), ProviderLifetimeEnd: now.Add(-time.Minute),
			ProviderAbsentAt: now, ProviderAbsenceReference: "absence:1", BillingStopReference: "stop:1",
			WindowsClosedAt: now, WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{}`),
		},
	}
	// base64 expands raw bytes by 4/3: this raw body fits, one more block does not.
	in.RawBody = make([]byte, openrails.ProviderBillingObservationMaxBytes*3/4-1024)
	require.NoError(t, validateProviderBillingInput(in))
	in.RawBody = make([]byte, openrails.ProviderBillingObservationMaxBytes*3/4)
	require.ErrorContains(t, validateProviderBillingInput(in), "limit is")

	for _, id := range []string{".", "..", "bad\x00id", string([]byte{0xff})} {
		in.OperationID = id
		require.Error(t, validateProviderBillingInput(in), "operation id %q", id)
	}
}

// Only the evidence qualifier may reach customer settlement: the settlement
// primitive is unexported, and its one SQL statement has exactly one caller.
func TestProviderSettlementHasOneQualifiedCaller(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(t, err)
	allowed := map[string]map[string]bool{
		"settlePassThroughProviderCostInTx":                   {"internal/modules/money/operation_authorization.go": true, "internal/modules/money/provider_billing.go": true},
		"SettleOperationAuthorizationPassThroughProviderCost": {"internal/modules/money/operation_authorization.go": true},
	}
	seen := map[string]int{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || rel == "internal/db/gen" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if files, guarded := allowed[sel.Sel.Name]; guarded {
				require.True(t, files[rel], "%s reaches %s", rel, sel.Sel.Name)
				seen[sel.Sel.Name]++
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, seen["settlePassThroughProviderCostInTx"])
	require.Equal(t, 1, seen["SettleOperationAuthorizationPassThroughProviderCost"])
}
