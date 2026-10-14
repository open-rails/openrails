package riverjobs

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/pspimport"
)

const (
	KindPSPImport = "openrails.psp_import"
	// pspImportAttempts bounds retries of a failing pull or landing; each
	// resumes where the last stopped.
	pspImportAttempts = 5
)

// PSPImportArgs runs one step of a PSP import: its pull or its landing.
type PSPImportArgs struct {
	MerchantID uuid.UUID      `json:"merchant_id"`
	ImportID   uuid.UUID      `json:"import_id"`
	Step       pspimport.Step `json:"step"`
}

func (PSPImportArgs) Kind() string { return KindPSPImport }

// PSPImportWorker runs PSP import steps. A step that fails its last attempt,
// or cannot succeed, marks the import failed with why.
type PSPImportWorker struct {
	river.WorkerDefaults[PSPImportArgs]
	Importer func() *pspimport.Importer
}

func (PSPImportWorker) Kind() string { return KindPSPImport }

func (w *PSPImportWorker) Work(ctx context.Context, job *river.Job[PSPImportArgs]) error {
	im := w.Importer()
	if im == nil {
		return errors.New("psp import: importer not wired")
	}
	mid, id := billing.MerchantID(job.Args.MerchantID), billing.PSPImportID(job.Args.ImportID)
	err := im.Run(ctx, mid, id, job.Args.Step)
	var permanent pspimport.PermanentError
	if err == nil || (!errors.As(err, &permanent) && job.Attempt < pspImportAttempts) {
		return err
	}
	if ferr := im.DB.RunInMerchantScope(ctx, mid, "psp import failure", func(ctx context.Context) error { return im.Fail(ctx, id, err) }); ferr != nil {
		return errors.Join(err, ferr)
	}
	return river.JobCancel(err)
}

// EnqueuePSPImport queues one step of an import. The import's status, moved
// under its row lock before the insert, admits one step at a time; a job
// whose step the status no longer names does nothing.
func EnqueuePSPImport(ctx context.Context, client *river.Client[pgx.Tx], mid billing.MerchantID, id billing.PSPImportID, step pspimport.Step) (int64, error) {
	if client == nil {
		return 0, errors.New("psp import: no River producer")
	}
	res, err := client.Insert(ctx, PSPImportArgs{MerchantID: mid.UUID(), ImportID: id.UUID(), Step: step}, &river.InsertOpts{Queue: QueueBilling, MaxAttempts: pspImportAttempts})
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}
