package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/requestauth"
)

// ResolvedDelegated is a verified browser-direct delegated access token
// (issue #222): the acting end user (the token's delegated_sub) and the
// OpenRails merchant whose issuer signed it.
type ResolvedDelegated = credential.ResolvedDelegated

var ResolvedDelegatedFromHostPrincipal = credential.ResolvedDelegatedFromHostPrincipal

// ErrDelegatedNotConfigured indicates the control plane has no delegated-token
// verifier: a wiring bug (#469), so it fails closed.
var ErrDelegatedNotConfigured = credential.ErrDelegatedNotConfigured

// ErrDelegatedInvalid is the sanitized error for every delegated-token
// rejection other than expiry, sender proof and an unknown issuer. It never
// leaks verifier detail.
var ErrDelegatedInvalid = credential.ErrDelegatedInvalid

// ErrDelegatedUnavailable means sender-proof replay protection could not be
// consulted. A caller must fail closed without treating it as invalid credentials.
var ErrDelegatedUnavailable = credential.ErrDelegatedUnavailable

// ErrDelegatedIssuerUnknown indicates a verified credential's issuer is no
// enabled remote application whose controlling group backs an active merchant.
var ErrDelegatedIssuerUnknown = credential.ErrDelegatedIssuerUnknown

// ResolveDelegated verifies a browser-direct delegated access token for the
// self-service/admin surface. AuthKit's verifier checks the signature,
// audience, expiry and sender proof, requires a delegated token signed by one
// of its enabled remote applications, and bounds its permissions to that
// application's stored authority (#564). The merchant is the one the
// application's controlling group backs: the token carries no merchant claim,
// so renames never invalidate it (#259).
//
// Errors: iam.ErrTokenExpired joined with auth.ErrExpired for an expired token;
// ErrDelegatedIssuerUnknown when the issuer backs no active merchant (or
// another merchant than the request's Host, #734); ErrDelegatedInvalid for any
// other rejection.
func (c *ControlPlane) ResolveDelegated(r *http.Request) (*ResolvedDelegated, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrDelegatedNotConfigured
	}
	if r == nil {
		return nil, ErrDelegatedInvalid
	}
	ctx := r.Context()
	cl, err := requestauth.Once(ctx, c.delegatedVerifier, func() (verify.Claims, error) { return c.delegatedVerifier.VerifyRequest(r) })
	switch {
	case errors.Is(err, verify.ErrSenderProofUnavailable):
		return nil, ErrDelegatedUnavailable
	case errors.Is(err, verify.ErrSenderProofRequired):
		return nil, errors.Join(verify.ErrSenderProofRequired, helpersauth.ErrSenderProofRequired)
	case errors.Is(err, iam.ErrTokenExpired):
		return nil, errors.Join(iam.ErrTokenExpired, helpersauth.ErrExpired)
	case err != nil:
		return nil, ErrDelegatedInvalid
	}
	subject := strings.TrimSpace(cl.DelegatedSubject)
	if cl.Kind != iam.ActorDelegated || cl.UserID != "" || subject == "" {
		return nil, ErrDelegatedInvalid
	}
	// Wire delegation always has a sender binding (#222).
	if cl.CertificateThumbprint == "" && cl.JWKThumbprint == "" {
		return nil, errors.Join(verify.ErrSenderProofRequired, helpersauth.ErrSenderProofRequired)
	}
	mid, slug, err := c.merchantForApplication(ctx, cl)
	if err != nil {
		return nil, err
	}
	class := billingauth.CredentialClassUnknown
	if raw, present := cl.Attributes[billingauth.DelegatedCredentialClassAttribute]; present {
		if err := json.Unmarshal(raw, &class); err != nil || (class != billingauth.CredentialClassUserSession && class != billingauth.CredentialClassAutomation) {
			return nil, ErrDelegatedInvalid
		}
	}
	customerID, err := c.TouchCustomer(ctx, mid, cl.Issuer, subject)
	if err != nil {
		return nil, err
	}
	return &ResolvedDelegated{
		CredentialClass:      class,
		Merchant:             slug,
		MerchantID:           mid,
		MerchantSlug:         slug,
		CustomerID:           customerID,
		DelegatedSubject:     subject,
		Issuer:               strings.TrimSpace(cl.Issuer),
		Permissions:          append([]string(nil), cl.Permissions...),
		Email:                delegatedStringAttribute(cl.Attributes, "email"),
		EmailVerified:        delegatedBoolAttribute(cl.Attributes, "email_verified"),
		Username:             delegatedStringAttribute(cl.Attributes, "username"),
		SolanaAddress:        delegatedStringAttribute(cl.Attributes, "solana_address"),
		SolanaPrimarySNSName: delegatedStringAttribute(cl.Attributes, "solana_primary_sns_name"),
		SolanaVerifiedAt:     delegatedStringAttribute(cl.Attributes, "solana_verified_at"),
	}, nil
}

// merchantForApplication resolves the merchant a remote application's verified
// credential acts for (#567): the active merchant bound to the application's
// controlling group. When a Host resolver pinned a merchant onto ctx (#734),
// it must be that one. Anything else is ErrDelegatedIssuerUnknown.
func (c *ControlPlane) merchantForApplication(ctx context.Context, cl verify.Claims) (billing.MerchantID, string, error) {
	if cl.RemoteApplicationID == "" || cl.Group == nil {
		return billing.MerchantID{}, "", ErrDelegatedIssuerUnknown
	}
	return c.merchantForAppGroup(ctx, cl.Group.GroupID)
}

func (c *ControlPlane) merchantForAppGroup(ctx context.Context, groupID string) (billing.MerchantID, string, error) {
	if c.pool == nil {
		return billing.MerchantID{}, "", errors.New("controlplane: control plane unavailable for issuer resolution")
	}
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return billing.MerchantID{}, "", ErrDelegatedIssuerUnknown
	}
	mid, slug, err := c.merchantDirectoryRow(gen.New(c.pool).ListLiveMerchantsByGroupID(ctx, groupID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return billing.MerchantID{}, "", ErrDelegatedIssuerUnknown
	case err != nil:
		return billing.MerchantID{}, "", err
	}
	if hostMID, ok := merchant.HostMerchant(ctx); ok && hostMID != mid {
		return billing.MerchantID{}, "", ErrDelegatedIssuerUnknown
	}
	return mid, slug, nil
}

func delegatedStringAttribute(attrs map[string]json.RawMessage, key string) string {
	var value string
	if raw, ok := attrs[key]; !ok || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func delegatedBoolAttribute(attrs map[string]json.RawMessage, key string) bool {
	var value bool
	if raw, ok := attrs[key]; !ok || json.Unmarshal(raw, &value) != nil {
		return false
	}
	return value
}
