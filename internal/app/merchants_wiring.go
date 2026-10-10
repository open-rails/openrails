package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/scim"
	"github.com/open-rails/openrails/internal/vaultconn"
)

// wireMerchantConfig opens Vault, builds every merchant's configuration (in
// Vault when a KV mount is named, else from files) and the merchants service
// over it, and binds the settings reader every database handle reads through.
// Login and loads run later: nothing here waits on Vault.
func (r *Runtime) wireMerchantConfig(ctx context.Context, borrowed *vaultapi.Client) error {
	conn, err := vaultconn.Open(ctx, r.Config, borrowed)
	if err != nil {
		return err
	}
	r.Vault = conn
	var source merchantdocs.Source = merchantdocs.NewFileSource()
	if conn.KV != nil {
		prefix := ""
		if r.Config.Vault != nil {
			prefix = r.Config.Vault.ScopePrefix
		}
		if source, err = merchantdocs.NewVaultSource(conn.KV, prefix); err != nil {
			return err
		}
	}
	cache := merchantdocs.NewCache(source)
	r.MerchantConfig = cache
	svc, err := merchants.NewService(r.DB.DataPool(), cache, config.ExpectedProviderEnvironment(config.IsTestMode(r.Config)))
	if err != nil {
		return err
	}
	// or#858: the merchant purge answers to the same #836 kill switch and #835
	// per-merchant policy as a mass cancellation.
	svc.WithDestructivePolicy(destructive.New(r.DB))
	overlap, err := config.WebhookSecretOverlapDuration(r.Config)
	if err != nil {
		return err
	}
	svc.WithClock(r.Clock).WithWebhookSecretOverlap(overlap)
	if r.NMIClients != nil {
		svc.WithNMIWire(r.NMIClients.Wire)
	}
	cache.SetSync(func(ctx context.Context, id billing.MerchantID, previous, loaded merchantdocs.Set) (merchantdocs.Set, error) {
		set, err := r.syncMerchantConfig(ctx, id, previous, loaded)
		if err == nil {
			r.followPSPPosture(id, previous, set)
		}
		return set, err
	})
	cache.SetAnnounce(r.announceMerchantConfig)
	r.DB.SetMerchantConfig(svc)
	r.ArmMerchantsService(svc)
	r.ArmSolanaRecurringServices(svc.Secrets(), conn.SolanaTransit)
	return nil
}

// EnsureMerchantsService readies merchant configuration for serving. With
// Vault holding it, it waits (bounded by ctx) for Vault login and loads the
// credential fingerprint key; it starts the listener that reloads a merchant
// another replica edited. Idempotent.
func (r *Runtime) EnsureMerchantsService(ctx context.Context) error {
	if r == nil || r.Merchants == nil || r.MerchantConfig == nil {
		return errors.New("initialize merchant services: merchant configuration is not wired")
	}
	r.configReady.Do(func() {
		vault, ok := r.MerchantConfig.Source().(*merchantdocs.VaultSource)
		if !ok {
			return
		}
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, awaitTimeout)
			defer cancel()
		}
		if r.Vault.Auth != nil {
			if err := r.Vault.Auth.Wait(ctx, awaitTimeout); err != nil {
				r.configErr = fmt.Errorf("initialize merchant services: Vault holds merchant configuration and is unreachable: %w", err)
				return
			}
		}
		key, err := vault.FingerprintKey(ctx)
		if err != nil {
			r.configErr = fmt.Errorf("initialize merchant services: Vault holds merchant configuration and is unreachable: %w", err)
			return
		}
		fingerprints, err := merchants.NewCredentialFingerprinter(key)
		if err != nil {
			r.configErr = err
			return
		}
		r.Merchants.WithCredentialFingerprinter(fingerprints)
		if pool := r.DB.Pool(); pool != nil {
			r.Go("merchant config notifications", func(ctx context.Context) {
				r.MerchantConfig.Listen(ctx, pool, r.DB.DataPool().Schema())
			})
		}
	})
	return r.configErr
}

// announceMerchantConfig tells every replica to reload a merchant.
func (r *Runtime) announceMerchantConfig(ctx context.Context, id billing.MerchantID) error {
	return gen.New(r.DB.Pool()).NotifyMerchantConfig(ctx, gen.NotifyMerchantConfigParams{
		Channel: merchantdocs.Channel(r.DB.DataPool().Schema()), Payload: id.UUID().String(),
	})
}

