package recurring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SeedConfiguredMerchantSolanaSecret bridges a single-merchant install that
// configures its Solana signing key via global config into the per-merchant
// secret store the recurring Solana services read (issue #253). The
// Submitter/signer always reads solana/private_key from the merchant secret store;
// a global-config-only install would otherwise have an EMPTY store for its
// merchant and could not sign without a manual secret write.
//
// merchantID is the engine's configured merchant (#336 — there is no default
// merchant). It is idempotent and safe to call on every boot:
//
//   - merchantID zero        -> no-op (no configured merchant to seed for).
//   - configKey empty        -> no-op (Solana not configured via global config).
//   - secret already present -> no-op (NEVER overwrites a manual/rotated key).
//   - secret absent          -> seed it from configKey and log once.
//
// A backend error (store unavailable) is returned so the caller can fail boot
// loudly rather than start a signer that will never resolve a key.
func SeedConfiguredMerchantSolanaSecret(ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, configKey string) error {
	if merchantID.IsZero() {
		return nil
	}
	return SeedMerchantSolanaSecret(ctx, store, merchantID, configKey)
}

func SeedMerchantSolanaSecret(ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, configKey string) error {
	key := strings.TrimSpace(configKey)
	if key == "" {
		return nil // nothing configured via global config
	}
	if store == nil {
		return fmt.Errorf("recurring: seed merchant solana secret: nil secret store")
	}
	_, err := store.Get(ctx, merchantID, solanaint.SecretSolanaPrivateKey)
	switch {
	case err == nil:
		return nil // already present — never overwrite an existing secret
	case errors.Is(err, merchants.ErrSecretNotFound):
		// fall through to seed
	default:
		return fmt.Errorf("recurring: check merchant solana secret: %w", err)
	}
	if _, err := store.Put(ctx, merchantID, solanaint.SecretSolanaPrivateKey, key); err != nil {
		return fmt.Errorf("recurring: seed merchant solana secret: %w", err)
	}
	log.WithField("merchant", merchantID.String()).
		Info("seeded merchant Solana signing key from global config (solana/private_key)")
	return nil
}

// secretStoreGetter adapts the per-merchant merchants.MerchantSecretStore to the
// solana.MerchantSecretGetter the keypair signer needs. The Solana signing key is
// the secret named solana/private_key; the store backend (DB+envelope or Vault)
// is chosen at the composition root and is transparent here.
type secretStoreGetter struct {
	store    merchants.MerchantSecretReader
	database *db.DB
}

func (g secretStoreGetter) GetSecret(ctx context.Context, merchantID merchant.ID, name string) (string, error) {
	if name == solanaint.SecretSolanaPrivateKey && g.database != nil {
		if account, ok, err := primarySolanaProviderAccount(ctx, g.database, merchantID); err != nil {
			return "", err
		} else if ok {
			secretName, err := merchants.ProviderAccountSecretName(account.Rail, account.Environment, account.AccountID, "private_key")
			if err != nil {
				return "", err
			}
			sec, err := g.store.Get(ctx, merchantID, secretName)
			switch {
			case err == nil:
				return sec.Value, nil
			case !errors.Is(err, merchants.ErrSecretNotFound):
				return "", err
			}
		}
	}
	sec, err := g.store.Get(ctx, merchantID, name)
	if err != nil {
		return "", err
	}
	return sec.Value, nil
}

// NewSignerFromStore builds the per-merchant keypair signer reading
// solana/private_key from the merchant secret store. Exposed (alongside the
// Submitter constructors) so the composition root can share the SAME signer
// between the Submitter-backed services and the signer-backed
// PrepareTierChangeService (#272), which co-signs with the merchant key directly.
func NewSignerFromStore(store merchants.MerchantSecretReader, ttl time.Duration) solanaint.Signer {
	return solanaint.NewKeypairSigner(secretStoreGetter{store: store}, ttl)
}

func NewSignerFromProviderAccounts(store merchants.MerchantSecretReader, transit solanaint.TransitClient, database *db.DB, ttl time.Duration) solanaint.Signer {
	return providerAccountSigner{
		keypair: solanaint.NewKeypairSigner(secretStoreGetter{store: store, database: database}, ttl),
		transit: transit,
		db:      database,
		ttl:     ttl,
	}
}

// NewSignerFromTransit builds the per-merchant signer whose key lives in Vault
// Transit (non-extractable). Shared with PrepareTierChangeService (#272).
func NewSignerFromTransit(transit solanaint.TransitClient, ttl time.Duration) solanaint.Signer {
	return solanaint.NewTransitSigner(transit, nil, ttl)
}

// NewSignerSubmitterFromStore builds the production Submitter: a per-merchant
// keypair signer reading solana/private_key from the merchant secret store, wired
// to the Solana RPC. ttl 0 uses the default signer cache TTL.
func NewSignerSubmitterFromStore(store merchants.MerchantSecretReader, rpc *solanaint.RPCClient, ttl time.Duration) Submitter {
	return NewSignerSubmitter(NewSignerFromStore(store, ttl), rpc)
}

