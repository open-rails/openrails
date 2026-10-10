package merchants

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchantdocs"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// SyncConfig reconciles a freshly loaded configuration with the identities
// Postgres records, the cache's SyncFunc: it registers each document's PSP and
// custodian identity, supersedes an identity its key no longer names, releases
// an archived PSP's fingerprint and fingerprints the rest. A document it
// cannot serve moves into Rejected and its previously served version keeps
// serving. Unchanged Vault versions skip the work.
func (s *Service) SyncConfig(ctx context.Context, id billing.MerchantID, previous, loaded merchantdocs.Set) (merchantdocs.Set, error) {
	if s == nil || s.pool == nil {
		return loaded, nil
	}
	if versioned(previous) && maps.Equal(previous.Revisions(), loaded.Revisions()) {
		loaded.Rejected = previous.Rejected
		return keepServed(previous, loaded), nil
	}
	set := loaded.Clone()
	if set.Rejected == nil {
		set.Rejected = map[string]string{}
	}
	reject := func(path, why string) { set.Rejected[path] = why }
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		for _, key := range set.CustodianKeys() {
			c := set.Custodians[key].Value
			if c.Environment != s.providerEnvironment {
				continue
			}
			if err := validateCustodianDocument(key, c); err != nil {
				reject(merchantdocs.CustodianDoc(key), err.Error())
				continue
			}
			if why, err := registerCustodian(ctx, q, id, key, c); err != nil {
				return err
			} else if why != "" {
				reject(merchantdocs.CustodianDoc(key), why)
			}
		}
		rows, err := q.ListPSPsForMerchant(ctx, id.UUID())
		if err != nil {
			return err
		}
		var declared []DeclaredCredential
		for _, key := range set.PSPKeys() {
			p := set.PSPs[key].Value
			if p.Environment != s.providerEnvironment {
				continue
			}
			if err := validatePSPDocument(key, p, set); err != nil {
				reject(merchantdocs.PSPDoc(key), err.Error())
				continue
			}
			row, why, err := s.registerPSP(ctx, q, id, key, p, rows)
			if err != nil {
				return err
			}
			if why != "" {
				reject(merchantdocs.PSPDoc(key), why)
				continue
			}
			if err := s.fingerprint(ctx, tx, id, row, p, previous); err != nil {
				return err
			}
			if s.fingerprints == nil && !p.Archived {
				declared = append(declared, DeclaredCredential{PSPID: row.ID, Rail: p.Rail, Credential: strings.TrimSpace(p.Secrets[AccountCredentialKey(p.Rail)])})
			}
		}
		if s.fingerprints == nil {
			sort.SliceStable(declared, func(a, b int) bool { return declared[a].PSPID.String() < declared[b].PSPID.String() })
			return ReconcileDeclaredDuplicates(ctx, q, id.UUID(), declared, s.now())
		}
		return nil
	})
	if err != nil {
		return previous, fmt.Errorf("merchants: reconcile configuration: %w", err)
	}
	if len(set.Rejected) == 0 {
		set.Rejected = nil
	}
	return keepServed(previous, set), nil
}

// versioned reports a set whose every document carries a Vault version: a
// file's documents carry none, so an unchanged revision says nothing there.
func versioned(set merchantdocs.Set) bool {
	revisions := set.Revisions()
	if len(revisions) == 0 {
		return false
	}
	for _, revision := range revisions {
		if revision <= 0 {
			return false
		}
	}
	return true
}

// keepServed puts back the previously served version of each rejected
// document.
func keepServed(previous, set merchantdocs.Set) merchantdocs.Set {
	for path := range set.Rejected {
		psp, custodian := merchantdocs.ParseDoc(path)
		switch {
		case path == merchantdocs.MerchantDoc:
			set.Merchant, set.HasMerchant = previous.Merchant, previous.HasMerchant
		case psp != "":
			if doc, ok := previous.PSPs[psp]; ok {
				set.PSPs[psp] = doc
			} else {
				delete(set.PSPs, psp)
			}
		case custodian != "":
			if doc, ok := previous.Custodians[custodian]; ok {
				set.Custodians[custodian] = doc
			} else {
				delete(set.Custodians, custodian)
			}
		}
	}
	return set
}

// registerCustodian records a custodian document's identity; why says why it
// cannot be served.
func registerCustodian(ctx context.Context, q *gen.Queries, id billing.MerchantID, key string, c merchantdocs.Custodian) (string, error) {
	_, err := q.RegisterCustodian(ctx, gen.RegisterCustodianParams{
		MerchantID: id.UUID(), Key: key, Kind: custodians.Normalize(c.Kind), Environment: c.Environment, AccountID: strings.TrimSpace(c.AccountID),
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "the custodian account belongs to another merchant", nil
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return "another custodian account of this merchant holds the key", nil
	}
	return "", err
}

// registerPSP records a PSP document's identity, current under its key. An
// identity the key named before is superseded and drains.
func (s *Service) registerPSP(ctx context.Context, q *gen.Queries, id billing.MerchantID, key string, p merchantdocs.PSP, rows []gen.BillingPsp) (gen.BillingPsp, string, error) {
	pspID, rail, environment, account := PSPNaturalKey(p.Rail, p.Environment, p.AccountID)
	for _, row := range rows {
		if row.SupersededAt == nil && strings.EqualFold(row.Key, key) && row.ID != pspID {
			log.WithFields(log.Fields{"merchant_id": id.String(), "psp": key, "superseded": row.AccountID, "current": account}).
				Warn("merchant config: a PSP document names another account; the old identity drains")
			if err := q.SupersedePSP(ctx, gen.SupersedePSPParams{MerchantID: id.UUID(), ID: row.ID}); err != nil {
				return gen.BillingPsp{}, "", err
			}
		}
	}
	row, err := q.RegisterPSP(ctx, gen.RegisterPSPParams{ID: pspID, MerchantID: id.UUID(), Key: key, Rail: rail, Environment: environment, AccountID: account})
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return gen.BillingPsp{}, "the account belongs to another merchant's PSP", nil
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return gen.BillingPsp{}, "another current PSP of this merchant holds the key", nil
	case err != nil:
		return gen.BillingPsp{}, "", err
	}
	return row, "", nil
}

