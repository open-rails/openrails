package openrails

import (
	"context"
	"errors"
	"net/http"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/operator"
)

// Control-plane operations, for hosted products running the engine with
// Config.ControlPlane: merchant provisioning and names, the merchant
// directory, fleet aggregates and retirement. They are local to the process:
// a remote Client refuses them with ErrRemoteClient.

// ErrNoControlPlane refuses a control-plane operation on an engine built
// without Config.ControlPlane.
var ErrNoControlPlane = errors.New("openrails: no control plane; set Config.ControlPlane")

func (c *Client) controlPlane() (*app.App, *controlplane.ControlPlane, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, nil, err
	}
	cp := operator.Get(e.App)
	if cp == nil {
		return nil, nil, ErrNoControlPlane
	}
	return e.App, cp, nil
}

// AuthKit is the control plane's AuthKit client, nil without one.
func (c *Client) AuthKit() *authkit.Client {
	if _, cp, err := c.controlPlane(); err == nil {
		return cp.Core()
	}
	return nil
}

// AuthenticateUser verifies a control-plane user session on r in process.
func (c *Client) AuthenticateUser(r *http.Request) (Identity, error) {
	a, cp, err := c.controlPlane()
	if err != nil {
		return Identity{}, err
	}
	authenticator := cp.UserAuthenticator()
	if authenticator == nil {
		return Identity{}, ErrUnauthenticated
	}
	user, err := authenticator.Authenticate(r.Context(), r)
	if err != nil {
		return Identity{}, err
	}
	if _, err := billing.ParseCustomerID(user.UserID); err != nil {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{
		Issuer: a.Config.ControlPlane.Auth.Issuer, Subject: user.UserID, SubjectKind: SubjectUser,
		Credential: Credential{Kind: CredentialSession, ID: user.SessionID},
		Email:      user.Email, EmailVerified: user.EmailVerified, Username: user.Username,
	}, nil
}

// ProvisionMerchant returns the merchant a name resolves to, or creates one
// claiming it, bound to a new merchant permission group owned by
// req.OwnerUserID. A user claim answers to Config.ControlPlane.MerchantCreation.
// A merchant's own changes afterwards (its name, display name, API host) go
// through its routes.
func (c *Client) ProvisionMerchant(ctx context.Context, req billing.ProvisionMerchantParams) (*billing.ProvisionMerchantResult, error) {
	a, _, err := c.controlPlane()
	if err != nil {
		return nil, err
	}
	return operator.ProvisionMerchant(ctx, a, req)
}

// SetMerchantAPIHost binds the host name requests resolve to this merchant
// from, as the operator and without the DNS proof SetAPIHost asks of a
// merchant: a host of the deployment's own. Empty clears it.
func (c *Client) SetMerchantAPIHost(ctx context.Context, id billing.MerchantID, apiHost string) error {
	a, _, err := c.controlPlane()
	if err != nil {
		return err
	}
	return operator.SetMerchantAPIHost(ctx, a, id, apiHost)
}

// ListMerchantsForSubject returns the live merchants where subject is a
// customer.
func (c *Client) ListMerchantsForSubject(ctx context.Context, subject string) ([]billing.MerchantRef, error) {
	a, _, err := c.controlPlane()
	if err != nil {
		return nil, err
	}
	return operator.ListMerchantsForSubject(ctx, a, subject)
}

// ListUserMerchants returns the live merchants the user authenticated by r
// holds a staff or owner role in. It checks the sign-in is still active and
// reads current memberships; customer relationships are a separate listing.
// This is a local control-plane operation, not a remote Client method.
func (c *Client) ListUserMerchants(ctx context.Context, r *http.Request) ([]billing.UserMerchant, error) {
	_, cp, err := c.controlPlane()
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrUnauthenticated
	}
	claims, err := cp.Core().VerifyRequest(r.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	actor, ok := verify.ActorFromClaims(claims)
	if !ok || actor.Kind() != iam.ActorUser {
		return nil, ErrUnauthenticated
	}
	// Listing memberships takes a user ID, unlike Can's session-bound actor.
	// Check the verified session explicitly before crossing that boundary.
	if err := cp.Core().CheckSession(ctx, claims); err != nil {
		if errors.Is(err, iam.ErrSessionRevoked) {
			err = errors.Join(err, helpersauth.ErrRevoked)
		}
		return nil, err
	}
	return cp.ListUserMerchants(ctx, actor.ID())
}

// ListActiveMerchantIDs pages the live merchants, newest first, for host
// background work.
func (c *Client) ListActiveMerchantIDs(ctx context.Context, page billing.PageRequest) (*billing.ListPage[billing.MerchantID], error) {
	a, _, err := c.controlPlane()
	if err != nil {
		return nil, err
	}
	return operator.ListActiveMerchantIDs(ctx, a, page)
}

// ResolveAuthorizedMerchant captures the merchant behind ref (the user's sole
// merchant when empty), then checks live that the user r authenticates as
// holds permission on it. The slug is display metadata; carry the ID.
func (c *Client) ResolveAuthorizedMerchant(ctx context.Context, r *http.Request, ref, permission string) (billing.MerchantID, string, error) {
	_, cp, err := c.controlPlane()
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	return cp.ResolveAuthorizedMerchant(ctx, r, ref, permission)
}

// ResolveMerchantForGroup captures the merchant ID and canonical slug behind a
// group reference, without an authority check.
func (c *Client) ResolveMerchantForGroup(ctx context.Context, ref string) (billing.MerchantID, string, error) {
	_, cp, err := c.controlPlane()
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	return cp.ResolveMerchantForGroup(ctx, ref)
}

// HasRootPermission checks live whether the user r authenticates as holds
// permission in the root group.
func (c *Client) HasRootPermission(ctx context.Context, r *http.Request, permission string) (bool, error) {
	_, cp, err := c.controlPlane()
	if err != nil {
		return false, err
	}
	return cp.HasRootPermission(ctx, r, permission)
}

// EnsureCustomerPermissionGroup idempotently creates the customer's portal
// group (its ID is customerID) owned by ownerSubject and returns its ID.
func (c *Client) EnsureCustomerPermissionGroup(ctx context.Context, customerID, ownerSubject string) (string, error) {
	_, cp, err := c.controlPlane()
	if err != nil {
		return "", err
	}
	return cp.EnsureCustomerPermissionGroup(ctx, customerID, ownerSubject)
}

// SubjectHasVaultedPaymentMethod reports whether subject has a usable vaulted
// payment method with vaultMerchant; a host implements
// Deps.HasVaultedPaymentMethod with it.
func (c *Client) SubjectHasVaultedPaymentMethod(ctx context.Context, vaultMerchant billing.MerchantID, subject string) (bool, error) {
	a, _, err := c.controlPlane()
	if err != nil {
		return false, err
	}
	return operator.SubjectHasVaultedPaymentMethod(ctx, a, vaultMerchant, subject)
}

// FleetAnalytics returns cross-merchant aggregates over the last windowDays
// (1..365; anything else is billing.ErrInvalid), excluding one merchant. The
// caller gates and audits it.
func (c *Client) FleetAnalytics(ctx context.Context, exclude billing.MerchantID, windowDays int) (*billing.FleetSnapshot, error) {
	a, _, err := c.controlPlane()
	if err != nil {
		return nil, err
	}
	return operator.FleetAnalytics(ctx, a, exclude, windowDays)
}

// FleetTimeseries returns the weekly fleet trend over 4..52 weeks (anything
// else is billing.ErrInvalid), excluding one merchant. The caller gates and
// audits it.
func (c *Client) FleetTimeseries(ctx context.Context, exclude billing.MerchantID, weeks int) (*billing.FleetSeries, error) {
	a, _, err := c.controlPlane()
	if err != nil {
		return nil, err
	}
	return operator.FleetTimeseries(ctx, a, exclude, weeks)
}

// ListMerchantRetirementCandidates pages unreserved live merchants with their
// activity facts; the host owns the dormancy policy over them.
func (c *Client) ListMerchantRetirementCandidates(ctx context.Context, req billing.MerchantRetirementCandidateListParams) (billing.MerchantRetirementCandidatePage, error) {
	a, _, err := c.controlPlane()
	if err != nil {
		return billing.MerchantRetirementCandidatePage{}, err
	}
	return operator.ListMerchantRetirementCandidates(ctx, a, req)
}

// RetireUnusedMerchant retires an inactive merchant still bound to groupID and
// releases its group; refusals are reported in the result.
func (c *Client) RetireUnusedMerchant(ctx context.Context, id billing.MerchantID, groupID string) (billing.MerchantRetirement, error) {
	a, _, err := c.controlPlane()
	if err != nil {
		return billing.MerchantRetirement{}, err
	}
	return operator.RetireUnusedMerchant(ctx, a, id, groupID)
}

// CompletePendingMerchantRetirements retries group releases of committed
// retirements, up to limit, and reports how many completed.
func (c *Client) CompletePendingMerchantRetirements(ctx context.Context, limit int) (int, error) {
	a, _, err := c.controlPlane()
	if err != nil {
		return 0, err
	}
	return operator.CompletePendingMerchantRetirements(ctx, a, limit)
}
