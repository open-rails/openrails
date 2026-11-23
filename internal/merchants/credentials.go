package merchants

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	log "github.com/sirupsen/logrus"
)

// DefaultNMICollectJSURL is the standard NMI Collect.js script URL.
const DefaultNMICollectJSURL = "https://secure.networkmerchants.com/token/Collect.js"

// NMICollectJSURLAllowed accepts only NMI's own Collect.js script. The URL is
// loaded into the payment page, so a configured value can never name another
// origin (SEC-32).
func NMICollectJSURLAllowed(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/token/Collect.js" {
		return false
	}
	switch strings.ToLower(u.Host) {
	case "secure.networkmerchants.com", "secure.nmi.com":
		return true
	}
	return false
}

// ErrNoActivePSP means the merchant has PSPs on the requested rail, but all
// are archived and therefore drain-only.
var ErrNoActivePSP = errors.New("merchants: no non-archived PSP available for new work")

// StripeCredentials are a merchant's rail credentials, loaded by merchant id at
// request time (NOT injected process-wide). Empty fields mean "not configured".
type StripeCredentials struct {
	// AccountID is the exact account resolved with these secrets.
	AccountID            string
	SecretKey            string
	WebhookSigningSecret string
	WebhookSigningThin   string
	// WebhookSigningPrevious is the outgoing secret during an api_version
	// rollover (#856). Both endpoints deliver through the overlap, so inbound
	// verification must accept both secrets.
	WebhookSigningPrevious string
}

// NMITokenizationConfig is browser-facing NMI tokenization configuration for a
// merchant/provider. The key is public, but it is still loaded from the
// merchant-scoped provider config so merchant A's browser config does not bleed
// into merchant B.
type NMITokenizationConfig struct {
	TokenizationKey string
	CollectJSURL    string
}

// LoadStripeCredentials loads the credentials of the merchant's active Stripe
// PSP. A missing credential is an empty field, so a PSP with only a webhook
// secret (or only an API key) still loads.
func (s *Service) LoadStripeCredentials(ctx context.Context, id billing.MerchantID) (StripeCredentials, error) {
	scope, ok, err := s.ActivePSPScope(ctx, id, "stripe")
	if err != nil || !ok {
		return StripeCredentials{}, err
	}
	return s.stripeCredentials(scope), nil
}

func (s *Service) stripeCredentials(scope PSPScope) StripeCredentials {
	creds := StripeCredentials{
		AccountID:            scope.AccountID,
		SecretKey:            scope.Secret("secret_key"),
		WebhookSigningSecret: scope.Secret("webhook_signing_secret"),
		WebhookSigningThin:   scope.Secret("webhook_signing_secret_thin"),
	}
	if s.overlapOpen(scope) {
		creds.WebhookSigningPrevious = scope.Secret("webhook_signing_secret_previous")
	}
	return creds
}

// LoadNMIWebhookSigningSecret is the active NMI PSP's webhook signing secret.
func (s *Service) LoadNMIWebhookSigningSecret(ctx context.Context, id billing.MerchantID, provider string) (string, error) {
	if id.IsZero() || strings.ToLower(strings.TrimSpace(provider)) != string(models.RailNMI) {
		return "", nil
	}
	scope, ok, err := s.ActivePSPScope(ctx, id, "nmi")
	if err != nil || !ok {
		return "", err
	}
	return scope.Secret("webhook_signing_secret"), nil
}

// LoadNMITokenizationConfig loads merchant-scoped browser tokenization config
// for an NMI provider. Missing values return empty fields, except CollectJSURL
// defaults to NMI's standard URL when a supported provider is selected.
func (s *Service) LoadNMITokenizationConfig(ctx context.Context, id billing.MerchantID, provider string) (NMITokenizationConfig, error) {
	var cfg NMITokenizationConfig
	if id.IsZero() || strings.ToLower(strings.TrimSpace(provider)) != string(models.RailNMI) {
		return cfg, nil
	}
	scope, ok, err := s.ActivePSPScope(ctx, id, "nmi")
	if err != nil {
		return cfg, err
	}
	if !ok {
		cfg.CollectJSURL = DefaultNMICollectJSURL
		return cfg, nil
	}
	cfg.TokenizationKey = settingText(scope.Settings, "tokenization_key")
	cfg.CollectJSURL = settingText(scope.Settings, "tokenization_url")
	if !NMICollectJSURLAllowed(cfg.CollectJSURL) {
		cfg.CollectJSURL = DefaultNMICollectJSURL
	}
	return cfg, nil
}

