package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/messages"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/retry"
	"github.com/open-rails/openrails/internal/scim"
	"github.com/open-rails/openrails/internal/vaultconn"
)

// wireMerchantConfig opens Vault, builds every merchant's configuration (in
// Vault when a KV mount is named, else from files) and the merchants service
// over it, and binds the settings reader every database handle reads through.
// Login and loads run later: nothing here waits on Vault.
func (r *Runtime) wireMerchantConfig(ctx context.Context, borrowed *vaultapi.Client, custodianTransport http.RoundTripper) error {
	conn, err := vaultconn.Open(ctx, r.Log, r.Config, borrowed)
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
		if source, err = merchantdocs.NewVaultSource(r.Log, conn.KV, prefix); err != nil {
			// vault.scope_prefix is malformed, so Vault is unusable
			// (vaultconn.Open): the default prefix is never read.
			source, _ = merchantdocs.NewVaultSource(r.Log, conn.KV, "")
		}
	}
	cache := merchantdocs.NewCache(r.Log, source)
	r.MerchantConfig = cache
	svc, err := merchants.NewService(r.Log, r.DB.DataPool(), cache, r.Config)
	if err != nil {
		return err
	}
	svc.WithPlatformVaultStore(conn.PlatformKV)
	svc.WithBasisTheoryTransport(custodianTransport)
	// The merchant purge answers to the kill switch and per-merchant policy
	// that hold a mass cancellation, and queues its Vault cleanup with itself.
	svc.WithDestructivePolicy(destructive.New(r.DB)).WithJobs(r.DB)
	overlap, err := config.WebhookSecretOverlapDuration(r.Config)
	if err != nil {
		overlap = config.DefaultWebhookSecretOverlap
	}
	svc.WithClock(r.Clock).WithWebhookSecretOverlap(overlap).WithDataLinkPacing(r.DataLinkPacing())
	if r.NMIClients != nil {
		svc.WithNMIWire(r.NMIClients.Wire)
	}
	cache.SetSync(func(ctx context.Context, id billing.MerchantID, previous, loaded merchantdocs.Set) (merchantdocs.Set, error) {
		set, err := r.syncMerchantConfig(ctx, id, previous, loaded)
		if err == nil {
			set = checkMessages(previous, set)
			r.followPSPPosture(id, previous, set)
			r.followPaymentDomain(id, previous, set)
			r.followStripeWebhooks(id, previous, set)
			r.followMoneyPolicy(id, previous, set)
			r.recordRefusedDocuments(ctx, id, previous, set)
			if err := svc.RecordConfigLoad(ctx, id, previous, set); err != nil && ctx.Err() == nil {
				r.Log.WarnContext(ctx, "merchant config: recording what the load changed failed; a later load records it", logx.Err(err), "merchant_id", id.String())
			}
		}
		return set, err
	})
	cache.SetRecord(svc.RecordConfigWrite)
	cache.SetAnnounce(r.announceMerchantConfig)
	r.DB.SetMerchantConfig(svc)
	r.ArmMerchantsService(svc)
	r.ArmSolanaRecurringServices(conn.SolanaTransit)
	return nil
}

// EnsureMerchantsService readies merchant configuration for a one-off tool
// that needs it now. With Vault holding it, it waits (bounded by ctx, else
// 30s) for Vault login and the credential fingerprint key, and starts the
// listener that reloads a merchant another replica edited. In a serving
// process (ServeMerchantsService) it never waits: the background readies it.
func (r *Runtime) EnsureMerchantsService(ctx context.Context) error {
	if r == nil || r.Merchants == nil || r.MerchantConfig == nil {
		return errors.New("initialize merchant services: merchant configuration is not wired")
	}
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.configReady || r.configServing {
		return nil
	}
	if err := r.readyMerchantConfig(ctx); err != nil {
		return err
	}
	r.configReady = true
	return nil
}

// ServeMerchantsService is EnsureMerchantsService for a serving process:
// Vault unreachable never fails it. Merchant configuration then answers
// unavailable, the refusal is reported (Refusals), and a background task
// retries with capped backoff until Vault answers.
func (r *Runtime) ServeMerchantsService(ctx context.Context) error {
	if r == nil || r.Merchants == nil || r.MerchantConfig == nil {
		return errors.New("initialize merchant services: merchant configuration is not wired")
	}
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.configReady || r.configServing {
		return nil
	}
	wait, cancel := context.WithTimeout(ctx, serveAwait)
	err := r.readyMerchantConfig(wait)
	cancel()
	if err == nil {
		r.configReady = true
		return nil
	}
	r.configServing = true
	const subject = "vault.merchant_configuration"
	r.MerchantConfig.Hold(err)
	r.Refuse(subject, fmt.Errorf("%w; merchant configuration answers unavailable until Vault answers", err))
	r.Go("merchant configuration", func(ctx context.Context) {
		if retry.Forever(ctx, func(ctx context.Context) error {
			if err := r.Vault.State(); err != nil {
				return err
			}
			return r.readyMerchantConfig(ctx)
		}, nil) != nil {
			return
		}
		r.configMu.Lock()
		r.configReady = true
		r.configMu.Unlock()
		r.MerchantConfig.Release()
		r.Unrefuse(subject)
	})
	return nil
}

