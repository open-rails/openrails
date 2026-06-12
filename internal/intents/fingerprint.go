package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

// FingerprintSource resolves the fingerprint of the provider ACCOUNT the
// CURRENT credentials for a provider key point at (#365). Intents are stamped
// with this at enqueue; the executor/verifier compare before touching the
// provider and park on mismatch — a queue built against one account must
// never execute against another (NMI object ids are small numerics that
// collide across accounts: the wrong account's subscription would be deleted,
// the wrong customer's card charged).
//
// ok=false means "no fingerprint for this provider" — the check is skipped,
// both at stamp time and at execution. Deliberately exempt:
//   - solana: addresses are globally-unique pubkeys; an object from the wrong
//     cluster/keypair simply does not exist, execution cannot hit a wrong one.
//   - providers with no resolvable credentials (unconfigured clients park on
//     their own already).
type FingerprintSource interface {
	AccountFingerprint(ctx context.Context, provider string) (string, bool)
}

// RuntimeFingerprints is the production FingerprintSource over the runtime's
// live provider credentials. Both resolvers identify the ACCOUNT, never the
// credential, so same-account key rotation never false-parks anything:
//
//   - NMI-backed providers: the merchant identity from the gateway's own
//     profile report (nmi.NMIClient.AccountIdentity — query.php
//     report_type=profile), fetched lazily and cached per security key.
//   - stripe-type processors: "stripe:" + the account id from GET /v1/account,
//     fetched lazily through the stripeapi choke point and cached per secret
//     key.
//
// A fetch failure resolves to ok=false (check skipped this pass, warn logged)
// rather than blocking the ledger on provider availability.
type RuntimeFingerprints struct {
	Config *config.Config
	NMI    map[string]*nmi.NMIClient
	// StripeBaseURL overrides https://api.stripe.com in tests.
	StripeBaseURL string

	mu        sync.Mutex
	stripeKey string // secret key the cached account id was fetched with
	stripeFP  string
	nmiCache  map[string]nmiIdentityEntry // provider -> identity per security key
}

type nmiIdentityEntry struct {
	securityKey string
	fingerprint string
}

// NewRuntimeFingerprints builds the source over the live clients/config.
func NewRuntimeFingerprints(cfg *config.Config, nmiClients map[string]*nmi.NMIClient) *RuntimeFingerprints {
	return &RuntimeFingerprints{Config: cfg, NMI: nmiClients}
}

func (f *RuntimeFingerprints) AccountFingerprint(ctx context.Context, provider string) (string, bool) {
	if f == nil {
		return "", false
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	if c, ok := f.NMI[p]; ok && c != nil {
		return f.nmiFingerprint(ctx, p, c)
	}
	if f.Config != nil {
		if proc, ok := f.Config.Processors[p]; ok && proc != nil {
			isStripe := p == "stripe" || strings.EqualFold(proc.Type, "stripe")
			if key := strings.TrimSpace(proc.SecretKey); isStripe && key != "" {
				return f.stripeFingerprint(ctx, key)
			}
		}
	}
	return "", false
}

func (f *RuntimeFingerprints) nmiFingerprint(ctx context.Context, provider string, c *nmi.NMIClient) (string, bool) {
	f.mu.Lock()
	if e, ok := f.nmiCache[provider]; ok && e.securityKey == c.SecurityKey && e.fingerprint != "" {
		fp := e.fingerprint
		f.mu.Unlock()
		return fp, true
	}
	f.mu.Unlock()

	identity, err := c.AccountIdentity()
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("provider", provider).
			Warn("intents: nmi account fingerprint fetch failed; account guard skipped this pass")
		return "", false
	}

	f.mu.Lock()
	if f.nmiCache == nil {
		f.nmiCache = map[string]nmiIdentityEntry{}
	}
	f.nmiCache[provider] = nmiIdentityEntry{securityKey: c.SecurityKey, fingerprint: identity}
	f.mu.Unlock()
	return identity, true
}

func (f *RuntimeFingerprints) stripeFingerprint(ctx context.Context, key string) (string, bool) {
	f.mu.Lock()
	if f.stripeKey == key && f.stripeFP != "" {
		fp := f.stripeFP
		f.mu.Unlock()
		return fp, true
	}
	f.mu.Unlock()

	base := f.StripeBaseURL
	if base == "" {
		base = "https://api.stripe.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/account", nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := stripeapi.Client(f.Config, 0).Do(req)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("intents: stripe account fingerprint fetch failed; account guard skipped this pass")
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.WithContext(ctx).WithField("status", resp.StatusCode).Warn("intents: stripe account fingerprint fetch non-200; account guard skipped this pass")
		return "", false
	}
	var account struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&account); err != nil || account.ID == "" {
		log.WithContext(ctx).WithError(err).Warn("intents: stripe account fingerprint decode failed; account guard skipped this pass")
		return "", false
	}
	fp := "stripe:" + account.ID

	f.mu.Lock()
	f.stripeKey = key
	f.stripeFP = fp
	f.mu.Unlock()
	return fp, true
}

// accountMismatchReason formats the park/defer reason for a fingerprint
// mismatch. Exported shape is stable: it surfaces verbatim in
// last_failure_reason and the intents CLI.
func accountMismatchReason(stamped, current string) string {
	return fmt.Sprintf("provider account changed since enqueue (intent %s vs current %s); refingerprint after confirming, or restore the original credentials", stamped, current)
}