// NewCrankServiceFromStore builds a CrankService backed by the merchant secret
// store + RPC — the value the composition root injects into the cranker worker.
func NewCrankServiceFromStore(store merchants.MerchantSecretReader, rpc *solanaint.RPCClient, ttl time.Duration) *CrankService {
	return NewCrankService(NewSignerSubmitterFromStore(store, rpc, ttl))
}

// NewPlanServiceFromStore builds a PlanService backed by the merchant secret store
// + RPC for the given network (mainnet/devnet). The same RPC client serves as the
// plan reader, enabling the idempotent re-publish guard (#254).
func NewPlanServiceFromStore(store merchants.MerchantSecretReader, rpc *solanaint.RPCClient, network string, tokens map[string]config.TokenConfig, ttl time.Duration) *PlanService {
	return NewPlanServiceWithReader(NewSignerSubmitterFromStore(store, rpc, ttl), rpc, network, tokens)
}

// NewSignerSubmitterFromTransit builds a Submitter whose signing key lives in
// Vault Transit (non-extractable) — the key never enters this process.
func NewSignerSubmitterFromTransit(transit solanaint.TransitClient, rpc *solanaint.RPCClient, ttl time.Duration) Submitter {
	return NewSignerSubmitter(NewSignerFromTransit(transit, ttl), rpc)
}

// NewCrankServiceFromTransit builds a CrankService whose per-merchant Solana key is
// signed via Vault Transit.
func NewCrankServiceFromTransit(transit solanaint.TransitClient, rpc *solanaint.RPCClient, ttl time.Duration) *CrankService {
	return NewCrankService(NewSignerSubmitterFromTransit(transit, rpc, ttl))
}

type providerAccountSigner struct {
	keypair solanaint.Signer
	transit solanaint.TransitClient
	db      *db.DB
	ttl     time.Duration
}

func (s providerAccountSigner) PublicKey(ctx context.Context, merchantID merchant.ID) (solanago.PublicKey, error) {
	signer, err := s.resolve(ctx, merchantID)
	if err != nil {
		return solanago.PublicKey{}, err
	}
	return signer.PublicKey(ctx, merchantID)
}

func (s providerAccountSigner) SignMessage(ctx context.Context, merchantID merchant.ID, message []byte) (solanago.Signature, error) {
	signer, err := s.resolve(ctx, merchantID)
	if err != nil {
		return solanago.Signature{}, err
	}
	return signer.SignMessage(ctx, merchantID, message)
}

func (s providerAccountSigner) resolve(ctx context.Context, merchantID merchant.ID) (solanaint.Signer, error) {
	account, ok, err := primarySolanaProviderAccount(ctx, s.db, merchantID)
	if err != nil {
		return nil, err
	}
	if ok {
		cfg := signerConfigFromEvidence(account.Evidence)
		switch cfg.Mode {
		case "", "keypair":
			return s.keypair, nil
		case "vault_transit":
			if s.transit == nil {
				return nil, fmt.Errorf("solana: provider account %s uses vault_transit signer but Vault transit is unavailable", account.AccountID)
			}
			return solanaint.NewTransitSigner(s.transit, func(merchant.ID) string { return cfg.Key }, s.ttl), nil
		default:
			return nil, fmt.Errorf("solana: unknown signer mode %q", cfg.Mode)
		}
	}
	if s.transit != nil {
		return solanaint.NewTransitSigner(s.transit, nil, s.ttl), nil
	}
	return s.keypair, nil
}

type solanaSignerConfig struct {
	Mode string `json:"mode"`
	Key  string `json:"key"`
}

func signerConfigFromEvidence(raw []byte) solanaSignerConfig {
	var evidence struct {
		Signer solanaSignerConfig `json:"signer"`
	}
	_ = json.Unmarshal(raw, &evidence)
	evidence.Signer.Mode = strings.ToLower(strings.TrimSpace(evidence.Signer.Mode))
	evidence.Signer.Key = strings.TrimSpace(evidence.Signer.Key)
	return evidence.Signer
}

func primarySolanaProviderAccount(ctx context.Context, database *db.DB, merchantID merchant.ID) (gen.OpenrailsPaymentProviderAccount, bool, error) {
	if database == nil || merchantID.IsZero() {
		return gen.OpenrailsPaymentProviderAccount{}, false, nil
	}
	var row gen.OpenrailsPaymentProviderAccount
	if err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		var err error
		environment := "live"
		row, err = database.Gen(ctx).GetActiveProviderAccountForNewWork(ctx, gen.GetActiveProviderAccountForNewWorkParams{
			MerchantID:  merchantID.UUID(),
			Rail:        "solana",
			Environment: &environment,
		})
		return err
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return gen.OpenrailsPaymentProviderAccount{}, false, nil
		}
		return gen.OpenrailsPaymentProviderAccount{}, false, fmt.Errorf("solana: lookup active provider account: %w", err)
	}
	return row, true, nil
}
