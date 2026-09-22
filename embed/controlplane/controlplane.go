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

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	corecp "github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/providerqualification"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Options configures the attached control plane.
type Options = operator.AttachOptions

type (
	MerchantCreationConfig               = operator.MerchantCreationConfig
	MerchantCreationPolicy               = operator.MerchantCreationPolicy
	BootstrapOptions                     = operator.BootstrapOptions
	BootstrapResult                      = operator.BootstrapResult
	ProvisionMerchantRequest             = operator.ProvisionMerchantRequest
	ProvisionMerchantResult              = operator.ProvisionMerchantResult
	MerchantRef                          = operator.MerchantRef
	PaymentProviderConfig                = operator.PaymentProviderConfig
	UpsertPaymentProviderConfigRequest   = operator.UpsertPaymentProviderConfigRequest
	ArchivePaymentProviderAccountRequest = operator.ArchivePaymentProviderAccountRequest
	// LastActiveProviderAccountError is ArchivePaymentProviderAccount's refusal
	// to archive the rail's only active account without AllowLast; match it
	// with errors.As.
	LastActiveProviderAccountError      = merchants.LastActiveProviderAccountError
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
	ProviderAccountCutoverDisposition   = operator.ProviderAccountCutoverDisposition
	ProviderAccountCutoverCode          = operator.ProviderAccountCutoverCode
	ProviderAccountCutoverPlan          = operator.ProviderAccountCutoverPlan
	ProviderAccountCutoverQuery         = operator.ProviderAccountCutoverQuery
	ProviderAccountCutoverReport        = operator.ProviderAccountCutoverReport
)

