package server

import (
	"context"
	"errors"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
)

// errNetworkOff is a network method on a server whose AuthKit declares no
// network-terms agreement.
var errNetworkOff = errors.New("server: the customer network is off: auth.agreements declares no network-terms")

// CustomerLinks are an openrails.dev account's active links, one per
// merchant: what a network portal lists.
func (s *Server) CustomerLinks(ctx context.Context, account billing.NetworkAccountID) ([]billing.CustomerLink, error) {
	if s.network == nil {
		return nil, errNetworkOff
	}
	return s.network.Links(ctx, account)
}

// UnlinkCustomer ends one of the account's links. It is
// billing.ErrNetworkLinkInUse while the customer's subscriptions at that
// merchant still charge its cards, and billing.ErrNotFound for a link the
// account does not hold. The merchant's clients lose the account's consent;
// the customer, its orders and subscriptions stay.
func (s *Server) UnlinkCustomer(ctx context.Context, account billing.NetworkAccountID, link billing.CustomerLinkID) (*billing.CustomerLink, error) {
	if s.network == nil {
		return nil, errNetworkOff
	}
	out, err := s.network.Unlink(ctx, account, link)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// networkHooks has AuthKit tell the network about accounts, before the
// host's own hooks: consents revoked and terms accepted (OnEvent), deleted
// accounts (OnPurge), and the refusals while linked customers' cards still
// pay subscriptions (DeletionCheck, ConsentRevocationCheck). Consent screens
// name a merchant's client by the merchant's name unless the host does.
func (s *Server) networkHooks(d *authkit.Deps) {
	if d.GroupName == nil {
		d.GroupName = s.merchantName
	}
	onEvent, onPurge, deletion, revocation := d.OnEvent, d.OnPurge, d.DeletionCheck, d.ConsentRevocationCheck
	d.OnEvent = func(ctx context.Context, e iam.Event) error {
		if s.network != nil {
			if err := s.network.OnEvent(ctx, e); err != nil {
				return err
			}
		}
		if onEvent != nil {
			return onEvent(ctx, e)
		}
		return nil
	}
	d.OnPurge = func(ctx context.Context, del iam.UserDeletion) error {
		if s.network != nil {
			if err := s.network.OnPurge(ctx, del); err != nil {
				return err
			}
		}
		if onPurge != nil {
			return onPurge(ctx, del)
		}
		return nil
	}
	d.DeletionCheck = func(ctx context.Context, userID string) error {
		if s.network != nil {
			if err := s.network.DeletionCheck(ctx, userID); err != nil {
				return err
			}
		}
		if deletion != nil {
			return deletion(ctx, userID)
		}
		return nil
	}
	d.ConsentRevocationCheck = func(ctx context.Context, userID, clientID string) error {
		if s.network != nil {
			if err := s.network.ConsentRevocationCheck(ctx, userID, clientID); err != nil {
				return err
			}
		}
		if revocation != nil {
			return revocation(ctx, userID, clientID)
		}
		return nil
	}
}

// merchantName is a merchant group's name: its display name, else its slug.
func (s *Server) merchantName(ctx context.Context, groupID string) (string, error) {
	mid, err := billing.ParseMerchantID(groupID)
	if err != nil {
		return "", err
	}
	m, err := s.GetMerchant(ctx, mid)
	if err != nil {
		return "", err
	}
	if m.DisplayName != nil {
		return *m.DisplayName, nil
	}
	return m.Slug, nil
}