func settingText(settings map[string]any, key string) string {
	switch v := settings[key].(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// ActivePSPSecretRef resolves the active PSP for a merchant rail and returns
// that account's credential reference for key.
func (s *Service) ActivePSPSecretRef(ctx context.Context, id billing.MerchantID, rail, key string) (SecretRef, bool, error) {
	scope, ok, err := s.ActivePSPScope(ctx, id, rail)
	if err != nil || !ok {
		return SecretRef{}, ok, err
	}
	ref, err := scope.SecretRef(key)
	if err != nil {
		return SecretRef{}, false, err
	}
	return ref, true, nil
}

// ActivePSPScope resolves the PSP new work on a merchant rail uses: the
// newest live one. A rail whose PSPs are all archived is ErrNoActivePSP.
func (s *Service) ActivePSPScope(ctx context.Context, id billing.MerchantID, rail string) (PSPScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return PSPScope{}, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	j, err := s.load(ctx, id)
	if err != nil {
		return PSPScope{}, false, fmt.Errorf("load active PSP %s: %w", rail, err)
	}
	live := j.live(rail)
	if len(live) > 1 {
		log.WithFields(log.Fields{"merchant_id": id.String(), "rail": rail, "active_count": len(live)}).
			Warn("multiple active PSPs configured; using newest for new work")
	}
	if len(live) == 0 {
		for _, p := range j.psps {
			if p.Rail == rail {
				return PSPScope{}, false, ErrNoActivePSP
			}
		}
		return PSPScope{}, false, nil
	}
	return live[0], true, nil
}

// PSPKeyArchived reports whether key names an ARCHIVED PSP of the merchant,
// so routing can say "retired" instead of "unknown".
func (s *Service) PSPKeyArchived(ctx context.Context, id billing.MerchantID, key string) (bool, error) {
	key = strings.TrimSpace(key)
	if s == nil || s.pool == nil || id.IsZero() || key == "" {
		return false, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return false, fmt.Errorf("load archived PSP by key %s: %w", key, err)
	}
	live := false
	archived := false
	for _, p := range j.psps {
		if strings.EqualFold(p.Key, key) {
			live = live || !p.Archived
			archived = archived || p.Archived
		}
	}
	return archived && !live, nil
}

// PSPScopeByKey resolves a live PSP by its key: how the payment-provider
// vocabulary of the catalog and purchase resolves to a concrete account.
func (s *Service) PSPScopeByKey(ctx context.Context, id billing.MerchantID, key string) (PSPScope, bool, error) {
	key = strings.TrimSpace(key)
	if s == nil || s.pool == nil || id.IsZero() || key == "" {
		return PSPScope{}, false, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return PSPScope{}, false, fmt.Errorf("load PSP by key %s: %w", key, err)
	}
	for _, p := range j.live("") {
		if strings.EqualFold(p.Key, key) {
			return p, true, nil
		}
	}
	return PSPScope{}, false, nil
}

// ActivePSPScopesForRail lists every live PSP on rail, newest first. Purchase
// accepts a bare rail-kind selector only when it is unambiguous (#848).
func (s *Service) ActivePSPScopesForRail(ctx context.Context, id billing.MerchantID, rail string) ([]PSPScope, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return nil, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list PSPs %s: %w", rail, err)
	}
	return j.live(normalizeProviderSecretType(rail)), nil
}

// PSPDocumentVersion is the revision and environment of the live document
// declaring account on rail, read from the configuration's source rather than
// the cache; ok is false when no live document declares it.
func (s *Service) PSPDocumentVersion(ctx context.Context, id billing.MerchantID, rail, account string) (revision int64, environment string, ok bool, err error) {
	if s == nil || s.config == nil {
		return 0, "", false, nil
	}
	set, err := s.config.Source().Load(ctx, id)
	if err != nil {
		return 0, "", false, err
	}
	for _, doc := range set.PSPs {
		if doc.Value.Rail == rail && doc.Value.AccountID == account && !doc.Value.Archived {
			return doc.Revision, doc.Value.Environment, true, nil
		}
	}
	return 0, "", false, nil
}

// PSPIdentities lists the merchant's current PSP identities on rail from
// Postgres alone, oldest first: what was provisioned, whether or not a
// configuration names it now. Only identity fields are set.
func (s *Service) PSPIdentities(ctx context.Context, id billing.MerchantID, rail string) ([]PSPScope, error) {
	if s == nil || s.database == nil || id.IsZero() {
		return nil, nil
	}
	var rows []gen.BillingPsp
	err := s.database.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
		var err error
		rows, err = s.database.Gen(ctx).ListPSPsForMerchant(ctx, id.UUID())
		return err
	})
	if err != nil {
		return nil, err
	}
	var out []PSPScope
	for _, row := range rows {
		if row.SupersededAt != nil || row.Rail != rail {
			continue
		}
		scope := PSPScope{ID: row.ID, Rail: row.Rail, Environment: row.Environment, AccountID: row.AccountID, Key: row.Key, CreatedAt: row.CreatedAt}
		out = append(out, scope)
	}
	return out, nil
}

