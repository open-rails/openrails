package reconcile

import "context"

// FindingNotifier receives each persisted reconciliation finding. Implementations
// own the requires-review predicate, notification deduplication and delivery.
type FindingNotifier interface {
	NotifyFinding(ctx context.Context, rec FindingRecord) error
}
