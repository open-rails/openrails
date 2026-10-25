package routes

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
)

// ResolveExistingCustomer shares ordinary customer authority resolution, but
// never calls admission: no customer creation, issuer reassignment or network
// enrollment can be a side effect of proving the existing customer.
func ResolveExistingCustomer(ctx context.Context, rt *app.Runtime, authority auth.Authenticator, verified auth.Verified) (config.CustomerProof, error) {
	refused := func(err error) (config.CustomerProof, error) { return config.CustomerProof{}, err }
	if rt == nil || IsNilAuth(authority) {
		return refused(auth.ErrUnavailable)
	}
	if verified == nil {
		return refused(auth.ErrUnauthenticated)
	}
	if err := checkCustomerCredential(ctx, verified); err != nil {
		return refused(err)
	}
	id, err := identityOf(verified)
	if err != nil {
		return refused(err)
	}
	if id.SubjectKind != auth.SubjectUser || !id.SelfInvoked() || !billingauth.Interactive(id) || id.Credential.Kind == auth.CredentialPersonalAccessToken {
		return refused(billingauth.Refusal(billing.CodeInvokerScopedPrincipal))
	}
	customer, err := billing.ParseCustomerID(id.Subject)
	if err != nil || customer.IsZero() || customer.String() != id.Subject {
		return refused(billingauth.Refusal(billing.CodeAuthenticationRequired))
	}
	// OAuth customer proof is explicit even when the host declares this issuer.
	// Native sessions have no OAuth scope capability. Console/resource tokens
	// without customer self scope must not become merchant-side link proof.
	if scoped, ok := verified.(interface{ Scopes() []string }); ok && !slices.Contains(scoped.Scopes(), billing.ScopeSelf) {
		return refused(billingauth.Refusal(billing.CodePermissionRequired))
	}
	env := &Env{Runtime: rt, live: liveMerchant(rt)}
	target, err := env.boundMerchant(ctx, authority, verified, id)
	if err != nil {
		return refused(err)
	}
	owner, err := customerIssuer(ctx, authority, verified, id)
	if err != nil {
		return refused(err)
	}
	if !owner.self {
		return refused(billingauth.Refusal(billing.CodePermissionRequired))
	}
	if err := requireCustomerTenantFor(ctx, rt, target.MerchantID, customer, verified); err != nil {
		return refused(err)
	}
	issuer, err := db.ExistingCustomer(ctx, rt.DB.GenDirectory(), target.MerchantID.UUID(), customer.UUID(), owner.issuer)
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return refused(billingauth.Refusal(billing.CodeResourceNotFound))
		case errors.Is(err, db.ErrCustomerIssuerMismatch):
			return refused(billingauth.Refusal(billing.CodeCustomerIssuerMismatch))
		default:
			return refused(errors.Join(auth.ErrUnavailable, err))
		}
	}
	// Storage may wait. Recheck live source and merchant policy before handing
	// the binding to the consumer; no original request is authenticated again.
	current, err := env.boundMerchant(ctx, authority, verified, id)
	if err != nil {
		return refused(err)
	}
	if current.MerchantID != target.MerchantID {
		return refused(billingauth.Refusal(billing.CodeMerchantUnresolved))
	}
	if err := requireCustomerTenantFor(ctx, rt, target.MerchantID, customer, verified); err != nil {
		return refused(err)
	}
	if err := checkCustomerCredential(ctx, verified); err != nil {
		return refused(err)
	}
	return config.CustomerProof{MerchantID: target.MerchantID, CustomerID: customer, Issuer: issuer}, nil
}

func checkCustomerCredential(ctx context.Context, verified auth.Verified) (err error) {
	checker, ok := verified.(auth.CredentialChecker)
	if !ok {
		return auth.ErrForbidden
	}
	defer func() {
		if recover() != nil {
			err = errors.Join(auth.ErrUnavailable, fault("credential checker panicked"))
		}
	}()
	return checker.CheckCredential(ctx)
}
