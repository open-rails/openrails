package riverjobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/merchant"
)

// or#901: River retries every returned error the same way — up to max_attempts,
// with backoff — because at the queue level a structural failure and a provider
// blip look identical. They are not. A job whose PRECONDITIONS are wrong (the
// database does not have the function the code calls; the context carries no
// merchant) will fail exactly the same way on attempt 25 as on attempt 1, and
// retrying it only buries the defect in noise that reads as transient.
//
// That is measured, not hypothetical: openrails.catalog_reconciliation_pull
// accumulated 578 retryable rows across two stacks and completed ZERO times in
// 15 days, first on `merchant: no merchant resolved on context` and then on
// `SQLSTATE 42883 undefined_function`. A release check flagged the backlog twice
// and both times it read as noise.
//
// StructuralFailureMiddleware converts those two classes into river.JobCancel,
// which is terminal and records the reason on the job row, and logs them at
// error with a typed event so the queue stops being the only witness.

// StructuralFailureMiddleware terminates jobs that failed on a structural
// precondition instead of letting them retry to exhaustion.
type StructuralFailureMiddleware struct {
	river.MiddlewareDefaults
}

// NewStructuralFailureMiddleware builds the classifier. It holds no state; one
// instance is safe to share across every worker.
func NewStructuralFailureMiddleware() *StructuralFailureMiddleware {
	return &StructuralFailureMiddleware{}
}

func (m *StructuralFailureMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	err := doInner(ctx)
	if err == nil {
		return nil
	}
	reason, structural := classifyStructural(err)
	if !structural {
		return err
	}
	// Loud: a terminal refusal that only exists as a job row is the same silence
	// the retries provided.
	log.WithContext(ctx).WithError(err).WithFields(log.Fields{
		"event":       "structural_job_refusal",
		"worker_kind": job.Kind,
		"job_id":      job.ID,
		"attempt":     job.Attempt,
		"reason":      reason,
	}).Error("river: refusing job — structural precondition failure is not retryable")
	return river.JobCancel(&StructuralFailureError{Reason: reason, Kind: job.Kind, Err: err})
}

// StructuralFailureReason names the class of precondition that was violated.
type StructuralFailureReason string

const (
	// StructuralReasonSchemaDrift: the database does not have the object the
	// code named. The binary and the schema describe different databases.
	StructuralReasonSchemaDrift StructuralFailureReason = "schema_drift"
	// StructuralReasonNoMerchantScope: the job reached merchant-scoped work on a
	// context that never had a merchant resolved onto it — a wiring bug.
	StructuralReasonNoMerchantScope StructuralFailureReason = "no_merchant_scope"
)

// StructuralFailureError wraps the underlying failure with its class, so the
// cancelled job row says WHY it will never be retried.
type StructuralFailureError struct {
	Reason StructuralFailureReason
	Kind   string
	Err    error
}

func (e *StructuralFailureError) Error() string {
	return "structural precondition failure (" + string(e.Reason) + ") in " + e.Kind +
		"; retrying cannot fix it: " + e.Err.Error()
}

func (e *StructuralFailureError) Unwrap() error { return e.Err }

// structuralSQLStates are the Postgres SQLSTATEs that mean "the code and the
// schema disagree". Every one of them is deterministic for a given (statement,
// schema) pair, so an identical retry is guaranteed to fail identically.
//
// Deliberately NOT included: connection/administrator-shutdown (class 08/57),
// serialization failures (40001), lock timeouts, and insufficient_resources —
// those are genuinely transient and must keep their retries.
var structuralSQLStates = map[string]struct{}{
	"42883": {}, // undefined_function
	"42P01": {}, // undefined_table
	"42703": {}, // undefined_column
	"42704": {}, // undefined_object
	"42P02": {}, // undefined_parameter
	"3F000": {}, // invalid_schema_name
	"42601": {}, // syntax_error
	"42846": {}, // cannot_coerce
}

// classifyStructural reports whether err is a structural precondition failure,
// and which class.
func classifyStructural(err error) (StructuralFailureReason, bool) {
	if err == nil {
		return "", false
	}
	// A snooze or an already-cancelled job is a deliberate outcome; never
	// reclassify it.
	if errors.Is(err, &rivertype.JobSnoozeError{}) {
		return "", false
	}
	var cancelErr *rivertype.JobCancelError
	if errors.As(err, &cancelErr) {
		return "", false
	}
	if errors.Is(err, merchant.ErrNoMerchant) {
		return StructuralReasonNoMerchantScope, true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if _, ok := structuralSQLStates[pgErr.Code]; ok {
			return StructuralReasonSchemaDrift, true
		}
	}
	return "", false
}
