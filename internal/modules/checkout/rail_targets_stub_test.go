package checkout

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
)

// stubRailTargets is the test implementation of checkoutRailTargets (or#893).
//
// The session path used to branch on the executor's concrete type — routing
// only when it was the real *CheckoutService and falling through to "the wire
// value IS the rail kind" otherwise — which meant the production PSP-resolution
// path was structurally unreachable from any test that used a fake executor.
// Multi-PSP target resolution is a REQUIREMENT of the executor now, so fakes
// embed this and the branch is gone.
//
// Resolution is deliberately trivial: a selector resolves to itself, on one
// armed account. Tests that need real routing behaviour use *CheckoutService.
type stubRailTargets struct {
	// psp, when set, is the account every selector resolves to. Otherwise a
	// fresh one is minted per call so provenance is never uuid.Nil.
	psp *merchants.PSPScope
}

func (s stubRailTargets) resolveRailTarget(_ context.Context, selector string) (railTarget, error) {
	name := strings.ToLower(strings.TrimSpace(selector))
	if name == "" {
		return railTarget{}, errors.New("rail is required")
	}
	scope := s.psp
	if scope == nil {
		scope = &merchants.PSPScope{ID: uuid.New(), Key: name, Rail: name}
	}
	return railTarget{PSP: name, Rail: name, Scope: scope}, nil
}

func (s stubRailTargets) pspKeyArchived(context.Context, string) bool { return false }

func (s stubRailTargets) railSource() railresolve.Source {
	return railresolve.FixedSet(config.PSPSet{})
}