// fingerprint keeps the PSP's account-credential fingerprint current: none
// while archived, recomputed when the credential changed.
func (s *Service) fingerprint(ctx context.Context, tx pgx.Tx, id billing.MerchantID, row gen.BillingPsp, p merchantdocs.PSP, previous merchantdocs.Set) error {
	q := gen.New(tx)
	accountKey := AccountCredentialKey(p.Rail)
	if accountKey == "" {
		return nil
	}
	if p.Archived {
		return q.ClearPSPCredentialFingerprint(ctx, gen.ClearPSPCredentialFingerprintParams{MerchantID: id.UUID(), ID: row.ID})
	}
	if s.fingerprints == nil {
		return nil
	}
	credential := strings.TrimSpace(p.Secrets[accountKey])
	if credential == "" {
		return nil
	}
	before := ""
	for _, doc := range previous.PSPs {
		if doc.Value.Rail == p.Rail && doc.Value.Environment == p.Environment && doc.Value.AccountID == p.AccountID {
			before = strings.TrimSpace(doc.Value.Secrets[accountKey])
		}
	}
	if before == credential && row.CredentialFingerprint != nil && row.CredentialDuplicateAt == nil {
		return nil
	}
	_, err := recordCredentialFingerprint(ctx, tx, id.UUID(), row.ID, s.fingerprints.Fingerprint(p.Rail, p.Environment, credential), s.now())
	return err
}

// validatePSPDocument checks a PSP document as the file parser checks a
// declaration.
func validatePSPDocument(key string, p merchantdocs.PSP, set merchantdocs.Set) error {
	rail := normalizeProviderSecretType(p.Rail)
	if err := config.ValidatePSPKeys(key, config.PSPConfig{Rail: billing.Rail(rail), Settings: p.Settings, Secrets: p.Secrets}); err != nil {
		return err
	}
	if normalizeProviderSecretEnvironment(p.Environment) != p.Environment {
		return fmt.Errorf("environment must be live or test")
	}
	account := strings.TrimSpace(p.AccountID)
	if account == "" {
		return fmt.Errorf("account_id is required")
	}
	if err := config.ValidateRailAccountID(models.Rail(rail), account); err != nil {
		return err
	}
	if rail == string(models.RailSolana) {
		if err := config.ValidateSolanaAccountSettings(p.Settings); err != nil {
			return err
		}
		settings, err := config.ParseSolanaAccountSettings(p.Settings)
		if err != nil {
			return err
		}
		network := "mainnet"
		if p.Environment == config.ProviderEnvironmentTest {
			network = "devnet"
		}
		if _, err := solanatokens.ResolveDeclared(network, settings.Tokens); err != nil {
			return err
		}
	} else if p.Signer != nil {
		return fmt.Errorf("a signer is only for solana")
	}
	if err := config.RejectRetiredCustodySettings(p.Settings); err != nil {
		return err
	}
	custodian := strings.ToLower(strings.TrimSpace(p.Custodian))
	if _, err := config.CardEntry(rail, p.Settings, custodian != ""); err != nil {
		return err
	}
	if _, err := config.NMIEndpointDeployment(p.Settings); rail == string(models.RailNMI) && err != nil {
		return err
	}
	if custodian != "" {
		held, ok := set.Custodians[custodian]
		if !ok || held.Value.Environment != p.Environment {
			return fmt.Errorf("custodian %q is not declared (declared: %s)", custodian, strings.Join(set.CustodianKeys(), ", "))
		}
		d, err := custodians.Require(held.Value.Kind)
		if err != nil {
			return err
		}
		if !d.SupportsRail(models.Rail(rail)) {
			return fmt.Errorf("custodian %q (%s) can only be charged through %s", custodian, d.Kind, d.RailNames())
		}
	}
	return nil
}

// validateCustodianDocument checks a custodian document as the file parser
// checks a declaration.
func validateCustodianDocument(key string, c merchantdocs.Custodian) error {
	if normalizeProviderSecretEnvironment(c.Environment) != c.Environment {
		return fmt.Errorf("environment must be live or test")
	}
	return config.ValidateCustodianEntry(config.CustodianEntry{
		Key: key, Kind: custodians.Normalize(c.Kind), AccountID: strings.TrimSpace(c.AccountID),
		Settings: c.Settings, Archived: c.Archived, SecretKeys: slices.Sorted(maps.Keys(c.Secrets)),
	})
}
