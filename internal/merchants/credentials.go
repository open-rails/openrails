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
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
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

// ErrNoActivePSP means the merchant has PSPs on the
// requested rail/environment, but all are archived and therefore drain-only.
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
	scope, ok, err := s.ActivePSPScope(ctx, id, "stripe", s.providerEnvironment)
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
	scope, ok, err := s.ActivePSPScope(ctx, id, "nmi", s.providerEnvironment)
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
	scope, ok, err := s.ActivePSPScope(ctx, id, "nmi", s.providerEnvironment)
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

// ActivePSPSecretName resolves the active PSP for a merchant rail/environment
// and returns that account's credential name for key.
func (s *Service) ActivePSPSecretName(ctx context.Context, id billing.MerchantID, rail, environment, key string) (string, bool, error) {
	ref, ok, err := s.ActivePSPSecretRef(ctx, id, rail, environment, key)
	return ref.Name, ok, err
}

// ActivePSPSecretRef is ActivePSPSecretName as a SecretRef.
func (s *Service) ActivePSPSecretRef(ctx context.Context, id billing.MerchantID, rail, environment, key string) (SecretRef, bool, error) {
	scope, ok, err := s.ActivePSPScope(ctx, id, rail, environment)
	if err != nil || !ok {
		return SecretRef{}, ok, err
	}
	ref, err := scope.SecretRef(key)
	if err != nil {
		return SecretRef{}, false, err
	}
	return ref, true, nil
}

// ActivePSPScope resolves the PSP new work on a merchant rail/environment
// uses: the newest live one. A rail whose PSPs are all archived is
// ErrNoActivePSP.
func (s *Service) ActivePSPScope(ctx context.Context, id billing.MerchantID, rail, environment string) (PSPScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return PSPScope{}, false, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return PSPScope{}, false, fmt.Errorf("PSP environment must be live or test")
	}
	rail = normalizeProviderSecretType(rail)
	j, err := s.load(ctx, id)
	if err != nil {
		return PSPScope{}, false, fmt.Errorf("load active PSP %s/%s: %w", rail, environment, err)
	}
	live := j.live(rail, environment)
	if len(live) > 1 {
		log.WithFields(log.Fields{"merchant_id": id.String(), "rail": rail, "environment": environment, "active_count": len(live)}).
			Warn("multiple active PSPs configured; using newest for new work")
	}
	if len(live) == 0 {
		for _, p := range j.psps {
			if p.Rail == rail && p.Environment == environment {
				return PSPScope{}, false, ErrNoActivePSP
			}
		}
		return PSPScope{}, false, nil
	}
	return live[0], true, nil
}

// PSPKeyArchived reports whether key names an ARCHIVED PSP of the merchant in
// environment, so routing can say "retired" instead of "unknown".
func (s *Service) PSPKeyArchived(ctx context.Context, id billing.MerchantID, key, environment string) (bool, error) {
	key = strings.TrimSpace(key)
	if s == nil || s.pool == nil || id.IsZero() || key == "" {
		return false, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return false, fmt.Errorf("PSP environment must be live or test")
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return false, fmt.Errorf("load archived PSP by key %s/%s: %w", key, environment, err)
	}
	live := false
	archived := false
	for _, p := range j.psps {
		if strings.EqualFold(p.Key, key) && p.Environment == environment {
			live = live || !p.Archived
			archived = archived || p.Archived
		}
	}
	return archived && !live, nil
}

// PSPScopeByKey resolves a live PSP by its key in environment: how the
// payment-provider vocabulary of the catalog and checkout resolves to a
// concrete account.
func (s *Service) PSPScopeByKey(ctx context.Context, id billing.MerchantID, key, environment string) (PSPScope, bool, error) {
	key = strings.TrimSpace(key)
	if s == nil || s.pool == nil || id.IsZero() || key == "" {
		return PSPScope{}, false, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return PSPScope{}, false, fmt.Errorf("PSP environment must be live or test")
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return PSPScope{}, false, fmt.Errorf("load PSP by key %s/%s: %w", key, environment, err)
	}
	for _, p := range j.live("", environment) {
		if strings.EqualFold(p.Key, key) {
			return p, true, nil
		}
	}
	return PSPScope{}, false, nil
}

