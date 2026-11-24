// Package disputejob holds the dispute jobs, committed in the transaction of
// the write that calls for them. It sits below the modules so a payment's
// writer can queue its PSP's monitor.
package disputejob

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

const (
	// MonitorKind evaluates one PSP's monitoring programs: after a chargeback
	// or a fraud report, and after a sale while one of its findings is open.
	MonitorKind = "openrails.dispute_monitor"
	// ResponseKind carries out what a chargeback or a fraud report calls for
	// once the destructive gates admit it.
	ResponseKind = "openrails.dispute_response"
	// FindingThreshold is a PSP's ratio at a program's warning or excessive
	// level in a month.
	FindingThreshold = "life.disputes.threshold"
)

// MonitorArgs names one PSP. After is the job already running when the write
// committed: it may have read before it, so this one reads again.
type MonitorArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	PSPID      uuid.UUID `json:"psp_id" river:"unique"`
	After      int64     `json:"after,omitempty" river:"unique"`
}

func (MonitorArgs) Kind() string { return MonitorKind }

// waiting leave out completed, so a finished job never absorbs the next write.
var waiting = []rivertype.JobState{
	rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
	rivertype.JobStateRunning, rivertype.JobStateScheduled,
}

// InsertOpts coalesce a batch of writes on one PSP into one evaluation.
func (MonitorArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", Priority: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: waiting}}
}

// Subject is what a response answers.
type Subject string

const (
	SubjectDispute     Subject = "dispute"
	SubjectFraudReport Subject = "fraud_report"
)

// ResponseArgs names one chargeback or fraud report.
type ResponseArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	Subject    Subject   `json:"subject" river:"unique"`
	ID         uuid.UUID `json:"id" river:"unique"`
}

func (ResponseArgs) Kind() string { return ResponseKind }

// InsertOpts keep one response per subject waiting; a finished one is done.
func (ResponseArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", Priority: 2, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: waiting}}
}

// QueueMonitor commits the PSP's evaluation with d's transaction. A handle
// outside a transaction, or a process with no River producer, queues none.
func QueueMonitor(ctx context.Context, d *db.DB, merchant, psp uuid.UUID) error {
	tx := d.Tx()
	if tx == nil || merchant == uuid.Nil || psp == uuid.Nil {
		return nil
	}
	args := MonitorArgs{MerchantID: merchant, PSPID: psp}
	opts := args.InsertOpts()
	for range 4 {
		res, err := d.InsertRiverJobTx(ctx, tx, args, &opts)
		if errors.Is(err, db.ErrNoRiverProducer) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("queue dispute monitor: %w", err)
		}
		if res == nil || !res.UniqueSkippedAsDuplicate || res.Job == nil || res.Job.State != rivertype.JobStateRunning {
			return nil
		}
		args.After = res.Job.ID
	}
	return fmt.Errorf("queue dispute monitor: jobs already running for PSP %s", psp)
}

// QueueMonitorIfFlagged queues the PSP's evaluation when one of its threshold
// findings is open: the sale that just settled may resolve it.
func QueueMonitorIfFlagged(ctx context.Context, d *db.DB, merchant, psp uuid.UUID) error {
	if d.Tx() == nil || merchant == uuid.Nil || psp == uuid.Nil {
		return nil
	}
	open, err := d.Gen(ctx).PSPHasOpenThresholdFinding(ctx, gen.PSPHasOpenThresholdFindingParams{MerchantID: merchant, PspID: psp})
	if err != nil || !open {
		return err
	}
	return QueueMonitor(ctx, d, merchant, psp)
}

// QueueResponse commits the subject's response with d's transaction.
func QueueResponse(ctx context.Context, d *db.DB, merchant uuid.UUID, subject Subject, id uuid.UUID) error {
	tx := d.Tx()
	if tx == nil {
		return errors.New("a dispute response is queued with the record that calls for it")
	}
	args := ResponseArgs{MerchantID: merchant, Subject: subject, ID: id}
	opts := args.InsertOpts()
	if _, err := d.InsertRiverJobTx(ctx, tx, args, &opts); err != nil && !errors.Is(err, db.ErrNoRiverProducer) {
		return fmt.Errorf("queue dispute response: %w", err)
	}
	return nil
}
