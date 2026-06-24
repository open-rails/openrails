package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ProviderAccountIdentity is the provider ACCOUNT the CURRENT credentials for
// a provider key point at (#518). AccountID is provider-returned or
// provider-configured account identity, never a credential hash.
type ProviderAccountIdentity struct {
	ProviderKey  string
	ProviderType string
	Environment  string
	AccountID    string
	DisplayName  string
	Evidence     map[string]any
}

// ProviderAccountResolver resolves the provider account identity for a
// provider key. Intents are stamped with the matching provider_accounts row at
// enqueue; the executor/verifier compare before touching the provider and park
// on mismatch. A queue built against one account must never execute against
// another account: NMI object ids are small numerics that collide across
// accounts, so the wrong account's subscription could be deleted or charged.
//
// ok=false means "no account identity for this provider" — the check is skipped,
// both at stamp time and at execution. Deliberately exempt:
//   - solana: addresses are globally-unique pubkeys; an object from the wrong
//     cluster/keypair simply does not exist, execution cannot hit a wrong one.
//   - providers with no resolvable credentials (unconfigured clients park on
//     their own already).
type ProviderAccountResolver interface {
	ResolveProviderAccount(ctx context.Context, provider string) (ProviderAccountIdentity, bool)
}

// RuntimeProviderAccounts is the production ProviderAccountResolver over the
// runtime's live provider credentials. Resolvers identify the ACCOUNT, never
// the credential, so same-account key rotation never false-parks anything:
//
//   - NMI-backed providers: the merchant identity from the gateway's own
//     profile report (nmi.NMIClient.AccountIdentity — query.php
//     report_type=profile), fetched lazily and cached per security key.
//   - stripe-type rails: the account id from GET /v1/account,
//     fetched lazily through the stripeapi choke point and cached per secret
//     key.
//   - CCBill/Solana: account identity comes from the configured provider
//     account fields because those rails do not currently have an HTTP whoami
//     integration in OpenRails.
//
// A fetch failure resolves to ok=false (check skipped this pass, warn logged)
// rather than blocking the ledger on provider availability.
type RuntimeProviderAccounts struct {
	Config *config.Config
	Rails  config.RailSet
	NMI    map[string]*nmi.NMIClient
	DB     *db.DB
	// StripeBaseURL overrides https://api.stripe.com in tests.
	StripeBaseURL string

	mu          sync.Mutex
	stripeCache map[string]string           // secret key -> account id
	nmiCache    map[string]nmiIdentityEntry // provider -> identity per security key
}

type nmiIdentityEntry struct {
	securityKey string
	accountID   string
}

// NewRuntimeProviderAccounts builds the resolver over the live clients/config.
func NewRuntimeProviderAccounts(cfg *config.Config, rails config.RailSet, nmiClients map[string]*nmi.NMIClient) *RuntimeProviderAccounts {
	return &RuntimeProviderAccounts{Config: cfg, Rails: rails, NMI: nmiClients}
}

func NewRuntimeProviderAccountsWithDB(cfg *config.Config, rails config.RailSet, nmiClients map[string]*nmi.NMIClient, database *db.DB) *RuntimeProviderAccounts {
	r := NewRuntimeProviderAccounts(cfg, rails, nmiClients)
	r.DB = database
	return r
}