// ActivePSPScopesForRail lists every live PSP on rail/environment, newest
// first. Checkout accepts a bare rail-kind selector only when it is
// unambiguous (#848).
func (s *Service) ActivePSPScopesForRail(ctx context.Context, id billing.MerchantID, rail, environment string) ([]PSPScope, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return nil, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return nil, fmt.Errorf("PSP environment must be live or test")
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list PSPs %s/%s: %w", rail, environment, err)
	}
	return j.live(normalizeProviderSecretType(rail), environment), nil
}

// PSPIdentities lists the merchant's current PSP identities on rail in
// environment from Postgres alone, oldest first: what was provisioned, whether
// or not a configuration names it now. Only identity fields are set.
func (s *Service) PSPIdentities(ctx context.Context, id billing.MerchantID, rail, environment string) ([]PSPScope, error) {
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
		if row.SupersededAt != nil || row.Rail != rail || row.Environment != environment {
			continue
		}
		scope := PSPScope{ID: row.ID, Rail: row.Rail, Environment: row.Environment, AccountID: row.AccountID, Key: row.Key, CreatedAt: row.CreatedAt}
		if row.PendingSignerPublicKey != nil {
			scope.SignerChange = *row.PendingSignerPublicKey
		}
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

// LivePSPScopes lists every live PSP of the merchant in environment, by rail
// and newest first.
func (s *Service) LivePSPScopes(ctx context.Context, id billing.MerchantID, environment string) ([]PSPScope, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return nil, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return nil, fmt.Errorf("PSP environment must be live or test")
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list PSPs for %s: %w", environment, err)
	}
	live := j.live("", environment)
	sort.SliceStable(live, func(a, b int) bool { return live[a].Rail < live[b].Rail })
	return live, nil
}

// PullPSPScope resolves the PSP the pull plane (provider refresh,
// unknown-cohort resolution, probes — #699) reads with: the active PSP for new
// work when one exists, else the newest archived one, which stays
// pull-addressable so its obligations drain (#655). ok=false when the merchant
// has no PSP on the rail/environment at all.
func (s *Service) PullPSPScope(ctx context.Context, id billing.MerchantID, rail, environment string) (PSPScope, bool, error) {
	scope, ok, err := s.ActivePSPScope(ctx, id, rail, environment)
	if !errors.Is(err, ErrNoActivePSP) {
		return scope, ok, err
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return PSPScope{}, false, err
	}
	rail, environment = normalizeProviderSecretType(rail), normalizeProviderSecretEnvironment(environment)
	for _, p := range slices.Backward(j.psps) {
		if p.Rail == rail && p.Environment == environment {
			return p, true, nil
		}
	}
	return PSPScope{}, false, nil
}

// PSPScopeByAccountID resolves a PSP of the deployment's environment by its
// rail-native account_id (#641). Archived PSPs remain addressable for inbound
// webhooks and existing obligations.
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
	for _, p := range j.psps {
		if p.Rail == rail && p.Environment == s.providerEnvironment && p.AccountID == accountID {
			return p, true, nil
		}
	}
	return PSPScope{}, false, nil
}

// NMIWebhookSecrets are the secrets an NMI webhook may be signed with: the
// current one and, only during its bounded overlap (SEC-29), the previous one.
type NMIWebhookSecrets struct {
	Current, Previous string
}

