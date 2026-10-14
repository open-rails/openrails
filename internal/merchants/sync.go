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
)

// SyncConfig reconciles a freshly loaded configuration with the identities
// Postgres records, the cache's SyncFunc: it registers each document's PSP and
// custodian identity, refuses changes to an existing PSP's identity, releases
// an archived PSP's fingerprint and fingerprints the rest. A document it
// cannot serve moves into Rejected and its previously served version keeps
// serving. Unchanged Vault versions skip the work.
func (s *Service) SyncConfig(ctx context.Context, id billing.MerchantID, previous, loaded merchantdocs.Set) (merchantdocs.Set, error) {
	if s == nil || s.pool == nil {
		return loaded, nil
	}
	if versioned(previous) && maps.Equal(previous.Revisions(), loaded.Revisions()) {
		loaded.Rejected, loaded.Undeclared = previous.Rejected, previous.Undeclared
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
		if err := s.syncCardVault(ctx, q, id, previous, set, reject); err != nil {
			return err
		}
		rows, err := q.ListPSPsForMerchant(ctx, id.UUID())
		if err != nil {
			return err
		}
		var declared []DeclaredCredential
		candidates := map[pspIdentity][]string{}
		for _, key := range set.PSPKeys() {
			p := set.PSPs[key].Value
			if _, refused := set.Rejected[merchantdocs.PSPDoc(key)]; refused {
				continue
			}
			if err := validatePSPDocument(key, p, set); err != nil {
				reject(merchantdocs.PSPDoc(key), err.Error())
				continue
			}
			if why := s.environmentRefusal(key, p.Rail, p.Environment, set.Merchant.Value.Sandbox); why != "" {
				reject(merchantdocs.PSPDoc(key), why)
				continue
			}
			// Refused before any label moves: an account another merchant
			// owns never costs this merchant's PSP its label.
			if err := AssertPSPUnowned(ctx, q, id.UUID(), p.Rail, p.Environment, p.AccountID); errors.Is(err, ErrPSPOwnedByAnotherMerchant) {
				reject(merchantdocs.PSPDoc(key), "the account belongs to another merchant's PSP")
				continue
			} else if err != nil {
				return err
			}
			candidates[docIdentity(p)] = append(candidates[docIdentity(p)], key)
		}
		labels := oneLabelPerAccount(rows, candidates, reject)
		accepted := slices.Sorted(maps.Values(labels))
		if err := moveLabels(ctx, q, id, rows, labels); err != nil {
			return err
		}
		for _, key := range accepted {
			p := set.PSPs[key].Value
			row, why, err := s.registerPSP(ctx, q, id, key, p)
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
		if set.Undeclared, err = undeclaredBilling(ctx, q, id, rows, labels); err != nil {
			return err
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

// pspIdentity is what a PSP is: its rail, money class and account. A label
// (its key) is only how the merchant's configuration names it.
type pspIdentity struct{ rail, class, account string }

func docIdentity(p merchantdocs.PSP) pspIdentity {
	return pspIdentity{normalizeProviderSecretType(p.Rail), config.EnvironmentClass(normalizeProviderSecretType(p.Rail), p.Environment), strings.TrimSpace(p.AccountID)}
}

func rowIdentity(row gen.BillingPsp) pspIdentity {
	return pspIdentity{row.Rail, config.EnvironmentClass(row.Rail, row.Environment), row.AccountID}
}

// oneLabelPerAccount keeps one label for each declared account: the label
// already holding it, else the first. Every other label declaring it is
// refused: an account is one PSP.
func oneLabelPerAccount(rows []gen.BillingPsp, candidates map[pspIdentity][]string, reject func(path, why string)) map[pspIdentity]string {
	held := map[pspIdentity]string{}
	for _, row := range rows {
		if row.SupersededAt == nil {
			held[rowIdentity(row)] = strings.ToLower(row.Key)
		}
	}
	labels := map[pspIdentity]string{}
	for identity, keys := range candidates {
		winner := keys[0]
		for _, key := range keys {
			if strings.EqualFold(key, held[identity]) {
				winner = key
			}
		}
		labels[identity] = winner
		for _, key := range keys {
			if key != winner {
				reject(merchantdocs.PSPDoc(key), fmt.Sprintf("%s account %s is already declared as psps/%s; an account is one PSP", identity.rail, identity.account, winner))
			}
		}
	}
	return labels
}

// moveLabels frees each label the documents now give another account. A
// current PSP its documents declare under another label steps aside for it;
// one no document declares any more is superseded: it keeps every record and
// has no credentials until a document declares its account again.
func moveLabels(ctx context.Context, q *gen.Queries, id billing.MerchantID, rows []gen.BillingPsp, labels map[pspIdentity]string) error {
	taken := map[string]pspIdentity{}
	for identity, key := range labels {
		taken[strings.ToLower(key)] = identity
	}
	for _, row := range rows {
		if row.SupersededAt != nil {
			continue
		}
		if key, ok := labels[rowIdentity(row)]; ok {
			if !strings.EqualFold(row.Key, key) {
				if _, err := q.RegisterPSP(ctx, gen.RegisterPSPParams{ID: row.ID, MerchantID: id.UUID(), Key: "~" + row.ID.String(), Rail: row.Rail, Environment: row.Environment, AccountID: row.AccountID}); err != nil {
					return err
				}
			}
			continue
		}
		next, ok := taken[strings.ToLower(row.Key)]
		if !ok {
			continue
		}
		if err := q.SupersedePSP(ctx, gen.SupersedePSPParams{MerchantID: id.UUID(), ID: row.ID}); err != nil {
			return err
		}
		log.WithFields(log.Fields{"merchant_id": id.String(), "psp": row.Key, "rail": row.Rail, "account_id": row.AccountID, "declared_account_id": next.account}).
			Warn("merchant config: a PSP's label names another account; the account it named keeps its records and has no declared credentials until a document declares it again")
	}
	return nil
}

// undeclaredBilling names each account of the merchant no document declares
// that still holds what needs its own credentials: cards in its vault and
// subscriptions renewing on it. Only historical payments on it is ordinary: a
// refund of one fails on its own with the reason.
func undeclaredBilling(ctx context.Context, q *gen.Queries, id billing.MerchantID, rows []gen.BillingPsp, labels map[pspIdentity]string) (map[string]string, error) {
	var out map[string]string
	for _, row := range rows {
		if _, declared := labels[rowIdentity(row)]; declared {
			continue
		}
		held, err := q.PSPHeldBilling(ctx, gen.PSPHeldBillingParams{MerchantID: id.UUID(), PspID: row.ID})
		if err != nil {
			return nil, err
		}
		if held.VaultedMethods+held.ProviderSubscriptions+held.EngineSubscriptions == 0 {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[merchantdocs.UndeclaredAccount(row.Rail, row.AccountID)] = undeclaredHolding(row, held)
	}
	return out, nil
}

// undeclaredHolding says what an undeclared account holds and how to fix it.
func undeclaredHolding(row gen.BillingPsp, held gen.PSPHeldBillingRow) string {
	why := fmt.Sprintf("%s account %s has no declared credentials but holds %d cards in its vault, %d subscriptions it bills itself and %d subscriptions OpenRails renews on it; declare the account's credentials under any PSP key",
		row.Rail, row.AccountID, held.VaultedMethods, held.ProviderSubscriptions, held.EngineSubscriptions)
	if held.ProviderSubscriptions > 0 {
		why += ". The provider keeps charging the subscriptions it bills, but their renewals are not recorded and cancels and refunds cannot reach it"
	}
	return why
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

// registerPSP records a PSP document's identity, current under its key.
func (s *Service) registerPSP(ctx context.Context, q *gen.Queries, id billing.MerchantID, key string, p merchantdocs.PSP) (gen.BillingPsp, string, error) {
	pspID, rail, environment, account := PSPNaturalKey(p.Rail, p.Environment, p.AccountID)
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
	if _, err := config.NormalizePSPDeclaration(key, config.PSPConfig{Rail: billing.Rail(rail), Environment: p.Environment, AccountID: p.AccountID,
		Custodian: p.Custodian, Settings: p.Settings, Secrets: p.Secrets}); err != nil {
		return err
	}
	if err := config.RejectRetiredCustodySettings(p.Settings); err != nil {
		return err
	}
	custodian := strings.ToLower(strings.TrimSpace(p.Custodian))
	if _, err := config.CardEntry(rail, p.Settings, custodian != ""); err != nil {
		return err
	}
	if rail == string(models.RailStripe) {
		if err := config.ValidateStripeSecretKeyEnvironment(p.Environment, p.Secrets["secret_key"]); err != nil {
			return err
		}
		if raw, ok := p.Settings["publishable_key"]; ok {
			value, _ := raw.(string)
			if err := config.ValidateStripePublishableKeyEnvironment(p.Environment, value); err != nil {
				return fmt.Errorf("publishable_key: %w", err)
			}
		}
	}
	if custodian != "" {
		held, ok := set.Custodians[custodian]
		if !ok || held.Value.Environment != config.EnvironmentClass(p.Rail, p.Environment) {
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
	if normalizeCustodianEnvironment(c.Environment) != c.Environment {
		return fmt.Errorf("environment must be live or test")
	}
	return config.ValidateCustodianEntry(config.CustodianEntry{
		Key: key, Kind: custodians.Normalize(c.Kind), AccountID: strings.TrimSpace(c.AccountID),
		Settings: c.Settings, Archived: c.Archived, SecretKeys: slices.Sorted(maps.Keys(c.Secrets)),
	})
}
