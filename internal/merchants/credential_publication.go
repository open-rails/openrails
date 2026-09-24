package merchants

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// publishProviderCredentials stages immutable backend candidates, then publishes
// their complete references with metadata in one SQL transaction. Unpublished
// candidates survive interruption and are recoverable by the caller's operation
// ID. Neither receipts nor SQL publication state contain credential values.
func (s *Service) publishProviderCredentials(ctx context.Context, id merchant.ID, rail, environment, account string, enabled bool, req UpsertPaymentProviderConfigRequest, names, keys map[string]string, validated bool, verifiedAt *time.Time, transitionSource ...credentialTransitionPublication) (gen.OpenrailsPsp, error) {
	transitionFrom := ""
	var publication credentialTransitionPublication
	var snapshotRefs map[string]SecretRef
	if len(transitionSource) > 0 {
		publication = transitionSource[0]
		transitionFrom = publication.From
		snapshotRefs = transitionSource[0].SnapshotRefs
	}
	if req.OperationID == uuid.Nil || req.ExpectedRevision == nil || *req.ExpectedRevision < 0 {
		return gen.OpenrailsPsp{}, apperr.Invalidf("operation_id and nonnegative expected_revision are required")
	}
	if len(names) > 0 {
		if !CanStageCredentials(s.secrets) && snapshotRefs == nil {
			return gen.OpenrailsPsp{}, credentialWriteRefusal(s.secrets)
		}
	}
	custody := SecretCustodyIdentity(s.secrets)
	if custody == "" {
		return gen.OpenrailsPsp{}, ErrSecretBackendUnavailable
	}
	normalizedKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		normalizedKeys = append(normalizedKeys, key)
	}
	sort.Strings(normalizedKeys)
	metadata, err := credentialPublicationMetadata(enabled, req.PublicConfig, normalizedKeys, transitionFrom, custody, publication.WebhookEndpointID, publication.RetireWebhookOverlap)
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
		err := q.CreateCredentialPublication(ctx, gen.CreateCredentialPublicationParams{MerchantID: id.UUID(), OperationID: req.OperationID, Rail: rail, Environment: environment, AccountID: account, ExpectedRevision: *req.ExpectedRevision, RequestMetadata: metadata})
		if err != nil {
			return err
		}
		stored, err := q.LockCredentialPublication(ctx, gen.LockCredentialPublicationParams{MerchantID: id.UUID(), OperationID: req.OperationID})
		if err != nil {
			return err
		}
		completed = stored.Result
		storedMetadata := stored.RequestMetadata
		// JSONB key order differs; decode for canonical comparison.
		var doc any
		_ = json.Unmarshal(storedMetadata, &doc)
		storedMetadata, _ = json.Marshal(doc)
		_ = json.Unmarshal(metadata, &doc)
		canonical, _ := json.Marshal(doc)
		if stored.Rail != rail || stored.Environment != environment || stored.AccountID != account || stored.ExpectedRevision != *req.ExpectedRevision || !bytes.Equal(storedMetadata, canonical) {
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
		if snapshotRefs != nil {
			key := NormalizeCredentialVersionKey(keys[name])
			ref, ok := snapshotRefs[key]
			if !ok {
				return gen.OpenrailsPsp{}, ErrSecretNotFound
			}
			secret, err := ReadSecretRef(ctx, s.secrets, id, ref)
			if err != nil {
				return gen.OpenrailsPsp{}, err
			}
			if secret.Value != names[name] {
				return gen.OpenrailsPsp{}, ErrCredentialOperationConflict
			}
			refs[key] = ref
			continue
		}
		candidate := "credential_candidates/" + req.OperationID.String() + "/" + name
		sec, err := stageSecret(ctx, s.secrets, id, candidate, names[name])
		if err != nil {
			return gen.OpenrailsPsp{}, err
		}
		refs[NormalizeCredentialVersionKey(keys[name])] = SecretRef{Name: candidate, MinVersion: sec.Version, Custody: custody}
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
		receipt, err := q.LockCredentialPublicationResult(ctx, gen.LockCredentialPublicationResultParams{MerchantID: id.UUID(), OperationID: req.OperationID})
		if err != nil {
			return err
		}
		if len(receipt) > 0 {
			return json.Unmarshal(receipt, &result)
		}
		identity, nRail, nEnv, nAccount := PSPNaturalKey(rail, environment, account)
		existing, err := q.LockPSPForCredentialPublication(ctx, gen.LockPSPForCredentialPublicationParams{MerchantID: id.UUID(), Rail: nRail, Environment: nEnv, AccountID: nAccount})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if req.Enabled == nil && existing.ID != uuid.Nil {
			enabled = !existing.Archived
		}
		evidence := unmarshalProviderEvidence(existing.Evidence)
		if evidence.CredentialCustody != "" && evidence.CredentialCustody != custody && evidence.CredentialCustody != transitionFrom {
			return ErrCredentialCustodyTransitionRequired
		}
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

		// SEC-29: rotating the webhook secret keeps the outgoing one only for a
		// bounded overlap; a supplied previous secret is accepted only from the
		// managed endpoint rollover. Retirement ends the overlap at once.
		overlapUntil := time.Time{}
		if transitionFrom == "" && hasWebhookOverlap(rail) {
			if _, supplied := refs["webhook_signing_secret_previous"]; supplied && publication.WebhookEndpointID == "" {
				return apperr.Invalidf("webhook_signing_secret_previous is retained by rotation and cannot be supplied")
			}
			next, rotating := refs["webhook_signing_secret"]
			current, hasCurrent := merged["webhook_signing_secret"]
			if rotating && hasCurrent && !publication.RetireWebhookOverlap {
				changed, err := s.secretRefsDiffer(ctx, id, current, next)
				if err != nil {
					return err
				}
				if changed {
					refs["webhook_signing_secret_previous"] = current
				}
			}
			if _, keeping := refs["webhook_signing_secret_previous"]; keeping {
				overlapUntil = s.now().Add(s.overlapWindow())
				if publication.OverlapFor > 0 {
					overlapUntil = s.now().Add(publication.OverlapFor)
				}
			} else if !publication.RetireWebhookOverlap {
				overlapUntil = webhookOverlapExpiry(existing.Evidence)
			}
		}

		retired := evidence.RetiredCredentials
		if retired == nil {
			retired = map[string]bool{}
		}
		if publication.RetireWebhookOverlap {
			delete(merged, "webhook_signing_secret_previous")
			retired["webhook_signing_secret_previous"] = true
			overlapUntil = time.Time{}
		}
		for key, ref := range refs {
			// Backend versions belong to immutable candidate names; each new
			// candidate can be version one. This separate per-slot generation
			// fences qualifications across rotations, including A -> B -> A.
			generation := versions[key]
			// Every reference being published must remain readable, including
			// a preserved webhook overlap; only a superseded old value may
			// be missing during an explicitly authorized recovery rotation.
			nextSecret, err := ReadSecretRef(ctx, s.secrets, id, ref)
			if err != nil {
				return err
			}
			unchanged := false
			if existing.ID != uuid.Nil && transitionFrom == "" && !retired[key] {
				previous, err := PSPSecretRef(rail, environment, account, existing.Evidence, key)
				if err != nil {
					return err
				}
				oldSecret, err := ReadSecretRef(ctx, s.secrets, id, previous)
				if err != nil && !errors.Is(err, ErrSecretNotFound) {
					return err
				}
				if err == nil {
					unchanged = oldSecret.Value == nextSecret.Value
				}
			}
			if !unchanged || generation == 0 {
				if generation == math.MaxInt {
					return fmt.Errorf("merchants: credential rotation generation exhausted")
				}
				generation++
			}
			// Keep retired-slot generations: later reuse must never reset its
			// epoch. Custody transitions advance even when values are equal.
			versions[key] = generation
			delete(retired, key)
			merged[key] = ref
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
		doc["credential_custody"] = custody
		if transitionFrom != "" {
			doc["credential_custody_transition"] = map[string]any{"operation_id": req.OperationID, "from": transitionFrom, "to": custody}
		}
		doc["credential_refs"] = merged
		doc["retired_credentials"] = retired
		delete(doc, "webhook_overlap_expires_at")
		if !overlapUntil.IsZero() {
			doc["webhook_overlap_expires_at"] = overlapUntil.UTC().Format(time.RFC3339)
		}
		if publication.WebhookEndpointID != "" {
			doc["webhook_endpoint_id"] = publication.WebhookEndpointID
		} else if _, changed := refs["webhook_signing_secret"]; changed && transitionFrom == "" {
			// An ordinary supplied signing key carries no proof that the old
			// provider endpoint uses it. Only qualified creation binds an ID.
			delete(doc, "webhook_endpoint_id")
		}
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
		err = q.CompleteCredentialPublication(ctx, gen.CompleteCredentialPublicationParams{MerchantID: id.UUID(), OperationID: req.OperationID, Result: receipt})
		return err
	})
	return result, err
}