// PSPScopes lists every PSP identity of the merchant, archived ones too,
// oldest first.
func (s *Service) PSPScopes(ctx context.Context, id billing.MerchantID) ([]PSPScope, error) {
	j, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	return j.psps, nil
}

// LivePSPScopes lists every live PSP of the merchant, by rail and newest
// first.
func (s *Service) LivePSPScopes(ctx context.Context, id billing.MerchantID) ([]PSPScope, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return nil, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list PSPs: %w", err)
	}
	live := j.live("")
	sort.SliceStable(live, func(a, b int) bool { return live[a].Rail < live[b].Rail })
	return live, nil
}

// PullPSPScope resolves the PSP the pull plane (provider refresh,
// unknown-cohort resolution, probes — #699) reads with: the active PSP for new
// work when one exists, else the newest archived one, which stays
// pull-addressable so its obligations drain (#655). ok=false when the merchant
// has no PSP on the rail at all.
func (s *Service) PullPSPScope(ctx context.Context, id billing.MerchantID, rail string) (PSPScope, bool, error) {
	scope, ok, err := s.ActivePSPScope(ctx, id, rail)
	if !errors.Is(err, ErrNoActivePSP) {
		return scope, ok, err
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return PSPScope{}, false, err
	}
	rail = normalizeProviderSecretType(rail)
	for _, p := range slices.Backward(j.psps) {
		if p.Rail == rail {
			return p, true, nil
		}
	}
	return PSPScope{}, false, nil
}

// PSPScopeByAccountID resolves a PSP by its rail-native account_id (#641):
// the live one, else the newest archived one. Archived PSPs remain
// addressable for inbound webhooks and existing obligations.
func (s *Service) PSPScopeByAccountID(ctx context.Context, id billing.MerchantID, rail, accountID string) (PSPScope, bool, error) {
	accountID = strings.TrimSpace(accountID)
	if s == nil || s.pool == nil || id.IsZero() || accountID == "" {
		return PSPScope{}, false, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return PSPScope{}, false, fmt.Errorf("load PSP %s/%s: %w", rail, accountID, err)
	}
	rail = normalizeProviderSecretType(rail)
	var found PSPScope
	ok := false
	for _, p := range slices.Backward(j.psps) {
		if p.Rail == rail && p.AccountID == accountID && (!ok || found.Archived && !p.Archived) {
			found, ok = p, true
		}
	}
	return found, ok, nil
}

// NMIWebhookSecrets are the secrets an NMI webhook may be signed with: the
// current one and, only during its bounded overlap (SEC-29), the previous one.
type NMIWebhookSecrets struct {
	Current, Previous string
}

// LoadNMIWebhookSecrets loads an NMI PSP's webhook secrets; none when no
// document declares it.
func (s *Service) LoadNMIWebhookSecrets(ctx context.Context, id billing.MerchantID, pspID uuid.UUID) (NMIWebhookSecrets, error) {
	scope, ok, err := s.PSPScopeByID(ctx, id, pspID)
	if err != nil || !ok || scope.Rail != string(models.RailNMI) {
		return NMIWebhookSecrets{}, err
	}
	out := NMIWebhookSecrets{Current: scope.Secret("webhook_signing_secret")}
	if s.overlapOpen(scope) {
		out.Previous = scope.Secret("webhook_signing_secret_previous")
	}
	return out, nil
}

