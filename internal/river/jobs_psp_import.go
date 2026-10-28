package riverjobs

import (
	"context"
	"errors"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/pspimport"
)

const KindPSPImport = pspimport.JobKind

// PSPImportWorker runs PSP import steps. A step that fails its last attempt,
// or cannot succeed, marks the import failed with why.
type PSPImportWorker struct {
	river.WorkerDefaults[pspimport.JobArgs]
	Importer func() *pspimport.Importer
}

func (PSPImportWorker) Kind() string { return KindPSPImport }

func (w *PSPImportWorker) Work(ctx context.Context, job *river.Job[pspimport.JobArgs]) error {
	im := w.Importer()
	if im == nil {
		return errors.New("psp import: importer not wired")
	}
	mid, id := billing.MerchantID(job.Args.MerchantID), billing.PSPImportID(job.Args.ImportID)
	err := im.Run(ctx, mid, id, job.Args.Step)
	var permanent pspimport.PermanentError
	if err == nil || (!errors.As(err, &permanent) && job.Attempt < pspimport.JobAttempts) {
		return err
	}
	if ferr := im.DB.RunInMerchantScope(ctx, mid, "psp import failure", func(ctx context.Context) error { return im.Fail(ctx, id, err) }); ferr != nil {
		return errors.Join(err, ferr)
	}
	return river.JobCancel(err)
}
