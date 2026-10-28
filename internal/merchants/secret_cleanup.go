package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchantdocs"
)

// SecretCleanupPlan names the Vault documents a purge removes: the backend,
// the merchant's subtree and the document paths captured before the purge.
// Retries remove the whole subtree, catching documents written after the
// capture.
type SecretCleanupPlan struct {
	Backend string   `json:"backend"`
	Root    string   `json:"root"`
	Names   []string `json:"names"`
}

// captureSecretCleanup plans the removal of the merchant's Vault documents;
// nil when a file holds the configuration, which stays the operator's.
func (s *Service) captureSecretCleanup(ctx context.Context, id billing.MerchantID) (*SecretCleanupPlan, error) {
	if s.config == nil {
		return nil, nil
	}
	vault, ok := s.config.Source().(*merchantdocs.VaultSource)
	if !ok {
		return nil, nil
	}
	set, err := vault.Load(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("inventory merchant configuration: %w", err)
	}
	return &SecretCleanupPlan{Backend: "vault", Root: vault.MerchantPath(id), Names: documentNames(set)}, nil
}

func documentNames(set merchantdocs.Set) []string {
	var names []string
	if set.HasMerchant {
		names = append(names, "merchant")
	}
	for _, key := range set.PSPKeys() {
		names = append(names, merchantdocs.PSPDoc(key))
	}
	for _, key := range set.CustodianKeys() {
		names = append(names, merchantdocs.CustodianDoc(key))
	}
	sort.Strings(names)
	return names
}

func (s *Service) cleanupSecrets(ctx context.Context, id billing.MerchantID, plan SecretCleanupPlan) (int64, error) {
	vault, ok := s.config.Source().(*merchantdocs.VaultSource)
	if !ok || plan.Backend != "vault" || plan.Root != vault.MerchantPath(id) {
		return 0, fmt.Errorf("the merchant configuration's Vault no longer matches the captured purge target")
	}
	if err := s.config.Delete(ctx, id); err != nil {
		return 0, err
	}
	remaining, err := vault.Load(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("verify configuration cleanup: %w", err)
	}
	if left := documentNames(remaining); len(left) > 0 {
		return 0, fmt.Errorf("configuration cleanup still has %d documents", len(left))
	}
	return int64(len(plan.Names)), nil
}

// SecretCleanupArgs is the job that deletes a purged merchant's Vault
// configuration. The purge queues it in its own transaction; River retries it.
type SecretCleanupArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	RunID      uuid.UUID `json:"run_id" river:"unique"`
}

func (SecretCleanupArgs) Kind() string { return "openrails.merchant_secret_cleanup" }

func (SecretCleanupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// WithJobs binds the database whose River producer queues the purge's
// secret cleanup. Without it a purge with Vault configuration refuses.
func (s *Service) WithJobs(d *db.DB) *Service {
	if s != nil {
		s.jobs = d
	}
	return s
}

// ErrSecretCleanupPending means database purge committed but external cleanup
// is not complete. Retrying the captured run never repeats the database purge.
var ErrSecretCleanupPending = errors.New("merchant secret cleanup pending")

// CleanupPurgedSecrets deletes the Vault configuration of an already committed
// purge of a tombstoned merchant. The purge's maintenance run is the task and
// its audit record.
func (s *Service) CleanupPurgedSecrets(ctx context.Context, id billing.MerchantID, runID uuid.UUID) error {
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
		deleted, err := s.cleanupSecrets(ctx, id, *proof.SecretCleanup)
		cleanupErr = err
		status := "completed"
		var message *string
		if err != nil {
			status = "failed"
			text := err.Error()
			message = &text
		}
		_, updateErr := q.RecordMerchantSecretCleanup(ctx, gen.RecordMerchantSecretCleanupParams{MerchantID: id.UUID(), ID: runID, Status: status, Error: message, Deleted: deleted})
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
