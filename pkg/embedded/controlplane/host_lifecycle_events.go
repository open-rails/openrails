package controlplane

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/app"
	internalcp "github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

// HostLifecycleEvent is one durable signal the embedding host must act on —
// today, an arrears delinquency transition (or#878). OpenRails owns the state
// and emits the signal; the host owns the shutoff.
type HostLifecycleEvent = internalcp.HostLifecycleEvent

// ErrHostLifecycleEventNotFound indicates the event id does not exist under the
// given merchant scope.
var ErrHostLifecycleEventNotFound = internalcp.ErrHostLifecycleEventNotFound

// ListPendingHostLifecycleEvents returns lifecycle signals not yet acknowledged
// by the embedding host, scoped to one merchant. This is a privileged host
// seam; callers must authorize the merchant ID before use.
func ListPendingHostLifecycleEvents(ctx context.Context, a *app.App, id merchant.ID, limit int) ([]HostLifecycleEvent, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	return cp.ListPendingHostLifecycleEvents(ctx, id, limit)
}

// AcknowledgeHostLifecycleEvent marks one of the merchant's events as processed
// by the embedding host. Ack only after the host's own action is durable: an
// unacked event is redelivered, an acked one is gone.
func AcknowledgeHostLifecycleEvent(ctx context.Context, a *app.App, id merchant.ID, eventID uuid.UUID) error {
	cp := Get(a)
	if cp == nil {
		return fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	return cp.AcknowledgeHostLifecycleEvent(ctx, id, eventID)
}
