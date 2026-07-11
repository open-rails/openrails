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

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// secretStoreGetter adapts the per-merchant merchants.MerchantSecretStore to the
// solana.MerchantSecretGetter the keypair signer needs.
type secretStoreGetter struct {
	store       merchants.MerchantSecretReader
	database    *db.DB
	environment string
	accountID   string
}

func (g secretStoreGetter) GetSecret(ctx context.Context, merchantID merchant.ID, name string) (string, error) {
	if name == "private_key" && g.database != nil {
		var (
			account gen.OpenrailsPsp
			ok      bool
			err     error
		)
		if strings.TrimSpace(g.accountID) != "" {
			account, ok, err = solanaRailMerchantAccountByIdentity(ctx, g.database, merchantID, g.environment, g.accountID)
		} else {
			account, ok, err = primarySolanaRailMerchantAccount(ctx, g.database, merchantID, g.environment)
		}
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("solana: no active provider account for signing key")
		}
		secretName, err := merchants.PSPSecretName(account.Rail, account.Environment, account.AccountID, "private_key")
		if err != nil {
			return "", err
		}
		sec, err := g.store.Get(ctx, merchantID, secretName)
		if err != nil {
			return "", err
		}
		return sec.Value, nil
	}
	sec, err := g.store.Get(ctx, merchantID, name)
	if err != nil {
		return "", err
	}
	return sec.Value, nil
}

// NewSignerFromRailMerchantAccounts builds the per-merchant signer. environment
// is required (#681): deployment posture, via config.ExpectedProviderEnvironment.
func NewSignerFromRailMerchantAccounts(store merchants.MerchantSecretReader, transit solanaint.TransitClient, database *db.DB, ttl time.Duration, environment string) solanaint.Signer {
	env := strings.TrimSpace(environment)
	return railMerchantAccountSigner{
		keypair:     solanaint.NewKeypairSigner(secretStoreGetter{store: store, database: database, environment: env}, ttl),
		store:       store,
		transit:     transit,
		db:          database,
		environment: env,
		ttl:         ttl,
	}
}

type railMerchantAccountSigner struct {
	keypair     solanaint.Signer
	store       merchants.MerchantSecretReader
	transit     solanaint.TransitClient
	db          *db.DB
	environment string
	ttl         time.Duration
}

func (s railMerchantAccountSigner) PublicKey(ctx context.Context, merchantID merchant.ID) (solanago.PublicKey, error) {
	signer, err := s.resolve(ctx, merchantID)
	if err != nil {
		return solanago.PublicKey{}, err
	}
	return signer.PublicKey(ctx, merchantID)
}

func (s railMerchantAccountSigner) SignMessage(ctx context.Context, merchantID merchant.ID, message []byte) (solanago.Signature, error) {
	signer, err := s.resolve(ctx, merchantID)
	if err != nil {
		return solanago.Signature{}, err
	}
	return signer.SignMessage(ctx, merchantID, message)
}

func (s railMerchantAccountSigner) SignMessageForPublicKey(ctx context.Context, merchantID merchant.ID, publicKey solanago.PublicKey, message []byte) (solanago.Signature, error) {
	signer, err := s.resolveForPublicKey(ctx, merchantID, publicKey.String())
	if err != nil {
		return solanago.Signature{}, err
	}
	return signer.SignMessage(ctx, merchantID, message)
}

func (s railMerchantAccountSigner) resolve(ctx context.Context, merchantID merchant.ID) (solanaint.Signer, error) {
	account, ok, err := primarySolanaRailMerchantAccount(ctx, s.db, merchantID, s.environment)
	if err != nil {
		return nil, err
	}
	if ok {
		cfg := signerConfigFromEvidence(account.Evidence)
		switch cfg.Mode {
		case "local_keypair":
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
	return nil, fmt.Errorf("solana: no active provider account for signer")
}

func (s railMerchantAccountSigner) resolveForPublicKey(ctx context.Context, merchantID merchant.ID, publicKey string) (solanaint.Signer, error) {
	account, ok, err := solanaRailMerchantAccountByIdentity(ctx, s.db, merchantID, s.environment, publicKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("solana: no provider account for merchant address %s", publicKey)
	}
	cfg := signerConfigFromEvidence(account.Evidence)
	switch cfg.Mode {
	case "local_keypair":
		return solanaint.NewKeypairSigner(secretStoreGetter{
			store:       s.store,
			database:    s.db,
			environment: account.Environment,
			accountID:   account.AccountID,
		}, s.ttl), nil
	case "vault_transit":
		if s.transit == nil {
			return nil, fmt.Errorf("solana: provider account %s uses vault_transit signer but Vault transit is unavailable", account.AccountID)
		}
		return solanaint.NewTransitSigner(s.transit, func(merchant.ID) string { return cfg.Key }, s.ttl), nil
	default:
		return nil, fmt.Errorf("solana: unknown signer mode %q", cfg.Mode)
	}
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

func primarySolanaRailMerchantAccount(ctx context.Context, database *db.DB, merchantID merchant.ID, environment string) (gen.OpenrailsPsp, bool, error) {
	if database == nil || merchantID.IsZero() {
		return gen.OpenrailsPsp{}, false, nil
	}
	environment = strings.TrimSpace(environment)
	if environment == "" {
		return gen.OpenrailsPsp{}, false, fmt.Errorf("solana: provider account environment is required")
	}
	var row gen.OpenrailsPsp
	if err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		var err error
		row, err = database.Gen(ctx).GetActivePSPForNewWork(ctx, gen.GetActivePSPForNewWorkParams{
			MerchantID:  merchantID.UUID(),
			Rail:        "solana",
			Environment: &environment,
		})
		return err
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return gen.OpenrailsPsp{}, false, nil
		}
		return gen.OpenrailsPsp{}, false, fmt.Errorf("solana: lookup active provider account: %w", err)
	}
	return row, true, nil
}

func solanaRailMerchantAccountByIdentity(ctx context.Context, database *db.DB, merchantID merchant.ID, environment, accountID string) (gen.OpenrailsPsp, bool, error) {
	if database == nil || merchantID.IsZero() || strings.TrimSpace(accountID) == "" {
		return gen.OpenrailsPsp{}, false, nil
	}
	environment = strings.TrimSpace(environment)
	if environment == "" {
		return gen.OpenrailsPsp{}, false, fmt.Errorf("solana: provider account environment is required")
	}
	var row gen.OpenrailsPsp
	if err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		var err error
		row, err = database.Gen(ctx).GetPSPByIdentity(ctx, gen.GetPSPByIdentityParams{
			MerchantID:  merchantID.UUID(),
			Rail:        "solana",
			Environment: &environment,
			AccountID:   strings.TrimSpace(accountID),
		})
		return err
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return gen.OpenrailsPsp{}, false, nil
		}
		return gen.OpenrailsPsp{}, false, fmt.Errorf("solana: lookup provider account %s: %w", accountID, err)
	}
	return row, true, nil
}
