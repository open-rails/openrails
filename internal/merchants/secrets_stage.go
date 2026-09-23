package merchants

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

var ErrCredentialOperationConflict = apperr.New(http.StatusConflict, "credential_operation_conflict", "credential operation conflicts with the published revision or existing candidate")

// SecretStager durably creates an immutable candidate. Repeating the same
// operation name and value returns its original version; a different value fails.
type SecretStager interface {
	StageSecret(context.Context, merchant.ID, string, string) (Secret, error)
}

func stageSecret(ctx context.Context, store MerchantSecretStore, id merchant.ID, name, value string) (Secret, error) {
	stager, ok := store.(SecretStager)
	if !ok {
		return Secret{}, fmt.Errorf("%w: durable credential staging unsupported", ErrSecretBackendUnavailable)
	}
	return stager.StageSecret(ctx, id, name, value)
}

func (v *vaultSecretStore) StageSecret(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	if err := validateSecretRef(id, name); err != nil {
		return Secret{}, err
	}
	writer, ok := v.client.(interface {
		WriteSecretCAS(context.Context, string, map[string]string, int) (int, error)
	})
	if !ok {
		return Secret{}, ErrSecretBackendUnavailable
	}
	existing, err := v.Get(ctx, id, name)
	if err == nil {
		if existing.Value != value || existing.Version != 1 {
			return Secret{}, ErrCredentialOperationConflict
		}
		return existing, nil
	}
	if !errors.Is(err, ErrSecretNotFound) {
		return Secret{}, err
	}
	version, writeErr := writer.WriteSecretCAS(ctx, v.pathFor(id, name), map[string]string{"value": value}, 0)
	if writeErr == nil && version > 0 {
		return Secret{Name: name, Value: value, Version: version}, nil
	}
	// A lost reply or competing retry is resolved by backend readback, never by
	// a second blind write. Raw backend responses are not exposed to callers.
	existing, readErr := v.Get(ctx, id, name)
	if readErr == nil {
		if existing.Value != value || existing.Version != 1 {
			return Secret{}, ErrCredentialOperationConflict
		}
		return existing, nil
	}
	return Secret{}, ErrSecretBackendUnavailable
}

func (c *cachedSecretStore) StageSecret(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	return stageSecret(ctx, c.inner, id, name, value)
}
func (s *lifecycleSecretStore) StageSecret(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	// Publication holds the live merchant row lock through staging and commit.
	var result Secret
	err := s.database.MerchantTx(merchant.WithID(ctx, id), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := gen.New(tx).LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			return err
		}
		var err error
		result, err = stageSecret(ctx, s.MerchantSecretStore, id, name, value)
		return err
	})
	return result, err
}
func (e *encryptedSecretStore) StageSecret(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	existing, err := e.Get(ctx, id, name)
	if err == nil {
		if existing.Value != value || existing.Version != 1 {
			return Secret{}, ErrCredentialOperationConflict
		}
		return existing, nil
	}
	if !errors.Is(err, ErrSecretNotFound) {
		return Secret{}, err
	}
	encrypted, err := e.enc.Encrypt(ctx, id, crypto.SecretAAD(id, name), []byte(value))
	if err != nil {
		return Secret{}, ErrSecretBackendUnavailable
	}
	stored, err := stageSecret(ctx, e.inner, id, name, encrypted)
	if err != nil {
		existing, readErr := e.Get(ctx, id, name)
		if readErr == nil && existing.Value == value {
			return existing, nil
		}
		return Secret{}, err
	}
	stored.Value = value
	return stored, nil
}

// CanStageCredentials is an advisory durable-custody gate, never authorization.
// Runtime checks it before creating provider-generated secret material.
func CanStageCredentials(store MerchantSecretStore) bool {
	switch s := store.(type) {
	case *cachedSecretStore:
		return CanStageCredentials(s.inner)
	case *lifecycleSecretStore:
		return CanStageCredentials(s.MerchantSecretStore)
	case *encryptedSecretStore:
		return CanStageCredentials(s.inner)
	case *readOnlySecretStore, *ManifestSecretStore, *manifestManagedSecretStore:
		return false
	case *dbSecretStore, *vaultSecretStore:
		return true
	default:
		return false
	}
}