// LoadStripeCredentialsForPSP loads a Stripe PSP's credentials; none when no
// document declares it.
func (s *Service) LoadStripeCredentialsForPSP(ctx context.Context, id billing.MerchantID, pspID uuid.UUID) (StripeCredentials, error) {
	scope, ok, err := s.PSPScopeByID(ctx, id, pspID)
	if err != nil || !ok || scope.Rail != string(models.RailStripe) {
		return StripeCredentials{}, err
	}
	return s.stripeCredentials(scope), nil
}

// ResolveActivePSPIDForRail returns the PSP whose credentials the account-less
// webhook routes verify with: the account whose secret validated the
// signature is the attribution (or#893). ok=false when nothing is armed on the
// rail.
func (s *Service) ResolveActivePSPIDForRail(ctx context.Context, id billing.MerchantID, rail string) (uuid.UUID, bool, error) {
	scope, ok, err := s.ActivePSPScope(ctx, id, rail)
	if err != nil || !ok {
		return uuid.Nil, false, err
	}
	return scope.ID, scope.ID != uuid.Nil, nil
}

// PSPIdentity is the globally unique rail-native PSP identity plus the
// merchant that owns it.
type PSPIdentity struct {
	ID          uuid.UUID
	MerchantID  billing.MerchantID
	Key         string
	Rail        string
	Environment string
	AccountID   string
	Superseded  bool
}

// ErrPSPOwnedByAnotherMerchant signals that a (rail, environment,
// account_id) is already registered to a DIFFERENT merchant. PSPs
// are globally unique to one merchant (#650) and are never shared or moved; the
// upsert rejects a cross-merchant claim, but with an opaque no-rows /
// unique-violation error — this names the conflict instead.
var ErrPSPOwnedByAnotherMerchant = apperr.New(http.StatusConflict, billing.CodePSPExists, "PSP is already owned by another merchant")

// PSPNaturalKey canonicalizes a PSP's GLOBAL natural key (rail, money class,
// account_id) and derives its deterministic id from it (#662). It is the
// SINGLE place this canonicalization lives — the same normalization the
// ownership guard and secret layer use (lower(rail); the declared environment;
// account_id trimmed). Every non-live environment is one money class: the id
// is derived from the class, so a PSP re-declared between them keeps its id.
// Every writer stores the returned nRail/nEnv/nAccount: the row holds the
// declared environment beside the class-derived id.
func PSPNaturalKey(rail, environment, accountID string) (id uuid.UUID, nRail, nEnv, nAccount string) {
	nRail = normalizeProviderSecretType(rail)
	nEnv = normalizePSPEnvironment(nRail, environment)
	nAccount = strings.TrimSpace(accountID)
	id = uuidutil.DeterministicID(uuidutil.DeterministicNamespace, nRail, config.EnvironmentClass(nRail, nEnv), nAccount)
	return id, nRail, nEnv, nAccount
}

// PspID returns just the deterministic id for a PSP's natural key (#662) — for callers that already hold the row and only
// need to compute or match its id.
func PspID(rail, environment, accountID string) uuid.UUID {
	id, _, _, _ := PSPNaturalKey(rail, environment, accountID)
	return id
}

