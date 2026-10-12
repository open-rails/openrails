package destructive

import "github.com/open-rails/openrails/billing"

// View exposes incident state without its private actor or target evidence.
func (h Hold) View() billing.DestructiveHold {
	return billing.DestructiveHold{ID: billing.DestructiveHoldID(h.ID), MerchantID: billing.MerchantID(h.MerchantID), Scope: string(h.Scope), Action: string(h.ActionKind), Window: h.Window, Observed: h.Observed, Requested: h.Requested, Limit: h.Limit, OpenedAt: h.OpenedAt, ClearedAt: h.ClearedAt}
}
