package analytics

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestMerchantFilterScopesByMerchant proves the metrics query builder emits a
// merchant predicate bound to the resolved merchant id (issue #232). This is the
// per-merchant admin path: a merchant operator's metrics query is always pinned to
// WHERE merchant_id = ?.
func TestMerchantFilterScopesByMerchant(t *testing.T) {
	tid := merchant.ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	filter, args := merchantFilter(tid, false)

	if !strings.Contains(filter, "merchant_id = ?") {
		t.Fatalf("filter = %q, want it to contain a merchant_id predicate", filter)
	}
	if len(args) != 1 {
		t.Fatalf("args = %v, want exactly one bound merchant arg", args)
	}
	if got, ok := args[0].(string); !ok || got != tid.String() {
		t.Fatalf("bound arg = %v, want merchant id %q", args[0], tid.String())
	}
}

// TestMerchantFilterScopedMerchant proves a resolved merchant id is always scoped
// to WHERE merchant_id = ? rather than reading unscoped.
func TestMerchantFilterScopedMerchant(t *testing.T) {
	filter, args := merchantFilter(dbtest.TestMerchantID, false)
	if !strings.Contains(filter, "merchant_id = ?") {
		t.Fatalf("filter = %q, want a merchant_id predicate", filter)
	}
	if len(args) != 1 || args[0].(string) != dbtest.TestMerchantID.String() {
		t.Fatalf("args = %v, want merchant id %q", args, dbtest.TestMerchantID.String())
	}
}

// TestMerchantFilterCrossMerchantHasNoPredicate proves the platform-superadmin
// cross-merchant path emits NO merchant predicate, so it can read across all
// merchants (issue #232). This path is library-only and must be gated on
// controlplane.PermPlatformSuperadmin by its (future) caller.
func TestMerchantFilterCrossMerchantHasNoPredicate(t *testing.T) {
	filter, args := merchantFilter(dbtest.TestMerchantID, true)
	if filter != "" {
		t.Fatalf("cross-merchant filter = %q, want empty (no merchant predicate)", filter)
	}
	if len(args) != 0 {
		t.Fatalf("cross-merchant args = %v, want none", args)
	}
}
