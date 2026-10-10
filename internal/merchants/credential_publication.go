package merchants

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// publishProviderCredentials stages immutable backend candidates, then publishes
// their complete references with metadata in one SQL transaction. Unpublished
// candidates survive interruption and are recoverable by the caller's operation
// ID. Neither receipts nor SQL publication state contain credential values.
func (s *Service) publishProviderCredentials(ctx context.Context, id billing.MerchantID, rail, environment, account string, req pspPublication, names, keys map[string]string, validated bool, verifiedAt *time.Time, publication webhookPublication) (gen.BillingPsp, error) {
	if req.OperationID == uuid.Nil || req.ExpectedRevision < 0 {
		return gen.BillingPsp{}, apperr.Invalidf("operation_id and nonnegative expected_revision are required")
	}
	if len(names) > 0 && !CanStageCredentials(s.secrets) {
		return gen.BillingPsp{}, credentialWriteRefusal(s.secrets)
	}
	custody := SecretCustodyIdentity(s.secrets)
	if custody == "" {
		return gen.BillingPsp{}, ErrSecretBackendUnavailable
	}
	normalizedKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		normalizedKeys = append(normalizedKeys, key)
	}
	sort.Strings(normalizedKeys)
	metadata, err := credentialPublicationMetadata(req.Key, req.Settings, normalizedKeys, custody, publication.WebhookEndpointID, publication.RetireWebhookOverlap)
	if err != nil {
		return gen.BillingPsp{}, err
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
		err := q.CreateCredentialPublication(ctx, gen.CreateCredentialPublicationParams{MerchantID: id.UUID(), OperationID: req.OperationID, Rail: rail, Environment: environment, AccountID: account, ExpectedRevision: req.ExpectedRevision, RequestMetadata: metadata})
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
		if stored.Rail != rail || stored.Environment != environment || stored.AccountID != account || stored.ExpectedRevision != req.ExpectedRevision || !bytes.Equal(storedMetadata, canonical) {
			return ErrCredentialOperationConflict
		}
		return nil
	})
	if err != nil {
		return gen.BillingPsp{}, err
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
			return gen.BillingPsp{}, err
		}
		refs[NormalizeCredentialVersionKey(keys[name])] = SecretRef{Name: candidate, MinVersion: sec.Version, Custody: custody}
	}
	if len(completed) > 0 {
		var row gen.BillingPsp
		if err := json.Unmarshal(completed, &row); err != nil {
			return row, ErrSecretBackendUnavailable
		}
		return row, nil
	}
	var result gen.BillingPsp
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
		_, lockRail, lockEnv, lockAccount := PSPNaturalKey(rail, environment, account)
		existing, err := q.LockPSPForCredentialPublication(ctx, gen.LockPSPForCredentialPublicationParams{MerchantID: id.UUID(), Rail: lockRail, Environment: lockEnv, AccountID: lockAccount})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		state := credentialState(existing)
		if state.Custody != "" && state.Custody != custody {
			return ErrCredentialCustodyTransitionRequired
		}
		if state.Revision != req.ExpectedRevision {
			return ErrCredentialOperationConflict
		}
		key := existing.Key
		if existing.ID == uuid.Nil {
			key = strings.ToLower(strings.TrimSpace(req.Key))
			if key == "" {
				return apperr.Invalidf("a new PSP needs a key")
			}
		}
		settings := mergeSettings(rowSettings(existing), req.Settings)
		if !validated {
			verifiedAt = existing.CredentialsValidatedAt
		}
		merged := state.Refs
		versions := mergeCredentialVersions(state.Versions, nil)
		if versions == nil {
			versions = map[string]int{}
		}

		// SEC-29: rotating the webhook secret keeps the outgoing one only for a
		// bounded overlap; a supplied previous secret is accepted only from the
		// managed endpoint rollover. Retirement ends the overlap at once.
		overlapUntil := time.Time{}
		if hasWebhookOverlap(rail) {
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
				overlapUntil = webhookOverlapExpiry(existing)
			}
		}

		retired := state.Retired
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
			if existing.ID != uuid.Nil && !retired[key] {
				previous, err := PSPSecretRef(existing, key)
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
			// epoch.
			versions[key] = generation
			delete(retired, key)
			merged[key] = ref
		}
		endpoint := existing.WebhookEndpointID
		if publication.WebhookEndpointID != "" {
			endpoint = &publication.WebhookEndpointID
		} else if _, changed := refs["webhook_signing_secret"]; changed {
			// An ordinary supplied signing key carries no proof that the old
			// provider endpoint uses it. Only qualified creation binds an ID.
			endpoint = nil
		}
		var overlap *time.Time
		if !overlapUntil.IsZero() {
			until := overlapUntil.UTC()
			overlap = &until
		}
		settingsJSON, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		refsJSON, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		versionsJSON, err := json.Marshal(versions)
		if err != nil {
			return err
		}
		identity, nRail, nEnv, nAccount := PSPNaturalKey(rail, environment, account)
		if existing.ID != uuid.Nil {
			identity = existing.ID
		}
		result, err = q.PublishPSP(ctx, gen.PublishPSPParams{
			ID: identity, MerchantID: id.UUID(), Key: key, Rail: nRail, Environment: nEnv, AccountID: nAccount,
			Settings: settingsJSON, CredentialCustody: &custody,
			CredentialRefs: refsJSON, CredentialVersions: versionsJSON, RetiredCredentials: retiredList(retired),
			CredentialsValidatedAt: verifiedAt, WebhookEndpointID: endpoint, WebhookOverlapExpiresAt: overlap,
			Revision: state.Revision + 1,
		})
		if err != nil {
			return mapPSPWriteError(err)
		}
		if err := s.fingerprintPublication(ctx, tx, id.UUID(), result, names, keys); err != nil {
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

// webhookPublication carries a managed webhook endpoint's facts into a
// publication.
type webhookPublication struct {
	WebhookEndpointID    string
	RetireWebhookOverlap bool
	// OverlapFor overrides the configured overlap (managed endpoint rollover).
	OverlapFor time.Duration
}

func credentialPublicationMetadata(key string, settings map[string]any, keys []string, custody, endpoint string, retire bool) ([]byte, error) {
	return json.Marshal(struct {
		Key                  string
		Settings             map[string]any
		Keys                 []string
		TargetCustody        string
		WebhookEndpointID    string
		RetireWebhookOverlap bool
	}{strings.ToLower(strings.TrimSpace(key)), settings, keys, custody, endpoint, retire})
}

// replayProviderCredentialPublication checks committed custody before any provider
// probe. Secret equality is checked privately against immutable references;
// neither payload values nor their hashes are stored in SQL receipts.
func (s *Service) replayProviderCredentialPublication(ctx context.Context, id billing.MerchantID, rail, environment, account string, req pspPublication) (gen.BillingPsp, bool, error) {
	var row gen.BillingPsp
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
	if storedRail != rail || storedEnv != environment || storedAccount != account || revision != req.ExpectedRevision {
		return row, true, ErrCredentialOperationConflict
	}
	if err := json.Unmarshal(receipt, &row); err != nil {
		return row, true, ErrSecretBackendUnavailable
	}
	if credentialState(row).Custody != SecretCustodyIdentity(s.secrets) {
		return row, true, ErrCredentialCustodyTransitionRequired
	}
	keys := make([]string, 0, len(req.Credentials))
	for key, value := range req.Credentials {
		key, err = NormalizePSPSecretKey(rail, key)
		if err != nil {
			return row, true, err
		}
		keys = append(keys, key)
		ref, err := PSPSecretRef(row, key)
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
	desired, err := credentialPublicationMetadata(req.Key, req.Settings, keys, SecretCustodyIdentity(s.secrets), "", req.RetireWebhookOverlap)
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
func (s *Service) secretRefsDiffer(ctx context.Context, id billing.MerchantID, a, b SecretRef) (bool, error) {
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

// fingerprintPublication records the fingerprint of the account credential a
// publication carries; a credential another live PSP holds disarms this one.
func (s *Service) fingerprintPublication(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, row gen.BillingPsp, names, keys map[string]string) error {
	accountKey := AccountCredentialKey(row.Rail)
	if s.fingerprints == nil || accountKey == "" {
		return nil
	}
	for name, value := range names {
		if keys[name] != accountKey || value == "" {
			continue
		}
		_, err := recordCredentialFingerprint(ctx, tx, merchantID, row.ID, s.fingerprints.Fingerprint(row.Rail, row.Environment, value), s.now())
		return err
	}
	return nil
}
