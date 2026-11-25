package controlplane

import (
	"context"
	"errors"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/userinfo"

	"github.com/open-rails/openrails/billing"
)

// Customers is merchant mid's customers of issuer as AuthKit knows them,
// keyed by their subject, the customer's id: a trusted issuer's are those it
// pushed over SCIM or whose tokens carried contact claims, in the merchant's
// group; the merchant's own ("") are ownCustomers.
func (c *ControlPlane) Customers(mid billing.MerchantID, issuer string) userinfo.Lookup {
	if issuer == "" {
		return ownCustomers{c: c, mid: mid}
	}
	return c.client.RemoteUserInfo(iam.GroupByID(mid.String()), issuer)
}

// ownCustomers are the merchant's own customers: this AuthKit's users, and
// the users its applications name in assertions (RFC 7523), whom AuthKit
// keeps under the application's issuer while the customer is the merchant's
// own. A user no AuthKit user is, whom two applications hold differently, is
// absent: which one names the customer is unknown.
type ownCustomers struct {
	c   *ControlPlane
	mid billing.MerchantID
}

func (o ownCustomers) Get(ctx context.Context, ids []string) (map[string]userinfo.User, error) {
	out, err := o.c.client.UserInfo().Get(ctx, ids)
	if err != nil {
		return nil, err
	}
	var rest []string
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			rest = append(rest, id)
		}
	}
	if len(rest) == 0 {
		return out, nil
	}
	issuers, err := o.c.TrustedIssuers(ctx, o.mid)
	if err != nil {
		return nil, err
	}
	held := map[string]userinfo.User{}
	ambiguous := map[string]bool{}
	for _, issuer := range issuers {
		found, err := o.c.client.RemoteUserInfo(iam.GroupByID(o.mid.String()), issuer).Get(ctx, rest)
		if err != nil {
			return nil, err
		}
		for id, u := range found {
			if prior, ok := held[id]; ok && prior != u {
				ambiguous[id] = true
			}
			held[id] = u
		}
	}
	for id, u := range held {
		if !ambiguous[id] {
			out[id] = u
		}
	}
	return out, nil
}

func (o ownCustomers) Search(ctx context.Context, query string, limit int) ([]userinfo.User, error) {
	out, err := o.c.client.UserInfo().Search(ctx, query, limit)
	if err != nil || len(out) >= limit {
		return out, err
	}
	issuers, err := o.c.TrustedIssuers(ctx, o.mid)
	if err != nil {
		return nil, err
	}
	native := len(out)
	seen := map[string]int{}
	for i, u := range out {
		seen[u.ID] = i
	}
	ambiguous := map[string]bool{}
	for _, issuer := range issuers {
		found, err := o.c.client.RemoteUserInfo(iam.GroupByID(o.mid.String()), issuer).Search(ctx, query, limit)
		if err != nil {
			return nil, err
		}
		for _, u := range found {
			if i, ok := seen[u.ID]; ok {
				// An AuthKit user is answered as Get answers it.
				ambiguous[u.ID] = ambiguous[u.ID] || i >= native && out[i] != u
				continue
			}
			seen[u.ID] = len(out)
			out = append(out, u)
		}
	}
	kept := out[:0]
	for _, u := range out {
		if !ambiguous[u.ID] {
			kept = append(kept, u)
		}
	}
	return kept[:min(len(kept), limit)], nil
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
