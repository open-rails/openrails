package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/httpx"
	"github.com/open-rails/openrails/pkg/merchant"
)

type ManagedStripeWebhookParams struct {
	Config              *config.Config
	SecretStore         merchants.MerchantSecretStore
	MerchantID          merchant.ID
	MerchantSlug        string
	ProviderEnvironment string
	PspID               string
	SecretKey           string
	EnabledEvents       []string
	StripeBaseURL       string
	// Now overrides the rollover clock (tests). Zero = time.Now().
	Now time.Time
	// AllowRetire lets this pass DELETE endpoints that a successor replaced
	// longer ago than RetireOverlap. False (the default, and what the kill
	// switch produces) means the pass is purely additive.
	AllowRetire bool
	// RetireOverlap overrides WebhookRolloverOverlap.
	RetireOverlap time.Duration
}

type ManagedStripeWebhookResult struct {
	Result     WebhookReconcileResult
	Skipped    bool
	SkipReason string
	SecretName string
	WebhookURL string
	// RepairedFrom names the secret this pass copied into the current derived
	// name because the derived name had drifted (a psps row's environment or
	// account_id changed). Non-empty means a local record was repaired instead
	// of a remote endpoint being replaced.
	RepairedFrom string
	// Retired are superseded endpoints this pass deleted (AllowRetire only).
	Retired []string
	// RetirePending are superseded endpoints still inside their overlap window,
	// or held because retirement was not allowed.
	RetirePending []SupersededEndpoint
	// OperatorAction is a non-empty human sentence when the pass needs a person:
	// a rollover is in flight, or the endpoint budget is exhausted.
	OperatorAction string
}