func (r *RuntimeProviderAccounts) ResolveProviderAccount(ctx context.Context, provider string) (ProviderAccountIdentity, bool) {
	if r == nil {
		return ProviderAccountIdentity{}, false
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	if c, ok := r.NMI[p]; ok && c != nil {
		return r.nmiAccount(ctx, p, c)
	}
	if r.Rails != nil {
		if proc, ok := r.Rails[p]; ok && proc != nil {
			providerType := proc.GetEffectiveType(p)
			isStripe := providerType == config.RailTypeStripe
			if key := strings.TrimSpace(proc.SecretKey); isStripe && key != "" {
				accountID, ok := r.stripeAccountID(ctx, key)
				if !ok {
					return ProviderAccountIdentity{}, false
				}
				return ProviderAccountIdentity{
					ProviderKey:  p,
					ProviderType: config.RailTypeStripe,
					Environment:  "live",
					AccountID:    accountID,
					Evidence:     map[string]any{"source": "stripe.get_account"},
				}, true
			}
			switch providerType {
			case config.RailTypeCCBill:
				accountID := strings.TrimSpace(proc.ClientAccNum)
				if sub := strings.TrimSpace(proc.ClientSubAcc); accountID != "" && sub != "" {
					accountID += "/" + sub
				}
				if accountID != "" {
					return ProviderAccountIdentity{
						ProviderKey:  p,
						ProviderType: config.RailTypeCCBill,
						Environment:  "live",
						AccountID:    accountID,
						Evidence:     map[string]any{"source": "config.client_acc_num"},
					}, true
				}
			case config.RailTypeSolana:
				if accountID := strings.TrimSpace(proc.RecipientWallet); accountID != "" {
					return ProviderAccountIdentity{
						ProviderKey:  p,
						ProviderType: config.RailTypeSolana,
						Environment:  "live",
						AccountID:    accountID,
						Evidence:     map[string]any{"source": "config.recipient_wallet"},
					}, true
				}
			}
		}
	}
	if account, ok := r.dbPrimaryProviderAccount(ctx, p); ok {
		return account, true
	}
	return ProviderAccountIdentity{}, false
}

func (r *RuntimeProviderAccounts) dbPrimaryProviderAccount(ctx context.Context, provider string) (ProviderAccountIdentity, bool) {
	if r == nil || r.DB == nil || r.DB.Pool() == nil {
		return ProviderAccountIdentity{}, false
	}
	providerType := dbProviderType(provider)
	if providerType == "" {
		return ProviderAccountIdentity{}, false
	}
	mid, ok := merchant.FromContext(ctx)
	if !ok || mid.IsZero() {
		return ProviderAccountIdentity{}, false
	}
	var out ProviderAccountIdentity
	err := r.DB.Pool().QueryRow(ctx, `
		SELECT provider_type, environment, account_id, coalesce(display_name, '')
		  FROM openrails.provider_accounts
		 WHERE merchant_id = $1::uuid
		   AND provider_type = $2
		   AND environment = 'live'
		   AND role = 'primary'
		   AND status = 'enabled'
		 LIMIT 1
	`, mid.String(), providerType).Scan(&out.ProviderType, &out.Environment, &out.AccountID, &out.DisplayName)
	if err != nil {
		if err != pgx.ErrNoRows {
			log.WithContext(ctx).WithError(err).WithField("provider", provider).
				Warn("intents: db provider account lookup failed")
		}
		return ProviderAccountIdentity{}, false
	}
	out.ProviderKey = strings.ToLower(strings.TrimSpace(provider))
	out.Evidence = map[string]any{"source": "provider_accounts.primary"}
	return out, true
}

func dbProviderType(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "mobius", config.RailTypeNMI:
		return config.RailTypeNMI
	default:
		return ""
	}
}

// FindProviderAccount returns the currently configured credential set that
// resolves to providerType/accountID. It intentionally searches every configured
// local provider key: keys such as "stripe_primary" are convenience labels, not
// durable account identity.
func (r *RuntimeProviderAccounts) FindProviderAccount(ctx context.Context, providerType, environment, accountID string) (ProviderAccountIdentity, bool) {
	if r == nil {
		return ProviderAccountIdentity{}, false
	}
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	environment, err := normalizeProviderEnvironment(environment)
	if err != nil {
		return ProviderAccountIdentity{}, false
	}
	accountID = strings.TrimSpace(accountID)
	if providerType == "" || accountID == "" {
		return ProviderAccountIdentity{}, false
	}
	keys := r.providerKeysByType(providerType)
	for _, key := range keys {
		identity, ok := r.ResolveProviderAccount(ctx, key)
		identity.Environment, _ = normalizeProviderEnvironment(identity.Environment)
		if ok && identity.ProviderType == providerType && identity.Environment == environment && identity.AccountID == accountID {
			return identity, true
		}
	}
	return ProviderAccountIdentity{}, false
}

