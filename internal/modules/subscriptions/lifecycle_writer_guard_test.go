package subscriptions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// lifecycleDecisionWriters are the only functions that may name a lifecycle
// decision (#1091 part C). The state machine's Transition is the rule; the rest
// are explicit decisions outside it: an operator override, a plan change that
// supersedes a membership, a refund that revokes access, an engine takeover.
// Adding a writer means adding it here, in review.
var lifecycleDecisionWriters = map[string]bool{
	"internal/modules/subscriptions/upgrade.go:CompleteUpgradeTx":                  true,
	"internal/modules/subscriptions/admin_service.go:ExtendSubscriptionByDuration": true,
	"internal/modules/checkout/stripe_tier_change_intent.go:finalizeUpgrade":       true,
	"internal/intents/refund.go:revokeMembershipAccess":                            true,
	"internal/intents/nmi_engine_takeover.go:commit":                               true,
	"internal/modules/subscriptions/transition.go:Transition":                      true,
	"internal/modules/subscriptions/lifecycle_service.go:createMembershipCore":     true,
	"internal/modules/subscriptions/admin_service.go:UpdateSubscription":           true,
	"internal/modules/subscriptions/upgrade.go:SupersedeForUpgradeTx":              true,
	"internal/modules/webhooks/provider_refund_access.go:apply":                    true,
}

func TestLifecycleDecisionWriters(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "web", "node_modules", ".git", "sdk", "ci":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "MarkLifecycleDecision" {
					found = append(found, rel+":"+fn.Name.Name)
				}
				return true
			})
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(found)
	for _, site := range found {
		require.True(t, lifecycleDecisionWriters[site], "%s names a lifecycle decision outside the state machine; route it through Transition or add it to lifecycleDecisionWriters in review", site)
	}
	require.NotEmpty(t, found)
}
