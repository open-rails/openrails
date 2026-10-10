package controlplane

import (
	"context"
	"errors"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/helpers/userinfo"

	"github.com/open-rails/openrails/billing"
)

// Customers is merchant mid's customers of issuer as AuthKit knows them,
// keyed by their subject, the customer's id: "" is this AuthKit's own
// users; a trusted issuer's are those it pushed over SCIM or whose tokens
// carried contact claims, in the merchant's group.
func (c *ControlPlane) Customers(mid billing.MerchantID, issuer string) userinfo.Lookup {
	if issuer == "" {
		return c.client.UserInfo()
	}
	return c.client.RemoteUserInfo(iam.GroupByID(mid.String()), issuer)
}

// TrustedIssuers are the issuers merchant mid's group trusts.
func (c *ControlPlane) TrustedIssuers(ctx context.Context, mid billing.MerchantID) ([]string, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	var out []string
	for app, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.RemoteApplication], error) {
		return c.client.ListRemoteApplications(ctx, iam.GroupByID(mid.String()), p)
	}) {
		if errors.Is(err, iam.ErrGroupNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, app.Issuer)
	}
	return out, nil
}