func (r *RuntimeProviderAccounts) providerKeysByType(providerType string) []string {
	keys := map[string]struct{}{}
	if providerType == config.RailTypeNMI {
		for key, client := range r.NMI {
			key = strings.ToLower(strings.TrimSpace(key))
			if key != "" && client != nil {
				keys[key] = struct{}{}
			}
		}
	}
	if r.Rails != nil {
		for key, proc := range r.Rails {
			key = strings.ToLower(strings.TrimSpace(key))
			if key == "" || proc == nil || proc.GetEffectiveType(key) != providerType {
				continue
			}
			keys[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (r *RuntimeProviderAccounts) nmiAccount(ctx context.Context, provider string, c *nmi.NMIClient) (ProviderAccountIdentity, bool) {
	r.mu.Lock()
	if e, ok := r.nmiCache[provider]; ok && e.securityKey == c.SecurityKey && e.accountID != "" {
		accountID := e.accountID
		r.mu.Unlock()
		return ProviderAccountIdentity{
			ProviderKey:  provider,
			ProviderType: config.RailTypeNMI,
			Environment:  "live",
			AccountID:    accountID,
			DisplayName:  accountID,
			Evidence:     map[string]any{"source": "nmi.profile"},
		}, true
	}
	r.mu.Unlock()

	identity, err := c.AccountIdentity()
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("provider", provider).
			Warn("intents: nmi account identity fetch failed; account guard skipped this pass")
		return ProviderAccountIdentity{}, false
	}
	accountID := strings.TrimPrefix(identity, "nmi:")

	r.mu.Lock()
	if r.nmiCache == nil {
		r.nmiCache = map[string]nmiIdentityEntry{}
	}
	r.nmiCache[provider] = nmiIdentityEntry{securityKey: c.SecurityKey, accountID: accountID}
	r.mu.Unlock()
	return ProviderAccountIdentity{
		ProviderKey:  provider,
		ProviderType: config.RailTypeNMI,
		Environment:  "live",
		AccountID:    accountID,
		DisplayName:  accountID,
		Evidence:     map[string]any{"source": "nmi.profile"},
	}, true
}

func normalizeProviderEnvironment(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "live", "prod", "production", "mainnet":
		return "live", nil
	case "test", "sandbox", "devnet", "testnet":
		return "test", nil
	default:
		return "", fmt.Errorf("invalid provider environment %q", raw)
	}
}

func (r *RuntimeProviderAccounts) stripeAccountID(ctx context.Context, key string) (string, bool) {
	r.mu.Lock()
	if accountID := r.stripeCache[key]; accountID != "" {
		r.mu.Unlock()
		return accountID, true
	}
	r.mu.Unlock()

	base := r.StripeBaseURL
	if base == "" {
		base = "https://api.stripe.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/account", nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := stripeapi.Client(r.Config, 0).Do(req)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("intents: stripe account identity fetch failed; account guard skipped this pass")
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.WithContext(ctx).WithField("status", resp.StatusCode).Warn("intents: stripe account identity fetch non-200; account guard skipped this pass")
		return "", false
	}
	var account struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&account); err != nil || account.ID == "" {
		log.WithContext(ctx).WithError(err).Warn("intents: stripe account identity decode failed; account guard skipped this pass")
		return "", false
	}

	r.mu.Lock()
	if r.stripeCache == nil {
		r.stripeCache = map[string]string{}
	}
	r.stripeCache[key] = account.ID
	r.mu.Unlock()
	return account.ID, true
}

// accountMismatchReason formats the park/defer reason for a provider account
// mismatch. Exported shape is stable: it surfaces verbatim in
// last_failure_reason and the intents CLI.
func accountMismatchReason(stamped, current string) string {
	return fmt.Sprintf("provider account changed since enqueue (intent %s vs current %s); rebind after confirming, or restore the original credentials", stamped, current)
}