// syncMerchantConfig is the cache's reconciliation: identities first, then the
// merchant document, whose settings must validate and whose SCIM token is
// declared.
func (r *Runtime) syncMerchantConfig(ctx context.Context, id billing.MerchantID, previous, loaded merchantdocs.Set) (merchantdocs.Set, error) {
	set, err := r.Merchants.SyncConfig(ctx, id, previous, loaded)
	if err != nil {
		return set, err
	}
	if previous.HasMerchant && set.HasMerchant && previous.Merchant.Revision == set.Merchant.Revision && reflect.DeepEqual(previous.Merchant.Value, set.Merchant.Value) {
		return set, nil
	}
	reject := func(why string) merchantdocs.Set {
		if set.Rejected == nil {
			set.Rejected = map[string]string{}
		}
		set.Rejected["merchant"] = why
		set.Merchant, set.HasMerchant = previous.Merchant, previous.HasMerchant
		return set
	}
	doc := set.Merchant.Value
	settings, err := merchantconfig.Normalize(doc.DisplayName, doc.Settings)
	if err != nil {
		return reject("settings: " + err.Error()), nil
	}
	mctx := merchant.WithID(ctx, id)
	err = r.DB.RunInMerchantConn(mctx, func(ctx context.Context) error {
		q := r.DB.Gen(ctx)
		if err := scim.Declare(ctx, q, id, doc.Secrets.SCIMToken); err != nil {
			return err
		}
		return recordDanglingPolicies(ctx, q, id, settings)
	})
	if err != nil {
		if errors.Is(err, scim.ErrTokenTooShort) {
			return reject("secrets.scim_token: " + err.Error()), nil
		}
		return previous, err
	}
	return set, nil
}

// DanglingPolicyFindingType is the finding a customer assignment to a billing
// policy the settings no longer declare opens.
const DanglingPolicyFindingType = "consistency.dangling_billing_policy"

// recordDanglingPolicies opens a finding while customers are assigned a
// billing policy the settings no longer declare (an edit outside OpenRails);
// they fall back to their tier's policy, then the default.
func recordDanglingPolicies(ctx context.Context, q *gen.Queries, id billing.MerchantID, settings merchantconfig.Settings) error {
	names := make([]string, 0, len(settings.Policies))
	for name := range settings.Policies {
		names = append(names, name)
	}
	missing, err := q.FindAssignedBillingPolicyOutside(ctx, gen.FindAssignedBillingPolicyOutsideParams{MerchantID: id.UUID(), Names: names})
	if errors.Is(err, pgx.ErrNoRows) {
		_, err := q.ResolveConfigFinding(ctx, gen.ResolveConfigFindingParams{MerchantID: id.UUID(), FindingType: DanglingPolicyFindingType, SubjectKey: "billing_policies"})
		return err
	}
	if err != nil {
		return err
	}
	action := fmt.Sprintf("Customers are assigned the billing policy %q, which the merchant's settings no longer declare; they follow their tier's policy, then the default. Declare the policy again, or reassign those customers.", missing)
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: id.UUID(), FindingType: DanglingPolicyFindingType, SubjectKey: "billing_policies",
		Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: []byte(fmt.Sprintf(`{"policy":%q}`, missing)),
	})
	if err == nil {
		log.WithFields(log.Fields{"merchant_id": id.String(), "policy": missing}).Warn("merchant config: customers are assigned an undeclared billing policy")
	}
	return err
}

// ArmMerchantsService installs the merchants service and wires its
// credential reader into the request-time consumers (checkout, payment
// methods, alerting).
func (r *Runtime) ArmMerchantsService(svc *merchants.Service) {
	if r == nil || svc == nil {
		return
	}
	svc.StripeClients = r.StripeClients
	r.Merchants = svc
	store := svc.Secrets()
	if r.AlertService != nil {
		r.AlertService.SetMerchantConfig(svc.Config())
	}
	if r.CheckoutService != nil {
		r.CheckoutService.SetMerchantSecretStore(store)
		r.CheckoutService.SetPSPSecretResolver(svc)
	}
	if r.RailPaymentMethodService != nil {
		r.RailPaymentMethodService.SetMerchantSecretStore(store)
		r.RailPaymentMethodService.SetPSPSecretResolver(svc)
	}
}

// awaitTimeout bounds how long boot waits on Vault holding merchant
// configuration when the caller's context sets no deadline.
const awaitTimeout = 30 * time.Second