// readyMerchantConfig arms the Vault-held configuration: the fingerprint key
// for the admission gate and duplicate detection, and the edit listener.
// Nothing to do for a file.
func (r *Runtime) readyMerchantConfig(ctx context.Context) error {
	vault, ok := r.MerchantConfig.Source().(*merchantdocs.VaultSource)
	if !ok {
		return nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, awaitTimeout)
		defer cancel()
	}
	if err := r.Vault.State(); err != nil {
		return fmt.Errorf("initialize merchant services: Vault holds merchant configuration and cannot be used: %w", err)
	}
	key, err := vault.FingerprintKey(ctx)
	if err != nil {
		return fmt.Errorf("initialize merchant services: Vault holds merchant configuration and is unreachable: %w", err)
	}
	if err := r.MerchantConfig.SetDestructiveAdmissions(r.DestructiveAdmissions, key); err != nil {
		return fmt.Errorf("initialize configuration admission: %w", err)
	}
	fingerprints, err := merchants.NewCredentialFingerprinter(key)
	if err != nil {
		return err
	}
	r.Merchants.WithCredentialFingerprinter(fingerprints)
	if r.DB.Pool() != nil {
		r.Go("merchant config notifications", func(ctx context.Context) {
			r.MerchantConfig.Listen(ctx, r.DB, r.DB.DataPool().Schema())
		})
	}
	return nil
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
	mctx := merchant.WithID(ctx, id)
	// A unit something counts keeps the declaration it is counted in: a
	// document that removes or rescales one serves its previous declaration
	// of it, at the scale the units book recorded, with a finding.
	var inUse []string
	var counted map[string]int
	if err := r.DB.MerchantTx(mctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if inUse, err = merchantconfig.UnitsInUse(ctx, gen.New(tx), id.UUID()); err != nil {
			return err
		}
		counted, err = merchantconfig.CountedScales(ctx, gen.New(tx), id.UUID())
		return err
	}); err != nil {
		return previous, err
	}
	var served []billing.MerchantCurrency
	if previous.HasMerchant {
		served = previous.Merchant.Value.Currencies
	}
	if why := merchantconfig.KeepCountedUnits(inUse, counted, served, &set.Merchant.Value.MerchantSettings); why != "" {
		if set.Rejected == nil {
			set.Rejected = map[string]string{}
		}
		set.Rejected[merchantconfig.UnitsDoc] = why
	}
	doc := set.Merchant.Value
	settings, err := merchantconfig.Normalize(doc.DisplayName, doc.MerchantSettings)
	if err != nil {
		return reject("settings: " + err.Error()), nil
	}
	err = r.DB.MerchantTx(mctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := scim.Declare(ctx, q, id, doc.Secrets.SCIMToken); err != nil {
			return err
		}
		return recordDanglingPolicies(ctx, r.Log, q, id, settings)
	})
	if err != nil {
		if errors.Is(err, scim.ErrTokenTooShort) || errors.Is(err, scim.ErrTokenInUse) {
			return reject("secrets.scim_token: " + err.Error()), nil
		}
		return previous, err
	}
	return set, nil
}

// checkMessages refuses a messages document a message of which does not
// render; the version served before keeps serving.
func checkMessages(previous, set merchantdocs.Set) merchantdocs.Set {
	if !set.HasMessages || previous.HasMessages && previous.Messages.Revision == set.Messages.Revision && reflect.DeepEqual(previous.Messages.Value, set.Messages.Value) {
		return set
	}
	if err := messages.Check(set.Messages.Value); err != nil {
		if set.Rejected == nil {
			set.Rejected = map[string]string{}
		}
		set.Rejected[merchantdocs.MessagesDoc] = err.Error()
		set.Messages, set.HasMessages = previous.Messages, previous.HasMessages
	}
	return set
}

// configFindings are the configuration problems a merchant's operator is
// alerted to: each document the merchant does not serve, and each undeclared
// account still holding billing.
func configFindings(set merchantdocs.Set) []configFinding {
	return []configFinding{
		{merchantdocs.RefusedDocumentFindingType, "The merchant does not serve its %s document: %s. Correct it; it serves once it checks.", set.Rejected},
		{merchantdocs.UndeclaredAccountFindingType, "%s: %s.", set.Undeclared},
	}
}

type configFinding struct {
	findingType, action string
	open                map[string]string
}

