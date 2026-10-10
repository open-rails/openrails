// Package signeridentity keeps a Vault Transit Solana signer bound to the
// identity OpenRails stored for it. A Transit key that reports a different
// public key (a rotated key, or Vault reached at another address, namespace
// or mount) fails closed: the change is recorded on the stored PSP rows, the
// Solana rail refuses until an operator approves it, and the new identity is
// never provisioned before that. Embedded and standalone provisioning share it.
package signeridentity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	solanago "github.com/gagliardetto/solana-go"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchants"
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
	// declared maps a Transit key to the PSPs of the declaration being
	// applied that sign with it (Declare).
	declared map[string][]string
}

// Declare names the PSPs of the declaration being applied that sign with
// each Transit key: their stored identities are what a key is checked
// against, before the configuration that names them is loaded.
func (t *Transit) Declare(psps map[string]config.PSPConfig) {
	t.declared = map[string][]string{}
	for key, p := range psps {
		if p.Signer != nil && strings.EqualFold(strings.TrimSpace(p.Signer.Mode), "vault_transit") {
			transit := strings.TrimSpace(p.Signer.Key)
			t.declared[transit] = append(t.declared[transit], strings.ToLower(strings.TrimSpace(key)))
		}
	}
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
	pspKeys, declared := t.declared[key]
	if !declared && t.declared != nil {
		pspKeys = []string{}
	}
	mid, rows, lookupErr := Stored(ctx, t.DB, t.Directory, t.Slug, t.Environment, key, pspKeys)
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
			if _, err := t.DB.Gen(ctx).SetPSPPendingSigner(ctx, gen.SetPSPPendingSignerParams{PublicKey: current.String(), MerchantID: mid.UUID(), ID: r.PSP.ID}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("record solana signer change: %w: %w", vault.ErrUnavailable, err)
	}
	if t.OnChange != nil {
		t.OnChange(fmt.Errorf("solana signer %q changed from %s to %s; awaiting approval", key, previous, current))
	}
	// The PSP keeps its stored identity, marked pending: the rail refuses
	// (ErrSignerUnapproved) until an operator approves the new one.
	return previous.Bytes(), nil
}

// StoredSigner is an active Solana PSP signed by a Transit key.
type StoredSigner struct {
	PSP      merchants.PSPScope
	Identity solanago.PublicKey
}

// Stored returns the merchant and its current Solana PSPs signed by the
// Transit key, oldest first. pspKeys, when non-nil, names those PSPs (the
// declaration being applied), whose identities Postgres holds; nil reads the
// loaded configuration's signers. A merchant not provisioned yet has none;
// any other failure is returned, never read as "none".
func Stored(ctx context.Context, database *db.DB, directory *merchants.Service, slug, environment, key string, pspKeys []string) (billing.MerchantID, []StoredSigner, error) {
	m, err := directory.GetBySlug(ctx, billing.NormalizeMerchantSlug(slug))
	if errors.Is(err, merchants.ErrMerchantNotFound) {
		return billing.MerchantID{}, nil, nil
	}
	if err != nil {
		return billing.MerchantID{}, nil, err
	}
	var psps []merchants.PSPScope
	if pspKeys != nil {
		identities, err := directory.PSPIdentities(ctx, m.ID, "solana", environment)
		if err != nil {
			return billing.MerchantID{}, nil, err
		}
		for _, psp := range identities {
			if slices.Contains(pspKeys, strings.ToLower(psp.Key)) {
				psps = append(psps, psp)
			}
		}
	} else {
		live, err := directory.ActivePSPScopesForRail(ctx, m.ID, "solana", environment)
		if err != nil {
			return billing.MerchantID{}, nil, err
		}
		for _, psp := range slices.Backward(live) {
			if psp.Signer != nil && psp.Signer.Mode == "vault_transit" && psp.Signer.Key == key {
				psps = append(psps, psp)
			}
		}
	}
	var out []StoredSigner
	for _, psp := range psps {
		if pub, err := solanago.PublicKeyFromBase58(psp.AccountID); err == nil {
			out = append(out, StoredSigner{PSP: psp, Identity: pub})
		}
	}
	return m.ID, out, nil
}

// Approve accepts the identity Vault now reports for key: stored identities
// awaiting exactly that key are superseded and drain, and a Vault-held PSP
// document names the approved one. With a file, the caller re-applies the
// declaration, which registers it.
// It refuses when nothing is pending for that key.
func Approve(ctx context.Context, database *db.DB, directory *merchants.Service, transit solanaint.TransitClient, mid billing.MerchantID, environment, key string) (string, error) {
	if transit == nil {
		return "", fmt.Errorf("no Vault Transit signer is configured")
	}
	pub, err := transit.PublicKey(ctx, key)
	if err != nil {
		return "", fmt.Errorf("read the Transit key: %w", err)
	}
	approved := solanago.PublicKeyFromBytes(pub).String()
	identities, err := directory.PSPIdentities(ctx, mid, "solana", environment)
	if err != nil {
		return "", fmt.Errorf("read stored identity: %w", err)
	}
	var rows []StoredSigner
	for _, psp := range identities {
		if psp.SignerChange == approved {
			rows = append(rows, StoredSigner{PSP: psp})
		}
	}
	var n int64
	if err := database.RunInMerchantScope(ctx, mid, "approve solana signer", func(ctx context.Context) error {
		for _, r := range rows {
			c, err := database.Gen(ctx).ApprovePSPPendingSigner(ctx, gen.ApprovePSPPendingSignerParams{MerchantID: mid.UUID(), ID: r.PSP.ID, PublicKey: approved})
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
	for _, r := range rows {
		if err := directory.SetPSPAccount(ctx, mid, r.PSP.Key, approved); err != nil {
			return "", fmt.Errorf("record the approved signer in the PSP document: %w", err)
		}
	}
	log.WithFields(log.Fields{"merchant_id": mid.String(), "key": key, "public_key": approved}).Warn("solana signer: operator approved a new identity")
	return approved, nil
}
