package recurring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchants"
)

// secretStoreGetter adapts the per-merchant merchants.MerchantSecretReader to the
// solana.MerchantSecretGetter the keypair signer needs.
type secretStoreGetter struct {
	store       merchants.MerchantSecretReader
	database    *db.DB
	environment string
	accountID   string
}

func (g secretStoreGetter) GetSecret(ctx context.Context, merchantID billing.MerchantID, name string) (string, error) {
	if name == "private_key" && g.database != nil {
		var (
			account merchants.PSPScope
			ok      bool
			err     error
		)
		if strings.TrimSpace(g.accountID) != "" {
			account, ok, err = solanaPSPByIdentity(ctx, g.database, merchantID, g.environment, g.accountID)
		} else {
			account, ok, err = primarySolanaPSP(ctx, g.database, merchantID, g.environment)
		}
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("solana: no active PSP for signing key")
		}
		ref, err := account.SecretRef("private_key")
		if err != nil {
			return "", err
		}
		sec, err := merchants.ReadSecretRef(ctx, g.store, merchantID, ref)
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

// NewSignerFromPSPs builds the per-merchant signer. environment
// is required (#681): deployment posture, via config.ExpectedProviderEnvironment.
func NewSignerFromPSPs(store merchants.MerchantSecretReader, transit solanaint.TransitClient, database *db.DB, ttl time.Duration, environment string) solanaint.Signer {
	env := strings.TrimSpace(environment)
	return pspSigner{
		keypair:     solanaint.NewKeypairSigner(secretStoreGetter{store: store, database: database, environment: env}, ttl),
		store:       store,
		transit:     transit,
		db:          database,
		environment: env,
		ttl:         ttl,
	}
}

type pspSigner struct {
	keypair     solanaint.Signer
	store       merchants.MerchantSecretReader
	transit     solanaint.TransitClient
	db          *db.DB
	environment string
	ttl         time.Duration
}

func (s pspSigner) PublicKey(ctx context.Context, merchantID billing.MerchantID) (solanago.PublicKey, error) {
	signer, err := s.resolve(ctx, merchantID)
	if err != nil {
		return solanago.PublicKey{}, err
	}
	return signer.PublicKey(ctx, merchantID)
}

func (s pspSigner) SignMessage(ctx context.Context, merchantID billing.MerchantID, message []byte) (solanago.Signature, error) {
	signer, err := s.resolve(ctx, merchantID)
	if err != nil {
		return solanago.Signature{}, err
	}
	return signer.SignMessage(ctx, merchantID, message)
}

func (s pspSigner) SignMessageForPublicKey(ctx context.Context, merchantID billing.MerchantID, publicKey solanago.PublicKey, message []byte) (solanago.Signature, error) {
	signer, err := s.resolveForPublicKey(ctx, merchantID, publicKey.String())
	if err != nil {
		return solanago.Signature{}, err
	}
	return signer.SignMessage(ctx, merchantID, message)
}

func (s pspSigner) resolve(ctx context.Context, merchantID billing.MerchantID) (solanaint.Signer, error) {
	account, ok, err := primarySolanaPSP(ctx, s.db, merchantID, s.environment)
	if err != nil {
		return nil, err
	}
	if ok {
		if err := signerApproved(account); err != nil {
			return nil, err
		}
		cfg := signerConfigFromRow(account)
		switch cfg.Mode {
		case "local_keypair":
			return s.keypair, nil
		case "vault_transit":
			if s.transit == nil {
				return nil, fmt.Errorf("solana: PSP %s uses vault_transit signer but Vault transit is unavailable", account.AccountID)
			}
			return solanaint.NewTransitSigner(s.transit, func(billing.MerchantID) string { return cfg.Key }, s.ttl), nil
		default:
			return nil, fmt.Errorf("solana: unknown signer mode %q", cfg.Mode)
		}
	}
	return nil, fmt.Errorf("solana: no active PSP for signer")
}

func (s pspSigner) resolveForPublicKey(ctx context.Context, merchantID billing.MerchantID, publicKey string) (solanaint.Signer, error) {
	account, ok, err := solanaPSPByIdentity(ctx, s.db, merchantID, s.environment, publicKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("solana: no PSP for merchant address %s", publicKey)
	}
	if err := signerApproved(account); err != nil {
		return nil, err
	}
	cfg := signerConfigFromRow(account)
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
			return nil, fmt.Errorf("solana: PSP %s uses vault_transit signer but Vault transit is unavailable", account.AccountID)
		}
		return solanaint.NewTransitSigner(s.transit, func(billing.MerchantID) string { return cfg.Key }, s.ttl), nil
	default:
		return nil, fmt.Errorf("solana: unknown signer mode %q", cfg.Mode)
	}
}

// signerApproved refuses a PSP whose Transit signer reports an unapproved
// identity (#1101): nothing is signed for, or paid to, it until an operator
// approves the change.
func signerApproved(account merchants.PSPScope) error {
	if account.SignerChange != "" {
		return fmt.Errorf("solana: PSP %s signer now reports %s: %w", account.AccountID, account.SignerChange, vault.ErrSignerUnapproved)
	}
	return nil
}

type solanaSignerConfig struct {
	Mode string
	Key  string
}

// signerConfigFromRow is the PSP's declared signer; none is a local keypair.
func signerConfigFromRow(account merchants.PSPScope) solanaSignerConfig {
	signer := solanaSignerConfig{Mode: "local_keypair"}
	if account.Signer != nil && account.Signer.Mode != "" {
		signer.Mode = strings.ToLower(strings.TrimSpace(account.Signer.Mode))
		signer.Key = strings.TrimSpace(account.Signer.Key)
	}
	return signer
}

func primarySolanaPSP(ctx context.Context, database *db.DB, merchantID billing.MerchantID, environment string) (merchants.PSPScope, bool, error) {
	configuration := merchants.Of(database)
	if configuration == nil || merchantID.IsZero() {
		return merchants.PSPScope{}, false, nil
	}
	environment = strings.TrimSpace(environment)
	if environment == "" {
		return merchants.PSPScope{}, false, fmt.Errorf("solana: PSP environment is required")
	}
	scope, ok, err := configuration.ActivePSPScope(ctx, merchantID, "solana", environment)
	if errors.Is(err, merchants.ErrNoActivePSP) {
		return merchants.PSPScope{}, false, nil
	}
	if err != nil {
		return merchants.PSPScope{}, false, fmt.Errorf("solana: lookup active PSP: %w", err)
	}
	return scope, ok, nil
}

func solanaPSPByIdentity(ctx context.Context, database *db.DB, merchantID billing.MerchantID, environment, accountID string) (merchants.PSPScope, bool, error) {
	configuration := merchants.Of(database)
	if configuration == nil || merchantID.IsZero() || strings.TrimSpace(accountID) == "" {
		return merchants.PSPScope{}, false, nil
	}
	environment = strings.TrimSpace(environment)
	if environment == "" {
		return merchants.PSPScope{}, false, fmt.Errorf("solana: PSP environment is required")
	}
	scope, ok, err := configuration.PSPScopeByAccountID(ctx, merchantID, "solana", strings.TrimSpace(accountID))
	if err != nil {
		return merchants.PSPScope{}, false, fmt.Errorf("solana: lookup PSP %s: %w", accountID, err)
	}
	if !ok || scope.Environment != environment {
		return merchants.PSPScope{}, false, nil
	}
	return scope, true, nil
}
