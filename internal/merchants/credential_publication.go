package merchants

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// publishProviderCredentials stages immutable backend candidates, then publishes
// their complete references with metadata in one SQL transaction. Unpublished
// candidates survive interruption and are recoverable by the caller's operation
// ID. Neither receipts nor SQL publication state contain credential values.
func (s *Service) publishProviderCredentials(ctx context.Context, id merchant.ID, rail, environment, account string, enabled bool, req UpsertPaymentProviderConfigRequest, names, keys map[string]string, validated bool, verifiedAt *time.Time) (gen.OpenrailsPsp, error) {
	if req.OperationID == uuid.Nil || req.ExpectedRevision == nil || *req.ExpectedRevision < 0 {
		return gen.OpenrailsPsp{}, fmt.Errorf("merchants: operation_id and nonnegative expected_revision are required")
	}
	if len(names) > 0 {
		if !CanStageCredentials(s.secrets) {
			return gen.OpenrailsPsp{}, ErrManifestSecretsReadOnly
		}
	}
	normalizedKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		normalizedKeys = append(normalizedKeys, key)
	}
	sort.Strings(normalizedKeys)
	metadata, err := json.Marshal(struct {
		Enabled      bool
		PublicConfig map[string]string
		Keys         []string
	}{enabled, req.PublicConfig, normalizedKeys})
	if err != nil {
		return gen.OpenrailsPsp{}, err
	}
	var completed []byte
	// Committed custody state cannot be enlisted in a caller's rollback domain.
	err = s.pool.CommittedMerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			return err
		}
		if err := AssertPSPUnowned(ctx, q, id.UUID(), rail, environment, account); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO openrails.credential_publications (merchant_id,operation_id,rail,environment,account_id,expected_revision,request_metadata) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (merchant_id,operation_id) DO NOTHING`, id.UUID(), req.OperationID, rail, environment, account, *req.ExpectedRevision, metadata)
		if err != nil {
			return err
		}
		var storedRail, storedEnv, storedAccount string
		var revision int64
		var storedMetadata []byte
		err = tx.QueryRow(ctx, `SELECT rail,environment,account_id,expected_revision,request_metadata,result FROM openrails.credential_publications WHERE merchant_id=$1 AND operation_id=$2 FOR UPDATE`, id.UUID(), req.OperationID).Scan(&storedRail, &storedEnv, &storedAccount, &revision, &storedMetadata, &completed)
		if err != nil {
			return err
		}
		// JSONB key order differs; decode for canonical comparison.
		var doc any
		_ = json.Unmarshal(storedMetadata, &doc)
		storedMetadata, _ = json.Marshal(doc)
		_ = json.Unmarshal(metadata, &doc)
		canonical, _ := json.Marshal(doc)
		if storedRail != rail || storedEnv != environment || storedAccount != account || revision != *req.ExpectedRevision || !bytes.Equal(storedMetadata, canonical) {
			return ErrCredentialOperationConflict
		}
		return nil
	})
	if err != nil {
		return gen.OpenrailsPsp{}, err
	}
	refs := make(map[string]SecretRef, len(names))
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		candidate := "credential_candidates/" + req.OperationID.String() + "/" + name
		sec, err := stageSecret(ctx, s.secrets, id, candidate, names[name])
		if err != nil {
			return gen.OpenrailsPsp{}, err
		}
		refs[NormalizeCredentialVersionKey(keys[name])] = SecretRef{Name: candidate, MinVersion: sec.Version}
	}
	if len(completed) > 0 {
		var row gen.OpenrailsPsp
		if err := json.Unmarshal(completed, &row); err != nil {
			return row, ErrSecretBackendUnavailable
		}
		return row, nil
	}
	var result gen.OpenrailsPsp
	err = s.pool.CommittedMerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			return err
		}
		if err := AssertPSPUnowned(ctx, q, id.UUID(), rail, environment, account); err != nil {
			return err
		}
		var receipt []byte
		if err := tx.QueryRow(ctx, `SELECT result FROM openrails.credential_publications WHERE merchant_id=$1 AND operation_id=$2 FOR UPDATE`, id.UUID(), req.OperationID).Scan(&receipt); err != nil {
			return err
		}
		if len(receipt) > 0 {
			return json.Unmarshal(receipt, &result)
		}
		identity, nRail, nEnv, nAccount := PSPNaturalKey(rail, environment, account)
		existing, err := q.GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{MerchantID: id.UUID(), Rail: nRail, Environment: &nEnv, AccountID: nAccount})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		evidence := unmarshalProviderEvidence(existing.Evidence)
		if evidence.Revision != *req.ExpectedRevision {
			return ErrCredentialOperationConflict
		}
		if len(req.PublicConfig) == 0 {
			req.PublicConfig = evidence.PublicConfig
		}
		if !validated && evidence.CredentialsValidated {
			validated = true
			verifiedAt = existing.LastVerifiedAt
		}
		merged := CredentialRefs(existing.Evidence)
		if merged == nil {
			merged = map[string]SecretRef{}
		}
		versions := mergeCredentialVersions(evidence.CredentialVersions, nil)
		if versions == nil {
			versions = map[string]int{}
		}
		if _, rotating := refs["webhook_signing_secret"]; rotating {
			if previous, exists := merged["webhook_signing_secret"]; exists {
				if _, outstanding := merged["webhook_signing_secret_previous"]; outstanding {
					return fmt.Errorf("merchants: webhook rotation overlap requires explicit retirement before another rotation")
				}
				if _, supplied := refs["webhook_signing_secret_previous"]; !supplied {
					merged["webhook_signing_secret_previous"] = previous
					versions["webhook_signing_secret_previous"] = previous.MinVersion
				}
			}
		}
		for key, ref := range refs {
			merged[key] = ref
			versions[key] = ref.MinVersion
		}
		raw, err := marshalProviderEvidence(existing.Evidence, req.PublicConfig, validated, versions)
		if err != nil {
			return err
		}
		doc := map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &doc); err != nil {
				return err
			}
		}
		doc["credential_refs"] = merged
		doc["configuration_revision"] = evidence.Revision + 1
		raw, err = json.Marshal(doc)
		if err != nil {
			return err
		}
		key := existing.Key
		if key == nil {
			key = &nRail
		}
		archived := !enabled
		result, err = q.UpsertPSP(ctx, gen.UpsertPSPParams{ID: identity, Key: key, MerchantID: id.UUID(), Rail: nRail, Environment: &nEnv, AccountID: nAccount, Archived: &archived, Evidence: raw, LastVerifiedAt: verifiedAt})
		if err != nil {
			return err
		}
		receipt, err = json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE openrails.credential_publications SET state='published',result=$3,published_at=now() WHERE merchant_id=$1 AND operation_id=$2`, id.UUID(), req.OperationID, receipt)
		return err
	})
	return result, err
}