// AssertPSPUnowned is a clear-error preflight for the
// global-uniqueness upsert: it returns ErrPSPOwnedByAnotherMerchant
// (wrapped with the conflicting identity) when (rail, environment, account_id)
// already belongs to a merchant other than merchantID, and nil when the account
// is unclaimed or already this merchant's.
//
// A webhook names an account in neither money class, so a new claim is also
// refused while another merchant's current PSP holds the account in the other
// class. A superseded or archived one does not hold it: rows are never
// deleted, and a squatted or retired account must not block its owner for
// good. A merchant's own live and non-live PSPs may share an account.
//
// #824: the ownership question is global by definition, so it goes through the
// cross-merchant directory lookup (resolvePSPOwner). Reading psps directly on
// q could only ever see the caller's OWN merchant, which made this assertion
// pass unconditionally — the only thing still catching a hijack was UpsertPSP's
// `ON CONFLICT … WHERE psps.merchant_id = EXCLUDED.merchant_id`, and it reports
// the conflict as an opaque no-rows.
func (s *Service) AssertPSPUnowned(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, rail, environment, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if q == nil || accountID == "" {
		return nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizePSPEnvironment(rail, environment)
	if environment == "" {
		return fmt.Errorf("merchants: PSP environment must be one of %s's (%s)", rail, strings.Join(rails.Environments(models.Rail(rail)), ", "))
	}
	live := config.LiveEnvironment(rail, environment)
	owner, found, err := resolvePSPOwner(ctx, q, rail, live, accountID)
	if err != nil {
		return err
	}
	if found {
		if uuid.UUID(owner.MerchantID) != merchantID {
			return fmt.Errorf("PSP %s:%s (%s): %w", owner.Rail, owner.AccountID, owner.Environment, ErrPSPOwnedByAnotherMerchant)
		}
		return nil
	}
	other, found, err := resolvePSPOwner(ctx, q, rail, !live, accountID)
	if err != nil || !found || uuid.UUID(other.MerchantID) == merchantID {
		return err
	}
	current, err := s.pspCurrent(ctx, other)
	if err != nil {
		return err
	}
	if current {
		return fmt.Errorf("PSP %s:%s (%s): %w", other.Rail, other.AccountID, other.Environment, ErrPSPOwnedByAnotherMerchant)
	}
	return nil
}

// pspCurrent reports whether another merchant's PSP takes work: not
// superseded, and named by an unarchived document under its key.
func (s *Service) pspCurrent(ctx context.Context, p PSPIdentity) (bool, error) {
	if p.Superseded {
		return false, nil
	}
	set, known, err := s.foreignDocuments(ctx, p.MerchantID)
	if err != nil || !known {
		return !known, err
	}
	doc, ok := set.PSPs[strings.ToLower(p.Key)]
	if !ok || doc.Value.Archived {
		return false, nil
	}
	return strings.EqualFold(doc.Value.Rail, p.Rail) && strings.TrimSpace(doc.Value.AccountID) == p.AccountID &&
		config.LiveEnvironment(p.Rail, doc.Value.Environment) == config.LiveEnvironment(p.Rail, p.Environment), nil
}

// foreignDocuments reads another merchant's documents from the
// configuration's source, not its cache: the caller may be reloading a
// merchant itself. known is false while a file has not declared the merchant
// (boot puts merchants one at a time), and its identities then count as
// current.
func (s *Service) foreignDocuments(ctx context.Context, id billing.MerchantID) (merchantdocs.Set, bool, error) {
	if s == nil || s.config == nil {
		return merchantdocs.Set{}, false, nil
	}
	source := s.config.Source()
	if files, ok := source.(*merchantdocs.FileSource); ok && !files.Declared(id) {
		return merchantdocs.Set{}, false, nil
	}
	set, err := source.Load(ctx, id)
	return set, err == nil, err
}

// ResolvePSPByIdentity resolves an account globally by
// its rail-native identity. Use this at webhook/callback boundaries where the
// provider payload or route carries account_id and the merchant should be derived
// from the account row.
//
// This is a genuinely cross-merchant read — inbound webhooks have no
// merchant context yet — by the global (rail, money class, account_id) key.
func (s *Service) ResolvePSPByIdentity(ctx context.Context, rail, environment, accountID string) (PSPIdentity, bool, error) {
	if s == nil || s.pool == nil || strings.TrimSpace(accountID) == "" {
		return PSPIdentity{}, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizePSPEnvironment(rail, environment)
	if environment == "" {
		return PSPIdentity{}, false, fmt.Errorf("merchants: PSP environment must be one of %s's (%s)", rail, strings.Join(rails.Environments(models.Rail(rail)), ", "))
	}
	return resolvePSPOwner(ctx, gen.New(s.pool), rail, config.LiveEnvironment(rail, environment), strings.TrimSpace(accountID))
}

// ResolvePSPsByAccount lists the PSPs an account_id names on rail, live
// first: an inbound webhook names no environment, so a live and a non-live
// PSP may share it. The caller tells them apart by whose secret verifies the
// delivery.
func (s *Service) ResolvePSPsByAccount(ctx context.Context, rail, accountID string) ([]PSPIdentity, error) {
	if s == nil || s.pool == nil || strings.TrimSpace(accountID) == "" {
		return nil, nil
	}
	rail = normalizeProviderSecretType(rail)
	var found []PSPIdentity
	for _, live := range []bool{true, false} {
		owner, ok, err := resolvePSPOwner(ctx, gen.New(s.pool), rail, live, strings.TrimSpace(accountID))
		if err != nil {
			return nil, err
		}
		if ok {
			found = append(found, owner)
		}
	}
	return found, nil
}

// resolvePSPOwner is the one place the cross-merchant PSP directory lookup is
// made. q may be bound to anything (pool, merchant-pinned conn, tx): the
// definer function is what supplies cross-merchant visibility, not the handle.
func resolvePSPOwner(ctx context.Context, q *gen.Queries, rail string, live bool, accountID string) (PSPIdentity, bool, error) {
	row, err := q.ResolvePSPOwnerByRailIdentity(ctx, gen.ResolvePSPOwnerByRailIdentityParams{
		Rail:      rail,
		Live:      live,
		AccountID: accountID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return PSPIdentity{}, false, nil
	}
	if err != nil {
		return PSPIdentity{}, false, err
	}
	return PSPIdentity{
		ID:          row.ID,
		MerchantID:  billing.MerchantID(row.MerchantID),
		Key:         row.Key,
		Rail:        row.Rail,
		Environment: row.Environment,
		AccountID:   row.AccountID,
		Superseded:  row.SupersededAt != nil,
	}, true, nil
}

// LiveRailPresence is the TRI-state answer to "does a live PSP exist on this
// rail, anywhere in the catalog?". The zero value is Unknown so a dropped or
// defaulted result fails closed.
type LiveRailPresence int

const (
	// LiveRailUnknown means the probe proved nothing. Callers MUST treat it
	// exactly as they treat LiveRailPresent.
	LiveRailUnknown LiveRailPresence = iota
	LiveRailPresent
	LiveRailAbsent
)

func (p LiveRailPresence) String() string {
	switch p {
	case LiveRailPresent:
		return "present"
	case LiveRailAbsent:
		return "absent"
	default:
		return "unknown"
	}
}

// ProbeLiveAccount reports whether the PSPs account names on rail move real
// money: absent only when such a PSP exists and none does. Webhook ingestion
// has no merchant yet, so it asks by account.
func (s *Service) ProbeLiveAccount(ctx context.Context, rail, account string) (LiveRailPresence, error) {
	if s == nil || s.pool == nil {
		return LiveRailUnknown, errors.New("merchants: pgx pool is required")
	}
	rail, account = normalizeProviderSecretType(rail), strings.TrimSpace(account)
	if _, live, err := resolvePSPOwner(ctx, gen.New(s.pool), rail, true, account); err != nil || live {
		return LiveRailPresent, err
	}
	if _, found, err := resolvePSPOwner(ctx, gen.New(s.pool), rail, false, account); err != nil || !found {
		return LiveRailUnknown, err
	}
	return LiveRailAbsent, nil
}

// ProbeLiveCardPSP reports whether the merchant has a live PSP whose rail
// charges cards: absent when every card PSP it serves moves no money.
func (s *Service) ProbeLiveCardPSP(ctx context.Context, id billing.MerchantID) (LiveRailPresence, error) {
	set, err := s.Config().Get(ctx, id)
	if err != nil {
		return LiveRailUnknown, err
	}
	for _, p := range liveCardPSPs(set.PSPs) {
		if config.LiveEnvironment(p.Rail, p.Environment) {
			return LiveRailPresent, nil
		}
	}
	return LiveRailAbsent, nil
}

// CountActivePSPsForRail is how many PSPs the merchant has live for new work
// on a rail. More than one only warns at credential resolution (the newest
// wins), but a pull arms from exactly ONE PSP, so a rail with N>1 live PSPs is
// only partially covered (#841).
func (s *Service) CountActivePSPsForRail(ctx context.Context, id billing.MerchantID, rail string) (int, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return 0, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("count active PSPs %s: %w", rail, err)
	}
	return len(j.live(normalizeProviderSecretType(rail))), nil
}

// PSPScopeByID resolves a PSP by its id: the account existing obligations
// name, archived or not.
func (s *Service) PSPScopeByID(ctx context.Context, id billing.MerchantID, pspID uuid.UUID) (PSPScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() || pspID == uuid.Nil {
		return PSPScope{}, false, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return PSPScope{}, false, err
	}
	p, ok := j.byID(pspID)
	return p, ok, nil
}
