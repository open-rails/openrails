package operator

// Merchant retirement host seam. Core supplies activity facts and the safe,
// irreversible retire mechanism; a hosted product owns dormancy policy
// (warnings, cadence, arming) and its state. These are privileged host
// operations: callers authorize and audit their use.

import (
	"context"
	"errors"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
)

// ListMerchantRetirementCandidates pages live, group-bound merchants created
// before req.CreatedBefore, excluding the deployment's reserved slugs, each with
// its current activity fact.
func ListMerchantRetirementCandidates(ctx context.Context, a *app.App, req billing.MerchantRetirementCandidatesRequest) (billing.MerchantRetirementCandidatePage, error) {
	cp, dir, err := retirementDirectory(a)
	if err != nil {
		return billing.MerchantRetirementCandidatePage{}, err
	}
	return dir.ListRetirementCandidates(ctx, req, cp.ReservedMerchantSlugs())
}

// RetireUnusedMerchant retires a live, unreserved merchant with no activity that
// is still bound to groupID, then deletes exactly that AuthKit group with its
// slug released. Refusals are reported in the result, not as errors.
func RetireUnusedMerchant(ctx context.Context, a *app.App, merchantID billing.MerchantID, groupID string) (billing.MerchantRetirement, error) {
	cp, dir, err := retirementDirectory(a)
	if err != nil {
		return billing.MerchantRetirement{}, err
	}
	return dir.RetireUnused(ctx, merchantID, groupID, cp.ReservedMerchantSlugs(), groupReleaser(cp))
}

// CompletePendingMerchantRetirements finishes up to limit committed retirements
// whose group release is pending, by captured group UUID.
func CompletePendingMerchantRetirements(ctx context.Context, a *app.App, limit int) (int, error) {
	cp, dir, err := retirementDirectory(a)
	if err != nil {
		return 0, err
	}
	return dir.CompletePendingGroupReleases(ctx, limit, groupReleaser(cp))
}

func retirementDirectory(a *app.App) (*controlplane.ControlPlane, *merchants.Service, error) {
	cp := Get(a)
	if cp == nil || cp.Core() == nil {
		return nil, nil, errors.New("merchant retirement: no control plane attached (call Attach first)")
	}
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return nil, nil, err
	}
	return cp, dir, nil
}

// groupReleaser deletes a retired merchant's group; one already gone is
// released.
func groupReleaser(cp *controlplane.ControlPlane) merchants.GroupReleaser {
	core := cp.Core()
	return func(ctx context.Context, groupID string) error {
		if err := core.DeleteGroup(ctx, iam.GroupByID(groupID)); err != nil && !errors.Is(err, iam.ErrGroupNotFound) {
			return err
		}
		return nil
	}
}