// LoadNMIWebhookSigningSecretForAccount loads the webhook secrets of an NMI
// account (#641). ok=false when the merchant has no such account: reject.
func (s *Service) LoadNMIWebhookSigningSecretForAccount(ctx context.Context, id billing.MerchantID, accountID string) (NMIWebhookSecrets, bool, error) {
	scope, ok, err := s.PSPScopeByAccountID(ctx, id, string(models.RailNMI), accountID)
	if err != nil || !ok {
		return NMIWebhookSecrets{}, ok, err
	}
	out := NMIWebhookSecrets{Current: scope.Secret("webhook_signing_secret")}
	if s.overlapOpen(scope) {
		out.Previous = scope.Secret("webhook_signing_secret_previous")
	}
	return out, true, nil
}

// LoadStripeCredentialsForAccount loads the credentials of a Stripe account
// (#641). ok=false when the merchant has no such account.
func (s *Service) LoadStripeCredentialsForAccount(ctx context.Context, id billing.MerchantID, accountID string) (StripeCredentials, bool, error) {
	scope, ok, err := s.PSPScopeByAccountID(ctx, id, "stripe", accountID)
	if err != nil || !ok {
		return StripeCredentials{}, ok, err
	}
	return s.stripeCredentials(scope), true, nil
}

// ResolvePSPID returns the id of an account by account_id (#641), to stamp
// records from a per-account webhook. ok=false when none matches.
func (s *Service) ResolvePSPID(ctx context.Context, id billing.MerchantID, rail, accountID string) (uuid.UUID, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() || strings.TrimSpace(accountID) == "" {
		return uuid.Nil, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	var pid uuid.UUID
	err := s.database.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
		var err error
		pid, err = s.database.Gen(ctx).GetPSPIDByRailAccount(ctx, gen.GetPSPIDByRailAccountParams{
			MerchantID: id.UUID(), Rail: rail, AccountID: strings.TrimSpace(accountID),
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return pid, true, nil
}

// ResolveActivePSPIDForRail returns the PSP whose credentials the account-less
// webhook routes verify with: the account whose secret validated the
// signature is the attribution (or#893). ok=false when nothing is armed on the
// rail.
func (s *Service) ResolveActivePSPIDForRail(ctx context.Context, id billing.MerchantID, rail string) (uuid.UUID, bool, error) {
	scope, ok, err := s.ActivePSPScope(ctx, id, rail, s.providerEnvironment)
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
	Rail        string
	Environment string
	AccountID   string
}

// ErrPSPOwnedByAnotherMerchant signals that a (rail, environment,
// account_id) is already registered to a DIFFERENT merchant. PSPs
// are globally unique to one merchant (#650) and are never shared or moved; the
// upsert rejects a cross-merchant claim, but with an opaque no-rows /
// unique-violation error — this names the conflict instead.
var ErrPSPOwnedByAnotherMerchant = apperr.New(http.StatusConflict, "psp_exists", "PSP is already owned by another merchant")

// PSPNaturalKey canonicalizes a PSP's GLOBAL
// natural key (rail, environment, account_id) and derives its deterministic id
// from it (#662). It is the SINGLE place this canonicalization lives — the same
// normalization the ownership guard and secret layer use (lower(rail);
// environment mapped to live/test; account_id trimmed). Every writer stores the
// returned nRail/nEnv/nAccount so the stored row matches the id and the
// (rail, environment, account_id) unique index exactly; the id is a pure
// function of that key, identical across environments and fresh rebuilds.
func PSPNaturalKey(rail, environment, accountID string) (id uuid.UUID, nRail, nEnv, nAccount string) {
	nRail = normalizeProviderSecretType(rail)
	nEnv = normalizeProviderSecretEnvironment(environment)
	nAccount = strings.TrimSpace(accountID)
	id = uuidutil.DeterministicID(uuidutil.DeterministicNamespace, nRail, nEnv, nAccount)
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
// #824: the ownership question is global by definition, so it goes through the
// cross-merchant directory function (migration 0016). Reading psps directly on
// q could only ever see the caller's OWN merchant, which made this assertion
// pass unconditionally — the only thing still catching a hijack was UpsertPSP's
// `ON CONFLICT … WHERE psps.merchant_id = EXCLUDED.merchant_id`, and it reports
// the conflict as an opaque no-rows.
func AssertPSPUnowned(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, rail, environment, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if q == nil || accountID == "" {
		return nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return errors.New("merchants: PSP environment must be live or test")
	}
	owner, found, err := resolvePSPOwner(ctx, q, rail, environment, accountID)
	if err != nil || !found {
		return err
	}
	if uuid.UUID(owner.MerchantID) != merchantID {
		return fmt.Errorf("PSP %s:%s (%s): %w", owner.Rail, owner.AccountID, owner.Environment, ErrPSPOwnedByAnotherMerchant)
	}
	return nil
}

// ResolvePSPByIdentity resolves an account globally by
// its rail-native identity. Use this at webhook/callback boundaries where the
// provider payload or route carries account_id and the merchant should be derived
// from the account row.
//
// This is a genuinely cross-merchant read — inbound webhooks have no
// merchant context yet — by the global (rail, environment, account_id) key.
func (s *Service) ResolvePSPByIdentity(ctx context.Context, rail, environment, accountID string) (PSPIdentity, bool, error) {
	if s == nil || s.pool == nil || strings.TrimSpace(accountID) == "" {
		return PSPIdentity{}, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return PSPIdentity{}, false, errors.New("merchants: PSP environment must be live or test")
	}
	return resolvePSPOwner(ctx, gen.New(s.pool), rail, environment, strings.TrimSpace(accountID))
}

// or#880: the custody sibling of ResolvePSPByIdentity moved to
// custodians.go (ResolveCustodianByIdentity). It resolves the CUSTODIAN, not a
// PSP — one custodian may back several, so "the" PSP was never well defined.

// resolvePSPOwner is the one place the cross-merchant PSP directory lookup is
// made. q may be bound to anything (pool, merchant-pinned conn, tx): the
// definer function is what supplies cross-merchant visibility, not the handle.
func resolvePSPOwner(ctx context.Context, q *gen.Queries, rail, environment, accountID string) (PSPIdentity, bool, error) {
	row, err := q.ResolvePSPOwnerByRailIdentity(ctx, gen.ResolvePSPOwnerByRailIdentityParams{
		Rail:        rail,
		Environment: environment,
		AccountID:   accountID,
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
		Rail:        row.Rail,
		Environment: row.Environment,
		AccountID:   row.AccountID,
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

// ProbeLiveRailPSPs reports whether ANY merchant holds a PSP on rail with
// environment=live, archived ones included. Deliberately cross-merchant:
// webhook ingestion has no merchant yet.
func (s *Service) ProbeLiveRailPSPs(ctx context.Context, rail string) (LiveRailPresence, error) {
	if s == nil || s.pool == nil {
		return LiveRailUnknown, errors.New("merchants: pgx pool is required")
	}
	rail = normalizeProviderSecretType(rail)
	if rail == "" {
		return LiveRailUnknown, errors.New("merchants: rail is required")
	}
	present, err := gen.New(s.pool).PSPExistsOnRail(ctx, gen.PSPExistsOnRailParams{Rail: rail, Environment: "live"})
	if err != nil {
		return LiveRailUnknown, fmt.Errorf("merchants: probe live %s PSPs: %w", rail, err)
	}
	if present {
		return LiveRailPresent, nil
	}
	return LiveRailAbsent, nil
}

// CountActivePSPsForRail is how many PSPs the merchant has live for new work
// on a rail/environment. More than one only warns at credential resolution
// (the newest wins), but a pull arms from exactly ONE PSP, so a rail with N>1
// live PSPs is only partially covered (#841).
func (s *Service) CountActivePSPsForRail(ctx context.Context, id billing.MerchantID, rail, environment string) (int, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return 0, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return 0, fmt.Errorf("PSP environment must be live or test")
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("count active PSPs %s/%s: %w", rail, environment, err)
	}
	return len(j.live(normalizeProviderSecretType(rail), environment)), nil
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
