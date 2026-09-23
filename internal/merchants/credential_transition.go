package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CredentialTransitionRequest identifies one account and its expected published
// revision. Backend connections are host dependencies, never request paths.
type CredentialTransitionRequest struct {
	OperationID      uuid.UUID
	ExpectedRevision int64
	AccountID        string
}

// TransitionProviderCredentials is an operator composition operation. The local
// host constructs and owns source and target stores; the target is s.secrets.
// It copies all required published credentials (including webhook overlap),
// verifies the coherent account, and atomically publishes the new custody and
// exact references. Old source material remains untouched. A host-owned snapshot
// is a valid source. A labeled target snapshot must already contain every
// required value; this operation never writes to host-owned memory.
func (s *Service) TransitionProviderCredentials(ctx context.Context, id merchant.ID, rail string, req CredentialTransitionRequest, source MerchantSecretStore) (PaymentProviderConfig, error) {
	if s == nil || s.pool == nil || source == nil || req.OperationID == uuid.Nil || req.ExpectedRevision < 0 {
		return PaymentProviderConfig{}, apperr.Invalidf("credential transition requires operation, revision, and host-owned stores")
	}
	rail = normalizeProviderSecretType(rail)
	if !supportedPaymentProvider(rail) || req.AccountID == "" {
		return PaymentProviderConfig{}, apperr.Invalidf("credential transition requires a provider account")
	}
	from, to := SecretCustodyIdentity(source), SecretCustodyIdentity(s.secrets)
	snapshotTarget := strings.HasPrefix(to, "snapshot:")
	if from == "" || to == "" || from == to || (!CanStageCredentials(s.secrets) && !snapshotTarget) {
		return PaymentProviderConfig{}, ErrCredentialCustodyTransitionRequired
	}
	// Replay a committed transition before inspecting today's provider state.
	var receipt, metadata []byte
	var expected int64
	var receiptRail, receiptEnvironment, receiptAccount string
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := gen.New(tx).GetCredentialPublication(ctx, gen.GetCredentialPublicationParams{MerchantID: id.UUID(), OperationID: req.OperationID})
		receipt, metadata, expected = stored.Result, stored.RequestMetadata, stored.ExpectedRevision
		receiptRail, receiptEnvironment, receiptAccount = stored.Rail, stored.Environment, stored.AccountID
		return err
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PaymentProviderConfig{}, err
	}
	if len(receipt) > 0 {
		var meta struct{ TransitionFrom string }
		var row gen.OpenrailsPsp
		if json.Unmarshal(metadata, &meta) != nil || json.Unmarshal(receipt, &row) != nil {
			return PaymentProviderConfig{}, ErrSecretBackendUnavailable
		}
		if meta.TransitionFrom != from || expected != req.ExpectedRevision || receiptRail != rail || receiptEnvironment != s.providerEnvironment || receiptAccount != req.AccountID || unmarshalProviderEvidence(row.Evidence).CredentialCustody != to {
			return PaymentProviderConfig{}, ErrCredentialOperationConflict
		}
		for _, ref := range CredentialRefs(row.Evidence) {
			if _, err := ReadSecretRef(ctx, s.secrets, id, ref); err != nil {
				return PaymentProviderConfig{}, err
			}
		}
		return s.paymentProviderConfigWithObligations(ctx, id, row)
	}
	var existing gen.OpenrailsPsp
	err = s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			return err
		}
		if err := AssertPSPUnowned(ctx, q, id.UUID(), rail, s.providerEnvironment, req.AccountID); err != nil {
			return err
		}
		var err error
		existing, err = q.GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{MerchantID: id.UUID(), Rail: rail, Environment: &s.providerEnvironment, AccountID: req.AccountID})
		return err
	})
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	evidence := unmarshalProviderEvidence(existing.Evidence)
	if evidence.Revision != req.ExpectedRevision {
		return PaymentProviderConfig{}, ErrCredentialOperationConflict
	}
	if evidence.CredentialCustody != "" && evidence.CredentialCustody != from {
		return PaymentProviderConfig{}, ErrCredentialCustodyTransitionRequired
	}
	// Migration supports the ordinary provider credential contract. Unknown
	// historical slots fail closed instead of being silently dropped.
	keys := map[string]struct{}{}
	for _, key := range paymentProviderCredentialKeys(rail) {
		keys[key] = struct{}{}
	}
	for key := range evidence.CredentialRefs {
		if _, known := keys[key]; !known {
			return PaymentProviderConfig{}, ErrCredentialCustodyTransitionRequired
		}
	}
	values, names, nameKeys := map[string]string{}, map[string]string{}, map[string]string{}
	var snapshotRefs map[string]SecretRef
	if snapshotTarget {
		snapshotRefs = map[string]SecretRef{}
	}
	for key := range keys {
		name, err := PSPSecretName(rail, s.providerEnvironment, req.AccountID, key)
		if err != nil {
			return PaymentProviderConfig{}, err
		}
		ref, err := PSPSecretRef(rail, s.providerEnvironment, req.AccountID, existing.Evidence, key)
		if err != nil {
			return PaymentProviderConfig{}, err
		}
		value, err := ReadSecretRef(ctx, source, id, ref)
		if errors.Is(err, ErrSecretNotFound) {
			if _, required := evidence.CredentialRefs[key]; required {
				return PaymentProviderConfig{}, err
			}
			continue
		}
		if err != nil {
			return PaymentProviderConfig{}, err
		}
		if rail == "stripe" && key == "secret_key" {
			if err := validateSecretValueLocal(name, value.Value); err != nil {
				return PaymentProviderConfig{}, err
			}
		} else if err := s.ValidateCredential(ctx, id, name, value.Value, nil); err != nil {
			return PaymentProviderConfig{}, err
		}
		if snapshotTarget {
			target, err := s.secrets.Get(ctx, id, name)
			if err != nil {
				return PaymentProviderConfig{}, err
			}
			if target.Value != value.Value {
				return PaymentProviderConfig{}, ErrCredentialOperationConflict
			}
			snapshotRefs[key] = SecretRef{Name: name, MinVersion: target.Version, Custody: to}
		}
		values[key] = value.Value
		names[name] = value.Value
		nameKeys[name] = key
	}
	if len(values) == 0 {
		return PaymentProviderConfig{}, ErrSecretNotFound
	}
	if err := s.refuseLiveNMIUnderTestMode(ctx, id, rail, s.providerEnvironment, req.AccountID, values); err != nil {
		return PaymentProviderConfig{}, err
	}
	verified, err := s.probePaymentProviderCredentials(ctx, id, rail, s.providerEnvironment, req.AccountID, values)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	var verifiedAt *time.Time
	if verified {
		now := time.Now().UTC()
		verifiedAt = &now
	}
	enabled := !existing.Archived
	request := UpsertPaymentProviderConfigRequest{OperationID: req.OperationID, ExpectedRevision: &req.ExpectedRevision, AccountID: req.AccountID, Credentials: values}
	row, err := s.publishProviderCredentials(ctx, id, rail, s.providerEnvironment, req.AccountID, enabled, request, names, nameKeys, verified, verifiedAt, credentialTransitionPublication{From: from, SnapshotRefs: snapshotRefs})
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	return s.paymentProviderConfigWithObligations(ctx, id, row)
}
