package riverjobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchants"
)

// MerchantSecretCleanupWorker deletes a purged merchant's Vault configuration.
// The purge queued it with its commit; a failure is retried by River.
type MerchantSecretCleanupWorker struct {
	WorkerDefaults[merchants.SecretCleanupArgs]
	Merchants *merchants.Service
}

func (w MerchantSecretCleanupWorker) Work(ctx context.Context, job *river.Job[merchants.SecretCleanupArgs]) error {
	if w.Merchants == nil {
		return fmt.Errorf("merchant secret cleanup is not configured")
	}
	return w.Merchants.CleanupPurgedSecrets(ctx, billing.MerchantID(job.Args.MerchantID), job.Args.RunID)
}
