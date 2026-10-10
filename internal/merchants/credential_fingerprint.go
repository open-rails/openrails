package merchants

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// DuplicateAccountFindingType is the finding a duplicate declaration opens.
const DuplicateAccountFindingType = "consistency.duplicate_gateway_account"

const fingerprintIndex = "psps_live_credential_fingerprint_key"

// AccountCredentialKey is the credential that names a rail's gateway account:
// two PSPs holding the same one declare one account. Rails whose account id
// is derived (CCBill, Solana) have none.
func AccountCredentialKey(rail string) string {
	switch strings.ToLower(strings.TrimSpace(rail)) {
	case "nmi":
		return "security_key"
	case "stripe":
		return "secret_key"
	}
	return ""
}

// CredentialFingerprinter keys the fingerprint by a secret derived from the
// encryption master key, so the stored value proves nothing about the
// credential to anyone without that key.
type CredentialFingerprinter struct{ key []byte }

// NewCredentialFingerprinter derives the key from a base64 master key; an
// empty master key answers nil: no stored fingerprints.
func NewCredentialFingerprinter(masterKeyB64 string) (*CredentialFingerprinter, error) {
	if strings.TrimSpace(masterKeyB64) == "" {
		return nil, nil
	}
	master, err := base64.StdEncoding.DecodeString(strings.TrimSpace(masterKeyB64))
	if err != nil || len(master) != 32 {
		return nil, errors.New("encryption master key must be 32 bytes, base64")
	}
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("openrails/psp-credential-fingerprint/v1"))
	return &CredentialFingerprinter{key: mac.Sum(nil)}, nil
}

// Fingerprint is the keyed hash of credential on rail in environment.
func (f *CredentialFingerprinter) Fingerprint(rail, environment, credential string) string {
	mac := hmac.New(sha256.New, f.key)
	mac.Write([]byte(strings.ToLower(rail) + "\x00" + environment + "\x00" + credential))
	return hex.EncodeToString(mac.Sum(nil))
}

// WithCredentialFingerprinter records the fingerprint of every published
// account credential. nil leaves the check to declared (snapshot) PSPs.
func (s *Service) WithCredentialFingerprinter(f *CredentialFingerprinter) *Service {
	if s != nil {
		s.fingerprints = f
	}
	return s
}

// recordCredentialFingerprint stores the PSP's fingerprint, or marks it a
// duplicate when another live PSP on the rail holds the same one.
func recordCredentialFingerprint(ctx context.Context, tx pgx.Tx, merchantID, pspID uuid.UUID, fingerprint string, now time.Time) (bool, error) {
	q := gen.New(tx)
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	err = gen.New(savepoint).SetPSPCredentialFingerprint(ctx, gen.SetPSPCredentialFingerprintParams{MerchantID: merchantID, ID: pspID, Fingerprint: fingerprint})
	if err == nil {
		return false, savepoint.Commit(ctx)
	}
	_ = savepoint.Rollback(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != fingerprintIndex {
		return false, err
	}
	return true, markDuplicate(ctx, q, merchantID, pspID, &fingerprint, now)
}

// markDuplicate disarms the PSP and opens the finding that says why.
func markDuplicate(ctx context.Context, q *gen.Queries, merchantID, pspID uuid.UUID, fingerprint *string, now time.Time) error {
	if err := q.MarkPSPCredentialDuplicate(ctx, gen.MarkPSPCredentialDuplicateParams{MerchantID: merchantID, ID: pspID, Fingerprint: fingerprint, FoundAt: now.UTC()}); err != nil {
		return err
	}
	action := "This PSP's credentials name a gateway account another live PSP already declares, so it is disarmed: nothing charges, refunds or cancels through it. Archive it, or give it the credentials of its own account."
	evidence, err := json.Marshal(map[string]any{"psp_id": pspID.String()})
	if err != nil {
		return err
	}
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: merchantID, FindingType: DuplicateAccountFindingType, SubjectKey: pspID.String(),
		Severity: "critical", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
	})
	return err
}

// DeclaredCredential is one declared PSP's account credential.
type DeclaredCredential struct {
	PSPID      uuid.UUID
	Rail       string
	Credential string
}

// ReconcileDeclaredDuplicates compares declared PSPs' account credentials in
// memory, in declaration order: the first to hold a credential keeps it, any
// later one is a duplicate. A PSP that no longer duplicates another is armed
// again.
func ReconcileDeclaredDuplicates(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, declared []DeclaredCredential, now time.Time) error {
	first := map[string]uuid.UUID{}
	for _, d := range declared {
		if d.Credential == "" || AccountCredentialKey(d.Rail) == "" {
			continue
		}
		key := strings.ToLower(d.Rail) + "\x00" + d.Credential
		if _, taken := first[key]; taken {
			if err := markDuplicate(ctx, q, merchantID, d.PSPID, nil, now); err != nil {
				return fmt.Errorf("mark duplicate PSP %s: %w", d.PSPID, err)
			}
			continue
		}
		first[key] = d.PSPID
		if err := q.ClearPSPCredentialDuplicate(ctx, gen.ClearPSPCredentialDuplicateParams{MerchantID: merchantID, ID: d.PSPID}); err != nil {
			return err
		}
	}
	return nil
}

// FingerprintPSPs records the fingerprint of every live PSP of the merchant
// that has none yet or is a duplicate, as its published credentials stand. A
// PSP whose credential another live PSP holds is disarmed.
func (s *Service) FingerprintPSPs(ctx context.Context, id billing.MerchantID) error {
	if s == nil || s.fingerprints == nil || s.pool == nil {
		return nil
	}
	rows, err := s.pspRows(ctx, id)
	if err != nil {
		return err
	}
	for _, row := range rows {
		accountKey := AccountCredentialKey(row.Rail)
		// A duplicate is checked again: the PSP it duplicated may be archived.
		if row.Archived || row.Environment != s.providerEnvironment || accountKey == "" || (row.CredentialFingerprint != nil && row.CredentialDuplicateAt == nil) {
			continue
		}
		ref, err := pspScopeFromRow(row).publishedRef(accountKey)
		if err != nil || ref.Retired {
			continue
		}
		secret, err := ReadSecretRef(ctx, s.secrets, id, ref)
		if errors.Is(err, ErrSecretNotFound) || (err == nil && secret.Value == "") {
			continue
		}
		if err != nil {
			return err
		}
		fingerprint := s.fingerprints.Fingerprint(row.Rail, row.Environment, secret.Value)
		if err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			_, err := recordCredentialFingerprint(ctx, tx, id.UUID(), row.ID, fingerprint, s.now())
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) pspRows(ctx context.Context, id billing.MerchantID) ([]gen.BillingPsp, error) {
	var rows []gen.BillingPsp
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = gen.New(tx).ListPSPsForMerchant(ctx, id.UUID())
		return err
	})
	return rows, err
}
