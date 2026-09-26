// Package signeridentity keeps a Vault Transit Solana signer bound to the
// identity OpenRails stored for it. A Transit key that reports a different
// public key (a rotated key, or Vault reached at another address, namespace
// or mount) fails closed: the change is recorded on the stored PSP rows, the
// Solana rail refuses until an operator approves it, and the new identity is
// never provisioned before that. Embedded and standalone provisioning share it.
package signeridentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	solanago "github.com/gagliardetto/solana-go"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Transit checks every Transit public-key read made while provisioning one
// merchant against the identities stored for that key. With Tolerate, a read
// Vault cannot serve right now is answered from the stored identity.
type Transit struct {
	solanaint.TransitClient
	DB          *db.DB
	Directory   *merchants.Service
	Slug        string
	Environment string
	Tolerate    bool
	// OnChange hears a detected change (the host-facing probe).
	OnChange    func(error)
	unavailable atomic.Bool
}

// Unavailable reports whether a read fell back because Vault did not answer:
// the provisioning must be re-run once it does.
func (t *Transit) Unavailable() bool { return t.unavailable.Load() }

// Defers is the MerchantManifestReconcileOptions.DeferPSP policy: a PSP whose
// signer awaits approval, or (tolerating) whose Vault is down, is skipped.
func (t *Transit) Defers(_ string, err error) bool {
	return errors.Is(err, vault.ErrSignerUnapproved) || (t.Tolerate && errors.Is(err, vault.ErrUnavailable))
}

func (t *Transit) PublicKey(ctx context.Context, key string) ([]byte, error) {
	pub, err := t.TransitClient.PublicKey(ctx, key)
	if err != nil && (!t.Tolerate || !errors.Is(err, vault.ErrUnavailable)) {
		return nil, err
	}
	mid, rows, lookupErr := Stored(ctx, t.DB, t.Directory, t.Slug, t.Environment, key)
	if lookupErr != nil {
		// Without the stored identity nothing can be compared: fail closed.
		return nil, fmt.Errorf("solana signer %q: read stored identity: %w: %w", key, vault.ErrUnavailable, lookupErr)
	}
	if err != nil {
		t.unavailable.Store(true)
		if len(rows) == 0 {
			return nil, err
		}
		log.WithField("key", key).Warn("solana signer: Vault unavailable; the stored identity serves until Vault confirms it")
		return rows[len(rows)-1].Identity.Bytes(), nil
	}
	if len(pub) != 32 || len(rows) == 0 {
		return pub, nil
	}
	current := solanago.PublicKeyFromBytes(pub)
	for _, r := range rows {
		if r.Identity.Equals(current) {
			return pub, nil
		}
	}
	previous := rows[len(rows)-1].Identity
	log.WithFields(log.Fields{"merchant_id": mid.String(), "key": key, "stored_public_key": previous.String(), "transit_public_key": current.String()}).
		Error("solana signer: Vault Transit key changed; the Solana rail is refused until an operator approves the new identity")
	if err := t.DB.RunInMerchantScope(ctx, mid, "solana signer change", func(ctx context.Context) error {
		for _, r := range rows {
			if _, err := t.DB.Gen(ctx).SetPSPPendingSigner(ctx, gen.SetPSPPendingSignerParams{PublicKey: current.String(), MerchantID: mid.UUID(), ID: r.Row.ID}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("record solana signer change: %w", err)
	}
	if t.OnChange != nil {
		t.OnChange(fmt.Errorf("solana signer %q changed from %s to %s; awaiting approval", key, previous, current))
	}
	return nil, fmt.Errorf("solana signer %q: %w", key, vault.ErrSignerUnapproved)
}

// StoredSigner is an active Solana PSP signed by a Transit key.
type StoredSigner struct {
	Row      gen.OpenrailsPsp
	Identity solanago.PublicKey
}

// Stored returns the merchant and its active Solana PSPs signed by the
// Transit key, oldest first. A merchant not provisioned yet has none; any
// other failure is returned, never read as "none".
func Stored(ctx context.Context, database *db.DB, directory *merchants.Service, slug, environment, key string) (merchant.ID, []StoredSigner, error) {
	m, err := directory.GetBySlug(ctx, merchant.NormalizeSlug(slug))
	if errors.Is(err, merchants.ErrMerchantNotFound) {
		return merchant.ID{}, nil, nil
	}
	if err != nil {
		return merchant.ID{}, nil, err
	}
	rail := "solana"
	var out []StoredSigner
	err = database.RunInMerchantScope(ctx, m.ID, "stored solana signer", func(ctx context.Context) error {
		rows, err := database.Gen(ctx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{MerchantID: m.ID.UUID(), Rail: &rail})
		if err != nil {
			return err
		}
		for _, row := range rows {
			var evidence struct {
				Signer struct{ Mode, Key string } `json:"signer"`
			}
			if row.Archived || row.Environment != environment || json.Unmarshal(row.Evidence, &evidence) != nil {
				continue
			}
			if evidence.Signer.Mode != "vault_transit" || evidence.Signer.Key != key {
				continue
			}
			if pub, err := solanago.PublicKeyFromBase58(row.AccountID); err == nil {
				out = append(out, StoredSigner{Row: row, Identity: pub})
			}
		}
		return nil
	})
	if err != nil {
		return merchant.ID{}, nil, err
	}
	return m.ID, out, nil
}

// Approve accepts the identity Vault now reports for key: stored identities
// awaiting exactly that key drain (archived, pending cleared). The caller then
// re-applies the merchant's declaration, which provisions the approved one.
// It refuses when nothing is pending for that key.
func Approve(ctx context.Context, database *db.DB, directory *merchants.Service, transit solanaint.TransitClient, mid merchant.ID, environment, key string) (string, error) {
	if transit == nil {
		return "", fmt.Errorf("no Vault Transit signer is configured")
	}
	pub, err := transit.PublicKey(ctx, key)
	if err != nil {
		return "", fmt.Errorf("read the Transit key: %w", err)
	}
	approved := solanago.PublicKeyFromBytes(pub).String()
	m, err := directory.Get(ctx, mid)
	if err != nil {
		return "", err
	}
	_, rows, err := Stored(ctx, database, directory, m.Slug, environment, key)
	if err != nil {
		return "", fmt.Errorf("read stored identity: %w", err)
	}
	var n int64
	if err := database.RunInMerchantScope(ctx, mid, "approve solana signer", func(ctx context.Context) error {
		for _, r := range rows {
			c, err := database.Gen(ctx).ApprovePSPPendingSigner(ctx, gen.ApprovePSPPendingSignerParams{MerchantID: mid.UUID(), ID: r.Row.ID, PublicKey: approved})
			if err != nil {
				return err
			}
			n += c
		}
		return nil
	}); err != nil {
		return "", err
	}
	if n == 0 {
		return "", fmt.Errorf("no signer change awaiting approval for key %q reporting %s", key, approved)
	}
	log.WithFields(log.Fields{"merchant_id": mid.String(), "key": key, "public_key": approved}).Warn("solana signer: operator approved a new identity")
	return approved, nil
}
