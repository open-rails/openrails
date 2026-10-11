package billing

import (
	"encoding/json"
	"fmt"

	"github.com/open-rails/helpers/notify"
)

// InboxItem projects a customer notice into the shared inbox model. System
// identifies the source installation, not just the product name. The returned
// revision is zero: this is a read-only projection, not a second stored queue.
// Customer notices are historical events, not live workflow state. An
// importing application owns its copy's separate read state.
func (n Notification) InboxItem(system string) (notify.Item, error) {
	if n.ID.IsZero() || n.MerchantID.IsZero() || n.CustomerID.IsZero() {
		return notify.Item{}, fmt.Errorf("%w: notification, merchant and customer IDs are required", notify.ErrInvalid)
	}
	data, err := json.Marshal(n.Data)
	if err != nil {
		return notify.Item{}, fmt.Errorf("encode notification: %w", err)
	}
	in := notify.Input{
		Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: n.CustomerID.String()},
		Origin:    notify.Origin{System: system, Scope: n.MerchantID.String(), ID: n.ID.String()},
		Kind:      n.EventType, Data: data, CreatedAt: n.CreatedAt, ActionRequested: n.ActionRequested,
	}
	in, err = notify.Validate(in)
	if err != nil {
		return notify.Item{}, err
	}
	updated := n.CreatedAt
	if n.ReadAt != nil && n.ReadAt.After(updated) {
		updated = *n.ReadAt
	}
	return notify.Item{ID: n.ID.String(), Input: in, ReadAt: n.ReadAt, UpdatedAt: updated}, nil
}

// CustomerNotificationActionRequested classifies what an event asked when
// emitted. It does not describe the current state of its underlying resource.
// Unknown future kinds remain visible without an action claim.
func CustomerNotificationActionRequested(kind string) bool {
	switch kind {
	case "payment_method_failed", "payment_method_update_required", "payment_authentication_required",
		"order_due", "order_overdue", "order_collection_stopped", "account_delinquent":
		return true
	default:
		return false
	}
}

// InboxItem projects an operator finding into the same inbox model as a
// customer notice, preserving its authoritative workflow and operator-only
// evidence. The merchant's operator queue is the recipient. A caller must
// authorize that audience before exposing the result. Revision zero means
// transitions still go through OpenRails' finding API, never a generic store.
func (f Finding) InboxItem(system string) (notify.Item, error) {
	if f.ID.IsZero() || f.MerchantID.IsZero() {
		return notify.Item{}, fmt.Errorf("%w: finding and merchant IDs are required", notify.ErrInvalid)
	}
	data, err := json.Marshal(f)
	if err != nil {
		return notify.Item{}, fmt.Errorf("encode finding: %w", err)
	}
	in, err := notify.Validate(notify.Input{
		Recipient: notify.Recipient{Audience: notify.AudienceOperator, ID: f.MerchantID.String()},
		Origin:    notify.Origin{System: system, Scope: f.MerchantID.String(), ID: f.ID.String()},
		Kind:      f.Type, Data: data, CreatedAt: f.CreatedAt, Severity: f.Severity, Status: notify.StatusOpen,
	})
	if err != nil {
		return notify.Item{}, err
	}
	switch f.Status {
	case "requires_review", "reconcile_required":
		in.Status = notify.StatusOpen
	case "ignored":
		in.Status = notify.StatusDismissed
	case "fixed", "auto_fixed", "admin_fixed", "superseded", "stale":
		in.Status = notify.StatusResolved
	default:
		return notify.Item{}, fmt.Errorf("%w: unknown finding status %q", notify.ErrInvalid, f.Status)
	}
	return notify.Item{ID: f.ID.String(), Input: in, ResolvedAt: f.ResolvedAt, UpdatedAt: f.UpdatedAt}, nil
}
