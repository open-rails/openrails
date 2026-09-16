// Package controlplane attaches the OpenRails-owned AuthKit control plane to an
// embedded runtime and exposes the operator mechanisms a hosted product
// composes: merchant provisioning and directory, provider configuration,
// fleet aggregates, retirement and the standalone HTTP surface. Ordinary
// billing goes through openrails.Client; hosts that bring their own AuthKit
// never import this package.
package controlplane

import (
	"context"
	"errors"
	"net/http"

	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	corecp "github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Options configures the attached control plane.
type Options = operator.AttachOptions

type (
	MerchantCreationConfig              = operator.MerchantCreationConfig
	MerchantCreationPolicy              = operator.MerchantCreationPolicy
	BootstrapOptions                    = operator.BootstrapOptions
	BootstrapResult                     = operator.BootstrapResult
	ProvisionMerchantRequest            = operator.ProvisionMerchantRequest
	ProvisionMerchantResult             = operator.ProvisionMerchantResult
	MerchantRef                         = operator.MerchantRef
	PaymentProviderConfig               = operator.PaymentProviderConfig
	UpsertPaymentProviderConfigRequest  = operator.UpsertPaymentProviderConfigRequest
	FleetSnapshot                       = operator.FleetSnapshot
	FleetSeries                         = operator.FleetSeries
	FleetMerchantFunnel                 = operator.FleetMerchantFunnel
	FleetRailHealth                     = operator.FleetRailHealth
	FleetCurrencyRevenue                = operator.FleetCurrencyRevenue
	FleetMRR                            = operator.FleetMRR
	FleetWeeklyPoint                    = operator.FleetWeeklyPoint
	FleetWeeklyVolume                   = operator.FleetWeeklyVolume
	MerchantRetirementCursor            = operator.MerchantRetirementCursor
	MerchantRetirementCandidatesRequest = operator.MerchantRetirementCandidatesRequest
	MerchantRetirementCandidate         = operator.MerchantRetirementCandidate
	MerchantRetirementCandidatePage     = operator.MerchantRetirementCandidatePage
	MerchantRetirementRefusal           = operator.MerchantRetirementRefusal
	RetireUnusedMerchantResult          = operator.RetireUnusedMerchantResult
)

const (
	MerchantType = operator.MerchantType
	CustomerType = operator.CustomerType

	MerchantRetirementRefusedNotLive       = operator.MerchantRetirementRefusedNotLive
	MerchantRetirementRefusedGroupMismatch = operator.MerchantRetirementRefusedGroupMismatch
	MerchantRetirementRefusedReserved      = operator.MerchantRetirementRefusedReserved
	MerchantRetirementRefusedActive        = operator.MerchantRetirementRefusedActive
)

var (
	ErrPermissionRequired           = operator.ErrPermissionRequired
	ErrMerchantUnresolved           = operator.ErrMerchantUnresolved
	ErrMerchantNotFound             = operator.ErrMerchantNotFound
	ErrInvalidSlug                  = operator.ErrInvalidSlug
	ErrSlugReserved                 = operator.ErrSlugReserved
	ErrCreationRefused              = operator.ErrCreationRefused
	ErrEmailUnverified              = operator.ErrEmailUnverified
	ErrVaultedPaymentMethodRequired = operator.ErrVaultedPaymentMethodRequired
	ErrMerchantGroupReleasePending  = operator.ErrMerchantGroupReleasePending
)

// MerchantGroup and CustomerGroup name the AuthKit persona groups.
func MerchantGroup(slug string) authkit.GroupRef       { return operator.MerchantGroup(slug) }
func CustomerGroup(customerID string) authkit.GroupRef { return operator.CustomerGroup(customerID) }
func CustomerGroupSlug(userID string) string           { return operator.CustomerGroupSlug(userID) }

// ControlPlane is the attached OpenRails control plane for one runtime.
type ControlPlane struct {
	app *app.App
	cp  *corecp.ControlPlane
}

func graph(rt *embed.Runtime) (*app.App, error) {
	if rt == nil || app.HostGraph == nil {
		return nil, errors.New("controlplane: runtime is required")
	}
	a := app.HostGraph(rt)
	if a == nil {
		return nil, errors.New("controlplane: runtime is not initialized")
	}
	return a, nil
}

// Attach builds the control plane over the runtime's database and Redis and
// wires it into the runtime's merchant directory. Attach once per runtime;
// construction failure is fatal for a standalone or hosted process.
func Attach(ctx context.Context, rt *embed.Runtime, opts Options) (*ControlPlane, error) {
	a, err := graph(rt)
	if err != nil {
		return nil, err
	}
	if operator.Get(a) != nil {
		return nil, errors.New("controlplane: already attached to this runtime")
	}
	if err := operator.AttachWithOptions(ctx, a, a.Config, a.Runtime.DB.Pool(), opts); err != nil {
		return nil, err
	}
	return &ControlPlane{app: a, cp: operator.Get(a)}, nil
}

// MerchantCreationAdmission composes the hosted merchant-creation predicate for
// Options.MerchantCreation. It is built before Attach and resolves the control
// plane per call.
func MerchantCreationAdmission(rt *embed.Runtime, policy MerchantCreationPolicy) (func(ctx context.Context, instanceSlug, ownerUserID string) error, error) {
	a, err := graph(rt)
	if err != nil {
		return nil, err
	}
	return operator.MerchantCreationAdmission(a, policy)
}

// SubjectHasVaultedPaymentMethod reports whether the subject has a usable
// vaulted payment method under vaultMerchant.
func SubjectHasVaultedPaymentMethod(ctx context.Context, rt *embed.Runtime, vaultMerchant merchant.ID, subjectUserID string) (bool, error) {
	a, err := graph(rt)
	if err != nil {
		return false, err
	}
	return operator.SubjectHasVaultedPaymentMethod(ctx, a, vaultMerchant, subjectUserID)
}

// Handler returns the full standalone HTTP surface: billing routes, the
// control plane's AuthKit routes and the admin console when configured.
func (c *ControlPlane) Handler() (http.Handler, error) {
	srv, err := operator.StandaloneServer(c.app)
	if err != nil {
		return nil, err
	}
	return srv.Handler(), nil
}

// Core is the control plane's AuthKit engine client.
func (c *ControlPlane) Core() *authcore.Client { return c.cp.Core() }

// UserAuthenticator verifies control-plane session tokens in process.
func (c *ControlPlane) UserAuthenticator() billingauth.Authenticator { return c.cp.UserAuthenticator() }

// JWKSHandler serves the control plane's signing keys for external verifiers.
func (c *ControlPlane) JWKSHandler() http.Handler { return c.cp.AuthService().JWKSHandler() }

// RunBootstrap idempotently installs the operator authority and permission
// catalog; it mints an initial API key only when opts asks for one.
func (c *ControlPlane) RunBootstrap(ctx context.Context, opts BootstrapOptions) (*BootstrapResult, error) {
	return c.cp.Bootstrap(ctx, opts)
}

// ProvisionMerchant idempotently creates a merchant and its AuthKit group.
func (c *ControlPlane) ProvisionMerchant(ctx context.Context, req ProvisionMerchantRequest) (*ProvisionMerchantResult, error) {
	return operator.ProvisionMerchant(ctx, c.app, req)
}

func (c *ControlPlane) SetMerchantDisplayName(ctx context.Context, id merchant.ID, displayName string) error {
	return operator.SetMerchantDisplayName(ctx, c.app, id, displayName)
}

// ListMerchantRefs resolves directory identity for merchants the caller
// already holds a membership in.
func (c *ControlPlane) ListMerchantRefs(ctx context.Context, slugs []string) ([]MerchantRef, error) {
	return operator.ListMerchantRefs(ctx, c.app, slugs)
}

// ListMerchantsForSubject returns the active merchants where subject has a
// customer record.
func (c *ControlPlane) ListMerchantsForSubject(ctx context.Context, subject string) ([]MerchantRef, error) {
	return operator.ListMerchantsForSubject(ctx, c.app, subject)
}

// ListActiveMerchantIDs pages the merchant directory for host-level background work.
func (c *ControlPlane) ListActiveMerchantIDs(ctx context.Context, limit, offset int) ([]merchant.ID, error) {
	return operator.ListActiveMerchantIDs(ctx, c.app, limit, offset)
}

// ResolveAuthorizedMerchant captures the merchant behind ref, then checks that
// userID holds permission on it. The returned slug is display metadata.
func (c *ControlPlane) ResolveAuthorizedMerchant(ctx context.Context, ref, userID, permission string) (merchant.ID, string, error) {
	return c.cp.ResolveAuthorizedMerchant(ctx, ref, userID, permission)
}

// ResolveMerchantForGroup captures the merchant UUID and canonical slug behind
// a group reference without any authority check.
func (c *ControlPlane) ResolveMerchantForGroup(ctx context.Context, ref string) (merchant.ID, string, error) {
	return c.cp.ResolveMerchantForGroup(ctx, ref)
}

// HasRootPermission reads live root-group membership for perm.
func (c *ControlPlane) HasRootPermission(ctx context.Context, userID, perm string) (bool, error) {
	return c.cp.HasRootPermission(ctx, userID, perm)
}

// EnsureCustomerPermissionGroup idempotently materializes the customer's
// AuthKit permission group and returns its id.
func (c *ControlPlane) EnsureCustomerPermissionGroup(ctx context.Context, customerID, ownerSubject string) (string, error) {
	return c.cp.EnsureCustomerPermissionGroup(ctx, customerID, ownerSubject)
}

func (c *ControlPlane) SetMerchantAPIHost(ctx context.Context, id merchant.ID, apiHost string) error {
	return operator.SetMerchantAPIHost(ctx, c.app, id, apiHost)
}

func (c *ControlPlane) GetMerchantAPIHost(ctx context.Context, id merchant.ID) (string, error) {
	return operator.GetMerchantAPIHost(ctx, c.app, id)
}

// GetPaymentProviderConfig returns one redacted provider account.
func (c *ControlPlane) GetPaymentProviderConfig(ctx context.Context, id merchant.ID, rail, environment string) (PaymentProviderConfig, error) {
	return operator.GetPaymentProviderConfig(ctx, c.app, id, rail, environment)
}

// UpsertPaymentProviderConfig arms a provider account through the merchant
// secret backend.
func (c *ControlPlane) UpsertPaymentProviderConfig(ctx context.Context, id merchant.ID, rail string, req UpsertPaymentProviderConfigRequest) (PaymentProviderConfig, error) {
	return operator.UpsertPaymentProviderConfig(ctx, c.app, id, rail, req)
}

// FleetAnalytics returns cross-merchant operator aggregates, excluding one merchant.
func (c *ControlPlane) FleetAnalytics(ctx context.Context, exclude merchant.ID, windowDays int) (*FleetSnapshot, error) {
	return operator.FleetAnalytics(ctx, c.app, exclude, windowDays)
}

func (c *ControlPlane) FleetTimeseries(ctx context.Context, exclude merchant.ID, weeks int) (*FleetSeries, error) {
	return operator.FleetTimeseries(ctx, c.app, exclude, weeks)
}

// ListMerchantRetirementCandidates pages unreserved live merchants with their
// activity facts; the host owns the dormancy policy over them.
func (c *ControlPlane) ListMerchantRetirementCandidates(ctx context.Context, req MerchantRetirementCandidatesRequest) (MerchantRetirementCandidatePage, error) {
	return operator.ListMerchantRetirementCandidates(ctx, c.app, req)
}

// RetireUnusedMerchant retires an inactive merchant still bound to groupID and
// releases its AuthKit group; refusals are reported in the result.
func (c *ControlPlane) RetireUnusedMerchant(ctx context.Context, id merchant.ID, groupID string) (RetireUnusedMerchantResult, error) {
	return operator.RetireUnusedMerchant(ctx, c.app, id, groupID)
}

func (c *ControlPlane) CompletePendingMerchantRetirements(ctx context.Context, limit int) (int, error) {
	return operator.CompletePendingMerchantRetirements(ctx, c.app, limit)
}
