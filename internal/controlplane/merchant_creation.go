package controlplane

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
)

// MerchantCreationConfig declares a hosted deployment's policy for merchant
// names claimed by users (or#914): at creation and on rename. Operator paths
// (Bootstrap, manifests, ownerless provisioning) are never subject to it.
type MerchantCreationConfig struct {
	// ReservedSlugs are reserved IN ADDITION to billing.ReservedMerchantSlugs.
	ReservedSlugs []string
	// ReservedEscalationRole names the root-group role whose holders may claim
	// reserved names. Empty = reserved names are never user-claimable.
	ReservedEscalationRole string
	// SlugPattern further restricts user-claimed names: an unanchored regexp
	// source, anchored at construction. Empty = the built-in name rule only.
	SlugPattern string
	// Admission is the host COST gate for a new merchant, consulted with the
	// normalized name and the creating user id. Return an error to refuse.
	// nil = allow.
	Admission func(ctx context.Context, instanceSlug, ownerUserID string) error
}

// ReservedMerchantSlugs is the deployment's reserved namespace:
// billing.ReservedMerchantSlugs plus MerchantCreationConfig.ReservedSlugs.
func (c *ControlPlane) ReservedMerchantSlugs() []string {
	out := append([]string(nil), billing.ReservedMerchantSlugs...)
	if c != nil && c.merchantCreation != nil {
		for _, r := range c.merchantCreation.ReservedSlugs {
			out = append(out, billing.NormalizeMerchantSlug(r))
		}
	}
	return out
}

// authorizeNameClaim applies the reserved names and the creation pattern to a
// name claimed by a merchant principal; userID, when set, may hold the
// escalation role. No-op without a declared creation policy.
func (c *ControlPlane) authorizeNameClaim(ctx context.Context, name, userID string) error {
	userID = strings.TrimSpace(userID)
	if c == nil || c.merchantCreation == nil {
		return nil
	}
	name = billing.NormalizeMerchantSlug(name)
	if c.merchantCreationPattern != nil && !c.merchantCreationPattern.MatchString(name) {
		return fmt.Errorf("%w: slug %q does not match the deployment's creation pattern", billing.ErrMerchantSlugReserved, name)
	}
	if !slices.Contains(c.ReservedMerchantSlugs(), name) {
		return nil
	}
	if role := strings.TrimSpace(c.merchantCreation.ReservedEscalationRole); role != "" && userID != "" {
		held, err := c.holdsRootRole(ctx, userID, role)
		if err != nil || held {
			return err
		}
	}
	return fmt.Errorf("%w: %q", billing.ErrMerchantSlugReserved, name)
}

// holdsRootRole reports whether userID holds the root role named role, bare
// ("admin") or qualified ("root:admin").
func (c *ControlPlane) holdsRootRole(ctx context.Context, userID, role string) (bool, error) {
	roles, err := c.client.GroupRoles(ctx, iam.RootGroup(), []iam.Subject{iam.UserSubject(userID)})
	if err != nil {
		return false, err
	}
	held, ok := roles[iam.UserSubject(userID)]
	return ok && (strings.EqualFold(held.Name(), role) || strings.EqualFold(held.String(), role)), nil
}

// EnforceMerchantCreationPolicy applies the declared creation policy to a new
// merchant a user claims: the name claim rules, then the admission gate.
// No-op without a declared policy or for an operator (empty ownerUserID).
func (c *ControlPlane) EnforceMerchantCreationPolicy(ctx context.Context, name, ownerUserID string) error {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if c == nil || ownerUserID == "" {
		return nil
	}
	if err := c.authorizeNameClaim(ctx, name, ownerUserID); err != nil || c.merchantCreation == nil {
		return err
	}
	if admit := c.merchantCreation.Admission; admit != nil {
		if err := admit(ctx, billing.NormalizeMerchantSlug(name), ownerUserID); err != nil {
			return fmt.Errorf("%w: %w", billing.ErrMerchantCreationRefused, err)
		}
	}
	return nil
}
