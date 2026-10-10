package merchantdocs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/vault"
)

// KV is the KV v2 surface the Vault source needs; paths are under the mount.
type KV interface {
	ReadDocument(ctx context.Context, rel string) (vault.Document, error)
	WriteDocument(ctx context.Context, rel string, data json.RawMessage, cas int64) (vault.Document, error)
	ListDocuments(ctx context.Context, rel string) ([]string, error)
	DeleteDocument(ctx context.Context, rel string) error
}

// VaultSource keeps each merchant's configuration under
// <prefix>/merchants/<merchant_id>/: the merchant document at merchant, a PSP
// at psps/<key>, a custodian at custodians/<key>.
type VaultSource struct {
	kv     KV
	prefix string
}

// DefaultPrefix is the path under the KV mount OpenRails owns.
const DefaultPrefix = "openrails"

// NewVaultSource keeps configuration in kv under prefix ("" is
// DefaultPrefix).
func NewVaultSource(kv KV, prefix string) (*VaultSource, error) {
	if kv == nil {
		return nil, fmt.Errorf("merchantdocs: Vault source needs a KV client")
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if path.Clean(prefix) != prefix || strings.Contains(prefix, "..") {
		return nil, fmt.Errorf("merchantdocs: invalid Vault path prefix %q", prefix)
	}
	return &VaultSource{kv: kv, prefix: prefix}, nil
}

// Prefix is where the source keeps documents under the KV mount.
func (v *VaultSource) Prefix() string { return v.prefix }

// MerchantPath is the merchant's subtree under the mount.
func (v *VaultSource) MerchantPath(id billing.MerchantID) string {
	return v.prefix + "/merchants/" + id.UUID().String()
}

func (v *VaultSource) Writable() bool { return true }

func (v *VaultSource) Load(ctx context.Context, id billing.MerchantID) (Set, error) {
	if id.IsZero() {
		return Set{}, fmt.Errorf("merchantdocs: a merchant is required")
	}
	set := Set{}.Clone()
	set.Rejected = map[string]string{}
	base := v.MerchantPath(id) + "/"
	doc, err := v.kv.ReadDocument(ctx, base+merchantDoc)
	if err != nil {
		return Set{}, unavailable(err)
	}
	if doc.Found {
		var m Merchant
		if err := decodeStrict(doc.Data, &m); err != nil {
			set.Rejected[merchantDoc] = err.Error()
		} else {
			set.Merchant, set.HasMerchant = Doc[Merchant]{Value: m, Revision: doc.Version, UpdatedAt: doc.Updated}, true
		}
	}
	keys, err := v.keys(ctx, base+pspDir)
	if err != nil {
		return Set{}, err
	}
	for _, key := range keys {
		doc, err := v.kv.ReadDocument(ctx, base+pspDir+key)
		if err != nil {
			return Set{}, unavailable(err)
		}
		if !doc.Found {
			continue
		}
		var p PSP
		if err := decodeStrict(doc.Data, &p); err != nil {
			set.Rejected[pspDir+key] = err.Error()
			continue
		}
		set.PSPs[key] = Doc[PSP]{Value: p, Revision: doc.Version, UpdatedAt: doc.Updated}
	}
	keys, err = v.keys(ctx, base+custodianDir)
	if err != nil {
		return Set{}, err
	}
	for _, key := range keys {
		doc, err := v.kv.ReadDocument(ctx, base+custodianDir+key)
		if err != nil {
			return Set{}, unavailable(err)
		}
		if !doc.Found {
			continue
		}
		var c Custodian
		if err := decodeStrict(doc.Data, &c); err != nil {
			set.Rejected[custodianDir+key] = err.Error()
			continue
		}
		set.Custodians[key] = Doc[Custodian]{Value: c, Revision: doc.Version, UpdatedAt: doc.Updated}
	}
	if len(set.Rejected) == 0 {
		set.Rejected = nil
	}
	return set, nil
}

// keys lists the document keys under dir; a name that is not a key shape is
// ignored, loudly.
func (v *VaultSource) keys(ctx context.Context, dir string) ([]string, error) {
	names, err := v.kv.ListDocuments(ctx, dir)
	if err != nil {
		return nil, unavailable(err)
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !KeyShape.MatchString(name) {
			log.WithField("path", dir+name).Warn("merchant config: a Vault entry that is not a key is ignored (keys are 1-63 lowercase letters, digits, - or _)")
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

func (v *VaultSource) PutMerchant(ctx context.Context, id billing.MerchantID, doc Merchant, cas int64) (Doc[Merchant], error) {
	written, err := v.put(ctx, v.MerchantPath(id)+"/"+merchantDoc, doc, cas)
	return Doc[Merchant]{Value: doc, Revision: written.Version, UpdatedAt: written.Updated}, err
}

func (v *VaultSource) PutPSP(ctx context.Context, id billing.MerchantID, key string, doc PSP, cas int64) (Doc[PSP], error) {
	if !KeyShape.MatchString(key) {
		return Doc[PSP]{}, fmt.Errorf("merchantdocs: invalid PSP key %q", key)
	}
	written, err := v.put(ctx, v.MerchantPath(id)+"/"+pspDir+key, doc, cas)
	return Doc[PSP]{Value: doc, Revision: written.Version, UpdatedAt: written.Updated}, err
}

func (v *VaultSource) PutCustodian(ctx context.Context, id billing.MerchantID, key string, doc Custodian, cas int64) (Doc[Custodian], error) {
	if !KeyShape.MatchString(key) {
		return Doc[Custodian]{}, fmt.Errorf("merchantdocs: invalid custodian key %q", key)
	}
	written, err := v.put(ctx, v.MerchantPath(id)+"/"+custodianDir+key, doc, cas)
	return Doc[Custodian]{Value: doc, Revision: written.Version, UpdatedAt: written.Updated}, err
}

func (v *VaultSource) put(ctx context.Context, rel string, doc any, cas int64) (vault.Document, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return vault.Document{}, err
	}
	written, err := v.kv.WriteDocument(ctx, rel, data, cas)
	switch {
	case errors.Is(err, vault.ErrCASMismatch):
		return vault.Document{}, ErrRevisionMismatch
	case err != nil:
		return vault.Document{}, unavailable(err)
	}
	return written, nil
}

// Delete removes every document of the merchant, every version.
func (v *VaultSource) Delete(ctx context.Context, id billing.MerchantID) error {
	base := v.MerchantPath(id) + "/"
	for _, dir := range []string{pspDir, custodianDir} {
		names, err := v.kv.ListDocuments(ctx, base+dir)
		if err != nil {
			return unavailable(err)
		}
		for _, name := range names {
			if err := v.kv.DeleteDocument(ctx, base+dir+strings.TrimSuffix(name, "/")); err != nil {
				return unavailable(err)
			}
		}
	}
	if err := v.kv.DeleteDocument(ctx, base+merchantDoc); err != nil {
		return unavailable(err)
	}
	return nil
}

// FingerprintKey is the key credential fingerprints are kept under: random,
// created once in Vault, never in Postgres.
func (v *VaultSource) FingerprintKey(ctx context.Context) ([]byte, error) {
	rel := v.prefix + "/credential_fingerprint_key"
	for range 2 {
		doc, err := v.kv.ReadDocument(ctx, rel)
		if err != nil {
			return nil, unavailable(err)
		}
		if doc.Found {
			var held struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(doc.Data, &held); err != nil {
				return nil, fmt.Errorf("merchantdocs: %s: %w", rel, err)
			}
			key, err := base64.StdEncoding.DecodeString(held.Key)
			if err != nil || len(key) != 32 {
				return nil, fmt.Errorf("merchantdocs: %s holds no 32-byte base64 key", rel)
			}
			return key, nil
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		data, _ := json.Marshal(map[string]string{"key": base64.StdEncoding.EncodeToString(key)})
		_, err = v.kv.WriteDocument(ctx, rel, data, doc.Version)
		if err == nil {
			return key, nil
		}
		if !errors.Is(err, vault.ErrCASMismatch) {
			return nil, unavailable(err)
		}
	}
	return nil, fmt.Errorf("merchantdocs: %s changed while it was created", rel)
}

func unavailable(err error) error {
	if errors.Is(err, ErrUnavailable) {
		return err
	}
	return errors.Join(ErrUnavailable, err)
}

// decodeStrict refuses an unknown field: a misspelled key would otherwise be
// stored inert.
func decodeStrict(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}
