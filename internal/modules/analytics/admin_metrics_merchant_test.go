package analytics

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestMerchantFilterScopesByMerchant proves the metrics query builder emits a
// merchant predicate bound to the resolved merchant id (issue #232). This is the
// per-merchant admin path: a merchant operator's metrics query is always pinned to
// WHERE merchant_id = ?.
func TestMerchantFilterScopesByMerchant(t *testing.T) {
	tid := merchant.ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	filter, args := merchantFilter(tid)

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
