package engine

import (
	"context"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/email"
	"github.com/open-rails/openrails/internal/operator"
)

// attachControlPlane builds the OpenRails-owned AuthKit control plane over the
// engine's database and Redis. Its workers join the engine's River fleet.
func attachControlPlane(ctx context.Context, a *app.App, cp config.ControlPlaneConfig, deps config.Deps) error {
	auth := cp.Auth
	opts := operator.AttachOptions{
		Auth:                         &auth,
		Registration:                 cp.Registration,
		PasswordlessLogin:            cp.PasswordlessLogin,
		PasswordlessAutoRegistration: cp.PasswordlessAutoRegistration,
		Frontend:                     authkit.FrontendConfig{BaseURL: cp.FrontendBaseURL},
		TrustedProxies:               cp.TrustedProxies,
		CloudflareProxies:            cp.CloudflareProxies,
		DirectPeerIP:                 auth.DirectPeerIP,
	}
	if sender := a.Runtime.EmailSender; sender != nil {
		opts.EmailSender = email.AuthKitSender{Sender: sender}
	}
	if deps.SMS != nil {
		opts.SMSSender = deps.SMS
	}
	if len(cp.AuthRateLimits) > 0 {
		opts.AuthRateLimitOverrides = make(map[string]authkit.RateLimit, len(cp.AuthRateLimits))
		for name, limit := range cp.AuthRateLimits {
			opts.AuthRateLimitOverrides[name] = authkit.RateLimit{Limit: limit.Limit, Window: limit.Window, Cooldown: limit.Cooldown}
		}
	}
	if creation := cp.MerchantCreation; creation != nil {
		policy := controlplane.MerchantCreationConfig{
			ReservedSlugs:          creation.ReservedSlugs,
			ReservedEscalationRole: creation.ReservedEscalationRole,
			SlugPattern:            creation.SlugPattern,
		}
		if creation.FreeAllowance > 0 {
			admission, err := operator.MerchantCreationAdmission(a, operator.MerchantCreationPolicy{
				FreeAllowance:           creation.FreeAllowance,
				HasVaultedPaymentMethod: deps.HasVaultedPaymentMethod,
			})
			if err != nil {
				return err
			}
			policy.Admission = admission
		}
		opts.MerchantCreation = &policy
	}
	return operator.AttachWithOptions(ctx, a, a.Config, a.Runtime.DB.Pool(), opts)
}