// recordRefusedDocuments keeps one finding open per configuration problem,
// naming why, alerts the merchant's operator through the finding channels
// (alert webhooks, alert email) once per episode, and closes the findings of
// problems that are gone. A failure is logged: the state itself is served
// from memory.
func (r *Runtime) recordRefusedDocuments(ctx context.Context, id billing.MerchantID, previous, set merchantdocs.Set) {
	if maps.Equal(previous.Rejected, set.Rejected) && maps.Equal(previous.Undeclared, set.Undeclared) && len(previous.Revisions()) > 0 {
		return
	}
	ctx = merchant.WithID(ctx, id)
	var opened []gen.BillingReconciliationFinding
	err := r.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := r.DB.Gen(ctx)
		for _, kind := range configFindings(set) {
			for _, subject := range slices.Sorted(maps.Keys(kind.open)) {
				why := kind.open[subject]
				action := fmt.Sprintf(kind.action, subject, why)
				evidence, err := json.Marshal(map[string]string{"subject": subject, "reason": why})
				if err != nil {
					return err
				}
				row, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
					MerchantID: id.UUID(), FindingType: kind.findingType, SubjectKey: subject,
					Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
				})
				if err != nil {
					return err
				}
				opened = append(opened, row)
			}
			findingType := kind.findingType
			listed, err := q.ListReconciliationFindings(ctx, gen.ListReconciliationFindingsParams{MerchantID: id.UUID(), FindingType: &findingType, PageLimit: 1000})
			if err != nil {
				return err
			}
			for _, f := range listed {
				if _, open := kind.open[f.SubjectKey]; open || f.ResolvedAt != nil {
					continue
				}
				if _, err := q.ResolveConfigFinding(ctx, gen.ResolveConfigFindingParams{MerchantID: id.UUID(), FindingType: kind.findingType, SubjectKey: f.SubjectKey}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			r.Log.WarnContext(ctx, "merchant config: recording what is not served failed", logx.Err(err), "merchant_id", id.String())
		}
		return
	}
	if len(opened) > 0 {
		// Each alert's claim is its own transaction, after the load's.
		r.Go("configuration alerts", func(bg context.Context) { r.alertConfigFindings(merchant.WithID(bg, id), opened) })
	}
}

// alertConfigFindings notifies the merchant's operator of each finding not
// yet notified in its episode.
func (r *Runtime) alertConfigFindings(ctx context.Context, findings []gen.BillingReconciliationFinding) {
	if r.AlertService == nil {
		return
	}
	for _, f := range findings {
		if err := r.AlertService.NotifyFinding(ctx, reconcile.FindingRecordFromRow(f)); err != nil && ctx.Err() == nil {
			r.Log.WarnContext(ctx, "merchant config: alerting the operator failed; the finding stays in the console", logx.Err(err), "finding_id", f.ID.String())
		}
	}
}

// reloadMerchantConfig re-evaluates mid's configuration: a declaration or an
// ended subscription resolves an undeclared account's finding.
func (r *Runtime) reloadMerchantConfig(ctx context.Context, mid billing.MerchantID) error {
	if r.MerchantConfig == nil {
		return nil
	}
	_, err := r.MerchantConfig.Reload(ctx, mid)
	return err
}

// DanglingPolicyFindingType is the finding a customer assignment to a billing
// policy the settings no longer declare opens.
const DanglingPolicyFindingType = "consistency.dangling_billing_policy"

// recordDanglingPolicies opens a finding while customers are assigned a
// billing policy the settings no longer declare (an edit outside OpenRails);
// they fall back to their tier's policy, then the default.
func recordDanglingPolicies(ctx context.Context, logger *slog.Logger, q *gen.Queries, id billing.MerchantID, settings merchantconfig.Settings) error {
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
		logger.WarnContext(ctx, "merchant config: customers are assigned an undeclared billing policy", "merchant_id", id.String(), "policy", missing)
	}
	return err
}

// ArmMerchantsService installs the merchants service and wires its
// credential reader into the request-time consumers (purchase, payment
// methods, alerting).
func (r *Runtime) ArmMerchantsService(svc *merchants.Service) {
	if r == nil || svc == nil {
		return
	}
	svc.StripeClients = r.StripeClients
	svc.Changed = r.MerchantChanged
	r.Merchants = svc
	store := svc.Secrets()
	if r.AlertService != nil {
		r.AlertService.SetMerchantConfig(svc.Config())
	}
	if r.PurchaseService != nil {
		r.PurchaseService.SetMerchantSecretStore(store)
		r.PurchaseService.SetPSPSecretResolver(svc)
	}
	if r.RailPaymentMethodService != nil {
		r.RailPaymentMethodService.SetMerchantSecretStore(store)
		r.RailPaymentMethodService.SetPSPSecretResolver(svc)
	}
}

// awaitTimeout bounds how long a one-off tool waits on Vault holding merchant
// configuration when the caller's context sets no deadline.
const awaitTimeout = 30 * time.Second

// serveAwait bounds how long a serving process's boot waits on Vault holding
// merchant configuration before it serves without it.
const serveAwait = 5 * time.Second