// PublicStripeWebhookURL builds the inbound Stripe webhook URL for a merchant.
// When accountID is set it returns the per-account endpoint
// (…/merchants/{slug}/webhooks/stripe/{account_id}, #641) so a merchant with
// multiple Stripe accounts gets one managed endpoint each; empty accountID
// returns the shared …/webhooks/stripe path.
//
// or#893: the slug form is the EMBEDDED surface. Standalone stopped mounting
// the merchant-slug alias — its one surface is …/webhooks/stripe/{account_id},
// with the merchant derived from that globally unique account. Every managed-
// endpoint caller today passes a slug, and every Stripe consumer is embedded,
// so this is correct as written; a standalone deployment that adopts managed
// Stripe registration must pass an empty slug and get the account-derived path
// (the else-branch below then needs the account segment too).
func PublicStripeWebhookURL(cfg *config.Config, merchantSlug, accountID string) (string, bool, error) {
	base := ""
	if cfg != nil {
		base = strings.TrimSpace(cfg.APIURL)
	}
	if base == "" {
		return "", false, nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false, fmt.Errorf("invalid api_url %q", base)
	}
	// #SEC-21: Stripe must be able to REACH this endpoint, so it has to be a
	// public https host. The routability rule is the shared outbound policy —
	// the local check here used to miss 169.254/16 and 100.64/10.
	if u.Scheme != "https" {
		return "", false, nil
	}
	if err := (httpx.Policy{}).ValidateURL(base); err != nil {
		return "", false, nil
	}
	parts := []string{"v1"}
	if strings.TrimSpace(merchantSlug) != "" {
		parts = append(parts, "merchants", merchantSlug, "webhooks", "stripe")
		if id := strings.TrimSpace(accountID); id != "" {
			parts = append(parts, id) // #641 per-account endpoint
		}
	} else {
		parts = append(parts, "webhooks", "stripe")
	}
	out, err := url.JoinPath(strings.TrimRight(base, "/"), parts...)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

func ReconcileManagedStripeWebhook(ctx context.Context, p ManagedStripeWebhookParams) (ManagedStripeWebhookResult, error) {
	if p.Config != nil && p.Config.IsLimitedMode() {
		return ManagedStripeWebhookResult{Skipped: true, SkipReason: "provider writes disabled"}, nil
	}
	webhookURL, ok, err := PublicStripeWebhookURL(p.Config, p.MerchantSlug, p.PspID)
	if err != nil {
		return ManagedStripeWebhookResult{}, err
	}
	if !ok {
		return ManagedStripeWebhookResult{Skipped: true, SkipReason: "public webhook url not configured"}, nil
	}

	secretKey := strings.TrimSpace(p.SecretKey)
	var secretName, previousName, repairedFrom, currentSecret string
	haveSecret := false
	if p.SecretStore != nil && !p.MerchantID.IsZero() && strings.TrimSpace(p.PspID) != "" {
		keyName, err := merchants.PSPSecretName("stripe", p.ProviderEnvironment, p.PspID, "secret_key")
		if err != nil {
			return ManagedStripeWebhookResult{}, err
		}
		secretName, err = merchants.PSPSecretName("stripe", p.ProviderEnvironment, p.PspID, "webhook_signing_secret")
		if err != nil {
			return ManagedStripeWebhookResult{}, err
		}
		previousName, err = merchants.PSPSecretName("stripe", p.ProviderEnvironment, p.PspID, "webhook_signing_secret_previous")
		if err != nil {
			return ManagedStripeWebhookResult{}, err
		}
		if secretKey == "" {
			sec, err := p.SecretStore.Get(ctx, p.MerchantID, keyName)
			if err != nil && !errors.Is(err, merchants.ErrSecretNotFound) {
				return ManagedStripeWebhookResult{}, fmt.Errorf("load stripe secret key: %w", err)
			}
			secretKey = strings.TrimSpace(sec.Value)
		}
		sec, err := p.SecretStore.Get(ctx, p.MerchantID, secretName)
		if err != nil && !errors.Is(err, merchants.ErrSecretNotFound) {
			return ManagedStripeWebhookResult{}, fmt.Errorf("load stripe webhook secret: %w", err)
		}
		currentSecret = strings.TrimSpace(sec.Value)
		haveSecret = currentSecret != ""

		// #856: a miss here means UNKNOWN, not lost. The commonest cause is a
		// derived-name drift — the psps row's environment or account_id changed,
		// so the name we look under moved while the secret sat untouched under
		// the old one. Repair the local record before concluding anything about
		// the remote endpoint.
		if !haveSecret {
			recovered, from, rerr := recoverStripeWebhookSecret(ctx, p.SecretStore, p.MerchantID, secretName)
			if rerr != nil {
				return ManagedStripeWebhookResult{}, rerr
			}
			if recovered != "" {
				if _, err := p.SecretStore.Put(ctx, p.MerchantID, secretName, recovered); err != nil {
					return ManagedStripeWebhookResult{}, fmt.Errorf("repair stripe webhook secret name: %w", err)
				}
				currentSecret, haveSecret, repairedFrom = recovered, true, from
			}
		}
	}

	if secretKey == "" {
		return ManagedStripeWebhookResult{Skipped: true, SkipReason: "stripe secret key not configured", WebhookURL: webhookURL, SecretName: secretName}, nil
	}

	// MODE 1 (#723): a Stripe-minted signing secret would seed only process
	// memory and be lost on reboot — the endpoint would then be found WITHOUT a
	// known secret (webhooks unverifiable). Any registration path that needs a
	// mint is refused; a manifest-declared webhook_signing_secret keeps working.
	manifestMode := p.Config != nil && p.Config.IsManifestMerchantSource()
	if manifestMode && !haveSecret {
		return ManagedStripeWebhookResult{}, fmt.Errorf("merchant_source=manifest refuses managed stripe webhook registration without a declared webhook_signing_secret (a Stripe-minted secret cannot survive reboot, #723): declare secrets.webhook_signing_secret in the manifest and register the endpoint %s out-of-band, or run merchant_source=api", webhookURL)
	}

	rails := railresolve.FixedSet{"stripe": &config.PSPConfig{Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: secretKey}}}
	svc := &StripeCatalogService{Config: p.Config, Rails: rails, BaseURL: p.StripeBaseURL}
	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{
		URL:           webhookURL,
		EnabledEvents: p.EnabledEvents,
		HaveSecret:    haveSecret,
		ForbidCreate:  manifestMode,
		Now:           p.Now,
		RetireOverlap: p.RetireOverlap,
	})
	if errors.Is(err, ErrWebhookCreateForbidden) {
		return ManagedStripeWebhookResult{}, fmt.Errorf("merchant_source=manifest refuses to (re)create the managed stripe webhook endpoint %s (the Stripe-minted signing secret cannot survive reboot, #723): register it out-of-band and declare its webhook_signing_secret in the manifest, or run merchant_source=api: %w", webhookURL, err)
	}
	if errors.Is(err, ErrWebhookEndpointBudgetExhausted) {
		return ManagedStripeWebhookResult{
			SecretName: secretName, WebhookURL: webhookURL, RepairedFrom: repairedFrom,
			OperatorAction: fmt.Sprintf("stripe webhook endpoint %s could not roll over: %v — retire the superseded endpoints (they still deliver) before the account reaches Stripe's per-account limit", webhookURL, err),
		}, nil
	}
	if err != nil {
		return ManagedStripeWebhookResult{}, err
	}
	out := ManagedStripeWebhookResult{
		Result: res, SecretName: secretName, WebhookURL: webhookURL, RepairedFrom: repairedFrom,
	}

	if s := strings.TrimSpace(res.Secret); s != "" {
		if secretName == "" {
			return ManagedStripeWebhookResult{}, fmt.Errorf("stripe webhook secret destination not configured")
		}
		// Demote the outgoing secret BEFORE promoting the new one: during the
		// overlap both endpoints deliver, and the inbound verifier tries both.
		// Events already queued on the old endpoint keep verifying.
		if previousName != "" && currentSecret != "" && len(res.Superseded) > 0 {
			if _, err := p.SecretStore.Put(ctx, p.MerchantID, previousName, currentSecret); err != nil {
				return ManagedStripeWebhookResult{}, fmt.Errorf("retain previous stripe webhook secret: %w", err)
			}
		}
		if _, err := p.SecretStore.Put(ctx, p.MerchantID, secretName, s); err != nil {
			return ManagedStripeWebhookResult{}, fmt.Errorf("store stripe webhook secret: %w", err)
		}
	}

	out.RetirePending = res.Legacy
	if len(res.Legacy) == 0 {
		return out, nil
	}
	if !p.AllowRetire {
		out.OperatorAction = supersededOperatorAction(webhookURL, res.Legacy, "the destructive-action kill switch is off, so nothing will be deleted automatically")
		return out, nil
	}
	ret, rerr := svc.RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{Now: p.Now, Overlap: p.RetireOverlap})
	out.Retired, out.RetirePending = ret.Retired, ret.Pending
	if rerr != nil {
		out.OperatorAction = supersededOperatorAction(webhookURL, res.Legacy, rerr.Error())
		return out, nil
	}
	if len(out.Retired) > 0 && previousName != "" && len(out.RetirePending) == 0 {
		// The last predecessor is gone; its secret can no longer verify anything.
		if err := p.SecretStore.Delete(ctx, p.MerchantID, previousName); err != nil && !errors.Is(err, merchants.ErrSecretNotFound) {
			return ManagedStripeWebhookResult{}, fmt.Errorf("drop retired stripe webhook secret: %w", err)
		}
	}
	if len(out.RetirePending) > 0 {
		out.OperatorAction = supersededOperatorAction(webhookURL, out.RetirePending, "still inside the delivery-overlap window")
	}
	return out, nil
}

