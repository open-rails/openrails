package controlplane

import (
	"context"
	"errors"
	"fmt"

	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// GroupDirectory is the read-only subset shared by an attached AuthKit client
// and AuthKit's lightweight directory. It owns all alias and expiry semantics.
type GroupDirectory interface {
	GroupInstanceForSlug(context.Context, authkit.GroupRef) (authkit.GroupInstance, error)
	GroupInstanceByID(context.Context, string) (authkit.GroupInstance, error)
}

type merchantNameAuthority struct{ groups GroupDirectory }

// MerchantNameAuthority adapts one AuthKit directory to the merchant persona.
func MerchantNameAuthority(groups GroupDirectory) merchant.NameAuthority {
	if groups == nil {
		return nil
	}
	return merchantNameAuthority{groups: groups}
}

func (a merchantNameAuthority) ResolveGroup(ctx context.Context, name string) (string, string, error) {
	group, err := a.groups.GroupInstanceForSlug(ctx, MerchantGroup(name))
	if errors.Is(err, authkit.ErrGroupNotFound) {
		return "", "", merchants.ErrMerchantNotFound
	}
	if err != nil {
		return "", "", err
	}
	if group.Persona != MerchantType {
		return "", "", merchants.ErrMerchantNotFound
	}
	return group.ID, group.InstanceSlug, nil
}
func (a merchantNameAuthority) GroupName(ctx context.Context, id string) (string, error) {
	group, err := a.groups.GroupInstanceByID(ctx, id)
	if errors.Is(err, authkit.ErrGroupNotFound) {
		return "", merchants.ErrMerchantNotFound
	}
	if err != nil {
		return "", err
	}
	if group.Persona != MerchantType {
		return "", merchants.ErrMerchantNotFound
	}
	return group.InstanceSlug, nil
}

func (a merchantNameAuthority) SearchGroups(ctx context.Context, query, afterName, afterID string, limit int) ([]merchant.GroupName, error) {
	search, ok := a.groups.(interface {
		SearchGroupInstances(context.Context, authkit.Persona, string, string, string, int) ([]authkit.GroupInstance, error)
	})
	if !ok {
		return nil, fmt.Errorf("authoritative merchant group search is not configured")
	}
	groups, err := search.SearchGroupInstances(ctx, MerchantType, query, afterName, afterID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]merchant.GroupName, 0, len(groups))
	for _, group := range groups {
		if group.Persona != MerchantType {
			return nil, merchants.ErrMerchantNotFound
		}
		out = append(out, merchant.GroupName{ID: group.ID, Name: group.InstanceSlug})
	}
	return out, nil
}

// MerchantGroupSearchResolver uses the CP-owned pool and AuthKit's canonical
// directory, without constructing another issuer or copying namespace SQL.
func (c *ControlPlane) MerchantGroupSearchResolver() merchants.GroupSearchResolver {
	return func(ctx context.Context, query, afterName, afterID string, limit int) ([]merchant.GroupName, error) {
		if c == nil || c.pool == nil {
			return nil, ErrNoControlPlane
		}
		directory, err := authcore.NewGroupDirectory(c.pool.Raw(), "")
		if err != nil {
			return nil, err
		}
		return (merchantNameAuthority{groups: directory}).SearchGroups(ctx, query, afterName, afterID, limit)
	}
}
