package routes

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
)

// A shared issuer does not make every one of its accounts this merchant's
// customer. The authenticated source tenant must match explicit ownership
// before customer admission can create or touch a row. Staff use their own
// route policy; a global operator is never manufactured into a customer.
func requireCustomerTenant(ctx context.Context, rt *app.Runtime, merchant billing.MerchantID, customer billing.CustomerID) error {
	row, err := rt.DB.GenDirectory().GetMerchantTenantBinding(ctx, merchant.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		return billingauth.Refusal(billing.CodeMerchantUnresolved)
	}
	if err != nil {
		return errors.Join(auth.ErrUnavailable, err)
	}
	if row.TenantID == nil {
		return nil // the explicit legacy, unbound mode
	}
	verified, ok := billingauth.VerifiedFrom(ctx)
	if !ok {
		return billingauth.Refusal(billing.CodeCustomerTenantMismatch)
	}
	identity, err := identityOf(verified)
	if err != nil {
		return err
	}
	if identity.SubjectKind != auth.SubjectUser || identity.Subject != customer.String() || identity.TenantID != row.TenantID.String() {
		return billingauth.Refusal(billing.CodeCustomerTenantMismatch)
	}
	return nil
}