func supersededOperatorAction(webhookURL string, legacy []SupersededEndpoint, why string) string {
	ids := make([]string, 0, len(legacy))
	for _, l := range legacy {
		ids = append(ids, fmt.Sprintf("%s (api_version %s, superseded %s, retireable after %s)",
			l.ID, l.APIVersion, l.Since.Format(time.RFC3339), l.RetireAfter.Format(time.RFC3339)))
	}
	return fmt.Sprintf("stripe webhook endpoint %s rolled over to api_version %s; %d superseded endpoint(s) are STILL ENABLED and still delivering: %s — %s",
		webhookURL, stripeapi.APIVersion, len(legacy), strings.Join(ids, "; "), why)
}

// recoverStripeWebhookSecret looks for a stripe webhook signing secret stored
// under a DIFFERENT derived name for this merchant. It returns a value only when
// the choice is unambiguous — exactly one candidate — because guessing between
// two Stripe accounts' secrets would silently break inbound verification. A
// store that cannot list is not an error: recovery is best-effort, and the
// caller's fallback (roll over additively) is already safe.
func recoverStripeWebhookSecret(ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, wantName string) (string, string, error) {
	names, err := store.List(ctx, merchantID)
	if err != nil {
		return "", "", nil
	}
	var candidates []string
	for _, n := range names {
		if n == wantName {
			continue
		}
		rail, _, _, key, scoped, perr := merchants.ParsePSPSecretName(n)
		if perr != nil || !scoped || rail != "stripe" || key != "webhook_signing_secret" {
			continue
		}
		candidates = append(candidates, n)
	}
	if len(candidates) != 1 {
		return "", "", nil
	}
	sec, err := store.Get(ctx, merchantID, candidates[0])
	if err != nil {
		if errors.Is(err, merchants.ErrSecretNotFound) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("recover stripe webhook secret from %s: %w", candidates[0], err)
	}
	value := strings.TrimSpace(sec.Value)
	if value == "" {
		return "", "", nil
	}
	return value, candidates[0], nil
}
