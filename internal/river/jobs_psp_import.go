package riverjobs

import (
	"context"
	"errors"
	"time"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/pspimport"
)

const KindPSPImport = pspimport.JobKind

// PSPImportWorker runs PSP import steps. A failing step retries, waiting
// attempt⁴ seconds and at most an hour, so a provider that does not answer is
// retried in the background. A step that cannot succeed (the provider refused
// the credentials, the PSP or a credential is not declared), or that fails
// its last attempt, marks the import failed with why.
type PSPImportWorker struct {
	river.WorkerDefaults[pspimport.JobArgs]
	Importer func() *pspimport.Importer
}

func (PSPImportWorker) Kind() string { return KindPSPImport }

// NextRetry waits pspimport.RetryDelay after a failed attempt.
func (PSPImportWorker) NextRetry(job *river.Job[pspimport.JobArgs]) time.Time {
	return time.Now().Add(pspimport.RetryDelay(job.Attempt))
}

func (w *PSPImportWorker) Work(ctx context.Context, job *river.Job[pspimport.JobArgs]) error {
	im := w.Importer()
	if im == nil {
		return errors.New("psp import: importer not wired")
	}
	mid, id := billing.MerchantID(job.Args.MerchantID), billing.PSPImportID(job.Args.ImportID)
	err := im.Run(ctx, mid, id, job.Args.Step)
	var permanent pspimport.PermanentError
	if err == nil || (!errors.As(err, &permanent) && job.Attempt < job.MaxAttempts) {
		return err
	}
	if ferr := im.DB.RunInMerchantScope(ctx, mid, "psp import failure", func(ctx context.Context) error { return im.Fail(ctx, id, err) }); ferr != nil {
		return errors.Join(err, ferr)
	}
	return river.JobCancel(err)
}
