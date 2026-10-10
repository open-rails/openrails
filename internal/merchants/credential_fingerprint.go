package merchants

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// CredentialFingerprinter keys the fingerprint by a secret held in Vault, so
// the stored value proves nothing about the credential to anyone without it.
type CredentialFingerprinter struct{ key []byte }

// NewCredentialFingerprinter derives the fingerprint key from a 32-byte key
// held in Vault (merchantdocs.VaultSource.FingerprintKey).
func NewCredentialFingerprinter(key []byte) (*CredentialFingerprinter, error) {
	if len(key) != 32 {
		return nil, errors.New("the credential fingerprint key must be 32 bytes")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("openrails/psp-credential-fingerprint/v1"))
	return &CredentialFingerprinter{key: mac.Sum(nil)}, nil
}

// Fingerprint is the keyed hash of credential on rail in environment.
func (f *CredentialFingerprinter) Fingerprint(rail, environment, credential string) string {
	mac := hmac.New(sha256.New, f.key)
	mac.Write([]byte(strings.ToLower(rail) + "\x00" + environment + "\x00" + credential))
	return hex.EncodeToString(mac.Sum(nil))
}

// WithCredentialFingerprinter records the fingerprint of every PSP's account
// credential. nil compares a merchant's declared credentials in memory.
func (s *Service) WithCredentialFingerprinter(f *CredentialFingerprinter) *Service {
	if s != nil {
		s.fingerprints = f
	}
	return s
}

// recordCredentialFingerprint stores the PSP's fingerprint, or marks it a
// duplicate when another PSP on the rail holds the same one.
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