const (
	MerchantType = operator.MerchantType
	CustomerType = operator.CustomerType

	MerchantRetirementRefusedNotLive       = operator.MerchantRetirementRefusedNotLive
	MerchantRetirementRefusedGroupMismatch = operator.MerchantRetirementRefusedGroupMismatch
	MerchantRetirementRefusedReserved      = operator.MerchantRetirementRefusedReserved
	MerchantRetirementRefusedActive        = operator.MerchantRetirementRefusedActive

	ProviderAccountCutoverSameAccount     = operator.ProviderAccountCutoverSameAccount
	ProviderAccountCutoverRequiresReentry = operator.ProviderAccountCutoverRequiresReentry
	ProviderAccountCutoverBlocked         = operator.ProviderAccountCutoverBlocked

	ProviderAccountCutoverReady                     = operator.ProviderAccountCutoverReady
	ProviderAccountCutoverIdentityMissing           = operator.ProviderAccountCutoverIdentityMissing
	ProviderAccountCutoverRailUnsupported           = operator.ProviderAccountCutoverRailUnsupported
	ProviderAccountCutoverTargetRailMismatch        = operator.ProviderAccountCutoverTargetRailMismatch
	ProviderAccountCutoverSubscriptionNotRebilling  = operator.ProviderAccountCutoverSubscriptionNotRebilling
	ProviderAccountCutoverSubscriptionNotAtProvider = operator.ProviderAccountCutoverSubscriptionNotAtProvider
	ProviderAccountCutoverTargetArchived            = operator.ProviderAccountCutoverTargetArchived
	ProviderAccountCutoverSourceNotArchived         = operator.ProviderAccountCutoverSourceNotArchived
	ProviderAccountCutoverReplacementCardRequired   = operator.ProviderAccountCutoverReplacementCardRequired
	ProviderAccountCutoverReplacementCardNotFound   = operator.ProviderAccountCutoverReplacementCardNotFound
	ProviderAccountCutoverReplacementCardNotOwned   = operator.ProviderAccountCutoverReplacementCardNotOwned
	ProviderAccountCutoverReplacementCardUnusable   = operator.ProviderAccountCutoverReplacementCardUnusable
	ProviderAccountCutoverReplacementCardPSP        = operator.ProviderAccountCutoverReplacementCardPSP
	ProviderAccountCutoverCrossAccountNotQualified  = operator.ProviderAccountCutoverCrossAccountNotQualified
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
	// ErrProviderAccountCutoverNotQualified is the reason a cross-account plan
	// reports; it is never executed automatically.
	ErrProviderAccountCutoverNotQualified = operator.ErrProviderAccountCutoverNotQualified
	// ErrPaymentProviderNotFound is GetPaymentProviderConfig's answer when the
	// merchant has no active account on the rail; match it with errors.Is.
	ErrPaymentProviderNotFound = merchants.ErrPaymentProviderNotFound
	// ErrPaymentProviderAccountNotFound is ArchivePaymentProviderAccount's
	// answer for a PSP id the merchant does not own on that rail.
	ErrPaymentProviderAccountNotFound = merchants.ErrPaymentProviderAccountNotFound
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
// construction failure is fatal for a standalone or hosted process. Attach
// before Runtime.RunWorkers (managed) or Runtime.RiverJobs (host-owned).
// Its AuthKit workers, queues and schedules join that same composed fleet.
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
func (c *ControlPlane) Core() *authcore.Runtime { return c.cp.Core() }

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
// secret backend. The supplied credentials are live-probed before anything is
// written, so it cannot archive a dark account: use
// ArchivePaymentProviderAccount for that.
func (c *ControlPlane) UpsertPaymentProviderConfig(ctx context.Context, id merchant.ID, rail string, req UpsertPaymentProviderConfigRequest) (PaymentProviderConfig, error) {
	return operator.UpsertPaymentProviderConfig(ctx, c.app, id, rail, req)
}

// ListPaymentProviderConfigs returns the merchant's redacted provider accounts
// on rail ("" = every rail) with status "active", "archived" or "" (all) —
// the immutable `ID` each carries is what ArchivePaymentProviderAccount takes.
func (c *ControlPlane) ListPaymentProviderConfigs(ctx context.Context, id merchant.ID, rail, status string) ([]PaymentProviderConfig, error) {
	return operator.ListPaymentProviderConfigs(ctx, c.app, id, rail, status)
}

// ArchivePaymentProviderAccount archives exactly the account pspID on rail
// (#655 lifecycle, #656 emergency step 3). No provider call is made and the
// credentials stay, so a terminated account archives and its existing
// obligations keep draining. Archiving an archived account is a no-op. The
// rail's only active account is refused with LastActiveProviderAccountError
// unless req.AllowLast is set.
func (c *ControlPlane) ArchivePaymentProviderAccount(ctx context.Context, id merchant.ID, rail string, pspID uuid.UUID, req ArchivePaymentProviderAccountRequest) (PaymentProviderConfig, error) {
	return operator.ArchivePaymentProviderAccount(ctx, c.app, id, rail, pspID, req)
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

// PlanProviderAccountCutover reports how one subscriber could move to another
// provider account (#657). Executable only for an NMI subscription that still
// rebills, onto a ready replacement card vaulted by its own non-archived
// account (the durable payment-source update); every other case is a coded,
// non-executable plan, and a cross-account move is report-only card re-entry.
// Read-only: it resolves the subscription, the optional replacement method and
// both PSP rows and writes nothing.
func (c *ControlPlane) PlanProviderAccountCutover(ctx context.Context, id merchant.ID, q ProviderAccountCutoverQuery) (ProviderAccountCutoverReport, error) {
	return operator.PlanProviderAccountCutover(ctx, c.app, id, q)
}

// ProviderCutoverQualification identifies external proof for one immutable
// provider account; it is never inferred from a credential probe.
type ProviderCutoverQualification = providerqualification.Record

const NMIProviderCutoverContract = providerqualification.NMIContract

var (
	ErrProviderCutoverQualificationInvalid = providerqualification.ErrInvalid
	ErrProviderCutoverAccountNotFound      = providerqualification.ErrNotFound
)

// SetProviderCutoverQualification records external qualification on an existing
// PSP. Nil revokes. Revocation waits for the local HTTP call to return; it
// cannot cancel a request already sent. Later dispatches must qualify again.
func (c *ControlPlane) SetProviderCutoverQualification(ctx context.Context, merchantID merchant.ID, pspID uuid.UUID, qualification *ProviderCutoverQualification) error {
	return operator.SetProviderCutoverQualification(ctx, c.app, merchantID, pspID, qualification)
}