type credentialTransitionPublication struct {
	WebhookEndpointID    string
	RetireWebhookOverlap bool
	// OverlapFor overrides the configured overlap (managed endpoint rollover).
	OverlapFor   time.Duration
	From         string
	SnapshotRefs map[string]SecretRef
}

func credentialPublicationMetadata(enabled bool, public map[string]string, keys []string, source, custody, endpoint string, retire bool) ([]byte, error) {
	return json.Marshal(struct {
		Enabled              bool
		PublicConfig         map[string]string
		Keys                 []string
		TransitionFrom       string
		TargetCustody        string
		WebhookEndpointID    string
		RetireWebhookOverlap bool
	}{enabled, public, keys, source, custody, endpoint, retire})
}

// replayProviderCredentialPublication checks committed custody before any provider
// probe. Secret equality is checked privately against immutable references;
// neither payload values nor their hashes are stored in SQL receipts.
func (s *Service) replayProviderCredentialPublication(ctx context.Context, id merchant.ID, rail, environment, account string, req UpsertPaymentProviderConfigRequest) (gen.OpenrailsPsp, bool, error) {
	var row gen.OpenrailsPsp
	var metadata, receipt []byte
	var storedRail, storedEnv, storedAccount string
	var revision int64
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			return err
		}
		if err := AssertPSPUnowned(ctx, q, id.UUID(), rail, environment, account); err != nil {
			return err
		}
		stored, err := q.GetCredentialPublication(ctx, gen.GetCredentialPublicationParams{MerchantID: id.UUID(), OperationID: req.OperationID})
		storedRail, storedEnv, storedAccount = stored.Rail, stored.Environment, stored.AccountID
		revision, metadata, receipt = stored.ExpectedRevision, stored.RequestMetadata, stored.Result
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	if len(receipt) == 0 {
		return row, false, nil
	}
	if storedRail != rail || storedEnv != environment || storedAccount != account || revision != *req.ExpectedRevision {
		return row, true, ErrCredentialOperationConflict
	}
	if err := json.Unmarshal(receipt, &row); err != nil {
		return row, true, ErrSecretBackendUnavailable
	}
	if unmarshalProviderEvidence(row.Evidence).CredentialCustody != SecretCustodyIdentity(s.secrets) {
		return row, true, ErrCredentialCustodyTransitionRequired
	}
	keys := make([]string, 0, len(req.Credentials))
	for key, value := range req.Credentials {
		key, err = NormalizePSPSecretKey(rail, key)
		if err != nil {
			return row, true, err
		}
		keys = append(keys, key)
		ref, err := PSPSecretRef(rail, environment, account, row.Evidence, key)
		if err != nil {
			return row, true, err
		}
		secret, err := ReadSecretRef(ctx, s.secrets, id, ref)
		if err != nil {
			return row, true, err
		}
		if secret.Value != value {
			return row, true, ErrCredentialOperationConflict
		}
	}
	sort.Strings(keys)
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	desired, err := credentialPublicationMetadata(enabled, req.PublicConfig, keys, "", SecretCustodyIdentity(s.secrets), "", false)
	if err != nil {
		return row, true, err
	}
	var doc any
	if err := json.Unmarshal(metadata, &doc); err != nil {
		return row, true, err
	}
	canonical, _ := json.Marshal(doc)
	if err := json.Unmarshal(desired, &doc); err != nil {
		return row, true, err
	}
	normalized, _ := json.Marshal(doc)
	if !bytes.Equal(canonical, normalized) {
		return row, true, ErrCredentialOperationConflict
	}
	return row, true, nil
}

// secretRefsDiffer compares two published credential values privately.
func (s *Service) secretRefsDiffer(ctx context.Context, id merchant.ID, a, b SecretRef) (bool, error) {
	left, err := ReadSecretRef(ctx, s.secrets, id, a)
	if errors.Is(err, ErrSecretNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	right, err := ReadSecretRef(ctx, s.secrets, id, b)
	if err != nil {
		return false, err
	}
	return left.Value != right.Value, nil
}
