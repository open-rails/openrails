package controlplane

// or#914 item 3: the hosted-SaaS MayCreateInstance predicate — the host cost
// gate behind ak#263's WithInstanceAdmission seam, composed from openrails'
// own state: verified email ALWAYS; a free allowance of owned merchants; and
// beyond it, a VAULTED payment method on file (setup-intent vault + Radar
// check, no charge — openrails holds the vault) unlocks more. This is the
// "card before your 3rd org" gate.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrEmailUnverified: the creating user has no verified email. Always
// enforced — an unverified account never claims a merchant name.
var ErrEmailUnverified = errors.New("merchant creation requires a verified email")

// ErrVaultedPaymentMethodRequired: the user is past the free allowance and has
// no vaulted payment method on file.
var ErrVaultedPaymentMethodRequired = errors.New("merchant creation beyond the free allowance requires a payment method on file")

// MerchantCreationPolicy parameterizes MerchantCreationAdmission.
type MerchantCreationPolicy struct {
	// FreeAllowance is how many merchant groups a user may OWN before the
	// vault gate applies (1-2 is the intended range). Must be positive.
	FreeAllowance int
	// HasVaultedPaymentMethod answers whether the user has a usable vaulted
	// payment method on file (no charge involved). Hosts whose vault is the
	// platform merchant's own openrails book can use
	// SubjectHasVaultedPaymentMethod. nil = nothing unlocks creation beyond
	// the allowance (fail closed).
	HasVaultedPaymentMethod func(ctx context.Context, subjectUserID string) (bool, error)
}

// MerchantCreationAdmission composes the or#914 hosted admission predicate for
// MerchantCreationConfig.Admission. Late-bound: the control plane is resolved
// per call (the predicate is constructed before Attach completes). Every
// unanswerable question refuses — admission is a judgment about identity and
// money, never made on an unanswered question. An already-owned group is an
// idempotent repair, not a creation event, so it returns before allowance and
// vault checks; stable group identity keeps that true across slug renames.
func MerchantCreationAdmission(a *app.App, policy MerchantCreationPolicy) (func(ctx context.Context, instanceSlug, ownerUserID string) error, error) {
	if a == nil {
		return nil, errors.New("merchant creation admission: app is required")
	}
	if policy.FreeAllowance <= 0 {
		return nil, fmt.Errorf("merchant creation admission: FreeAllowance must be positive, got %d", policy.FreeAllowance)
	}
	return func(ctx context.Context, instanceSlug, ownerUserID string) error {
		cp := Get(a)
		if cp == nil || cp.Core() == nil {
			return errors.New("control plane unavailable")
		}
		core := cp.Core()
		ownerUserID = strings.TrimSpace(ownerUserID)

		u, err := core.AdminGetUser(ctx, ownerUserID)
		if err != nil {
			return fmt.Errorf("resolve creating user: %w", err)
		}
		if !u.EmailVerified {
			return ErrEmailUnverified
		}

		memberships, err := core.ListSubjectGroups(ctx, authkit.UserSubject(ownerUserID))
		if err != nil {
			return fmt.Errorf("list user's merchant memberships: %w", err)
		}

		claimedGroupID, err := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantGroup(merchant.NormalizeSlug(instanceSlug)))
		switch {
		case err == nil:
			for _, membership := range memberships {
				if membership.Persona == controlplane.MerchantType &&
					strings.EqualFold(string(membership.Role), controlplane.MerchantRoleOwner) &&
					membership.GroupID == claimedGroupID {
					return nil
				}
			}
		case errors.Is(err, authkit.ErrGroupNotFound):
			// A fresh slug has no group identity to match; apply the creation gate.
		default:
			return fmt.Errorf("resolve claimed merchant group: %w", err)
		}

		owned := 0
		for _, m := range memberships {
			if m.Persona == controlplane.MerchantType && strings.EqualFold(string(m.Role), controlplane.MerchantRoleOwner) {
				owned++
			}
		}
		if owned < policy.FreeAllowance {
			return nil
		}
		if policy.HasVaultedPaymentMethod == nil {
			return fmt.Errorf("%w (allowance %d reached)", ErrVaultedPaymentMethodRequired, policy.FreeAllowance)
		}
		ok, err := policy.HasVaultedPaymentMethod(ctx, ownerUserID)
		if err != nil {
			return fmt.Errorf("vaulted payment method check: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w (allowance %d reached)", ErrVaultedPaymentMethodRequired, policy.FreeAllowance)
		}
		return nil
	}, nil
}

// SubjectHasVaultedPaymentMethod reports whether subjectUserID has an
// un-parked vaulted payment method on file with vaultMerchant (for a hosted
// product: its PLATFORM merchant — the book that treats hosted merchants'
// owners as customers). Runs under MerchantTx so RLS answers truthfully.
func SubjectHasVaultedPaymentMethod(ctx context.Context, a *app.App, vaultMerchant merchant.ID, subjectUserID string) (bool, error) {
	cp := Get(a)
	if cp == nil || cp.Pool() == nil {
		return false, errors.New("control plane unavailable")
	}
	subjectUserID = strings.TrimSpace(subjectUserID)
	if vaultMerchant.IsZero() || subjectUserID == "" {
		return false, errors.New("vault merchant and subject are required")
	}
	var vaulted bool
	err := cp.Pool().MerchantTx(ctx, vaultMerchant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				  FROM openrails.payment_methods pm
				  JOIN openrails.customers c ON c.id = pm.customer_id
				 WHERE pm.merchant_id = $1::uuid
				   AND c.subject = $2
				   AND pm.parked_at IS NULL)
		`, vaultMerchant.String(), subjectUserID).Scan(&vaulted)
	})
	return vaulted, err
}
