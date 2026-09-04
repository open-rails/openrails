package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SecretCleanupPlan captures a non-secret backend identity and immutable root.
// Names describe the inventory; retries also re-list the root to catch writes
// that committed after inventory capture but before the purge's merchant lock.
type SecretCleanupPlan struct {
	Backend string   `json:"backend"`
	Root    string   `json:"root"`
	Names   []string `json:"names"`
}

type secretCleanupTarget interface {
	cleanupTarget(merchant.ID) (string, string, error)
}

func baseSecretStore(store MerchantSecretStore) MerchantSecretStore {
	for {
		switch s := store.(type) {
		case *lifecycleSecretStore:
			store = s.MerchantSecretStore
		case *cachedSecretStore:
			store = s.inner
		case *encryptedSecretStore:
			store = s.inner
		case *writeRestrictedSecretStore:
			store = s.inner
		default:
			return store
		}
	}
}

func captureSecretCleanup(ctx context.Context, store MerchantSecretStore, id merchant.ID) (*SecretCleanupPlan, error) {
	if store == nil {
		return nil, nil
	}
	base := baseSecretStore(store)
	if _, ok := base.(*dbSecretStore); ok {
		return nil, nil
	} // purged in the DB transaction
	target, ok := base.(secretCleanupTarget)
	if !ok {
		return nil, fmt.Errorf("secret store %T does not support durable purge", base)
	}
	backend, root, err := target.cleanupTarget(id)
	if err != nil {
		return nil, err
	}
	names, err := store.List(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("inventory external secrets: %w", err)
	}
	sort.Strings(names)
	return &SecretCleanupPlan{Backend: backend, Root: root, Names: names}, nil
}

func clearMerchantSecretCache(store MerchantSecretStore, id merchant.ID) {
	switch s := store.(type) {
	case *lifecycleSecretStore:
		clearMerchantSecretCache(s.MerchantSecretStore, id)
	case *cachedSecretStore:
		s.mu.Lock()
		for key := range s.entries {
			if key.merchant == id.String() {
				delete(s.entries, key)
			}
		}
		s.mu.Unlock()
		clearMerchantSecretCache(s.inner, id)
	case *encryptedSecretStore:
		clearMerchantSecretCache(s.inner, id)
	case *writeRestrictedSecretStore:
		clearMerchantSecretCache(s.inner, id)
	}
}

func cleanupSecrets(ctx context.Context, store MerchantSecretStore, id merchant.ID, plan SecretCleanupPlan) (int, error) {
	target, ok := baseSecretStore(store).(secretCleanupTarget)
	if !ok {
		return 0, fmt.Errorf("captured external secret backend is not configured")
	}
	backend, root, err := target.cleanupTarget(id)
	if err != nil {
		return 0, err
	}
	if backend != plan.Backend || root != plan.Root {
		return 0, fmt.Errorf("external secret cleanup backend/root no longer matches the captured target")
	}
	clearMerchantSecretCache(store, id)
	defer clearMerchantSecretCache(store, id)
	names, err := store.List(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("list external secrets for cleanup: %w", err)
	}
	// Captured names are retried even when a partial/list-inconsistent backend
	// omits one. Delete is idempotent and destroys all Vault versions.
	all := map[string]struct{}{}
	for _, name := range append(names, plan.Names...) {
		if err := validateSecretRef(id, name); err != nil {
			return 0, err
		}
		all[name] = struct{}{}
	}
	deleted := 0
	var failure error
	for name := range all {
		if err := store.Delete(ctx, id, name); err != nil {
			if failure == nil {
				failure = fmt.Errorf("delete external secret %q: %w", name, err)
			}
			continue
		}
		deleted++
	}
	if failure != nil {
		return deleted, failure
	}
	remaining, err := store.List(ctx, id)
	if err != nil {
		return deleted, fmt.Errorf("verify external secret cleanup: %w", err)
	}
	if len(remaining) > 0 {
		return deleted, fmt.Errorf("external secret cleanup still has %d names", len(remaining))
	}
	return deleted, nil
}

// ErrSecretCleanupPending means database purge committed but external cleanup
// is not complete. Retrying the captured run never repeats the database purge.
var ErrSecretCleanupPending = errors.New("merchant secret cleanup pending")

// RetrySecretCleanup resumes only an already committed purge of a tombstoned
// merchant. The destructive run is the durable task and audit authority.
func (s *Service) RetrySecretCleanup(ctx context.Context, id merchant.ID, runID uuid.UUID) error {
	var cleanupErr error
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		run, err := q.LockMerchantSecretCleanupRun(ctx, gen.LockMerchantSecretCleanupRunParams{MerchantID: id.UUID(), ID: runID})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("committed merchant purge cleanup not found")
		}
		if err != nil {
			return err
		}
		if run.Status == "completed" {
			return nil
		}
		var proof struct {
			SecretCleanup *SecretCleanupPlan `json:"secret_cleanup"`
		}
		if err := json.Unmarshal(run.Coverage, &proof); err != nil {
			return fmt.Errorf("decode cleanup target: %w", err)
		}
		if proof.SecretCleanup == nil {
			return fmt.Errorf("merchant purge has no captured cleanup target")
		}
		deleted, err := cleanupSecrets(ctx, s.secrets, id, *proof.SecretCleanup)
		cleanupErr = err
		status := "completed"
		var message *string
		if err != nil {
			status = "failed"
			text := err.Error()
			message = &text
		}
		_, updateErr := q.RecordMerchantSecretCleanup(ctx, gen.RecordMerchantSecretCleanupParams{MerchantID: id.UUID(), ID: runID, Status: status, Error: message, Deleted: int32(deleted)})
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSecretCleanupPending, err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("%w: %w", ErrSecretCleanupPending, cleanupErr)
	}
	return nil
}
