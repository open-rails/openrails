package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchantdocs"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// NewDestructiveAdmissions is the shared admission owner, projecting its
// incidents through this runtime.
func (r *Runtime) NewDestructiveAdmissions(clock clockwork.Clock) *destructive.Admissions {
	return destructive.NewAdmissions(r.DB, clock, r.projectDestructiveHold)
}

// WireOneOffDestructiveAdmissions arms a one-off operator runtime (CLI tools
// that write catalog or configuration without starting the server): the
// admission owner, the River producer its incident alerts need, and a
// writable Vault configuration's fingerprint key.
func (r *Runtime) WireOneOffDestructiveAdmissions(ctx context.Context) error {
	r.DestructiveAdmissions = r.NewDestructiveAdmissions(nil)
	if err := r.BindRiverProducer(ctx); err != nil {
		return fmt.Errorf("destructive admission: %w", err)
	}
	if r.MerchantConfig == nil {
		return nil
	}
	vault, ok := r.MerchantConfig.Source().(*merchantdocs.VaultSource)
	if !ok {
		return nil
	}
	key, err := vault.FingerprintKey(ctx)
	if err != nil {
		return fmt.Errorf("configuration admission: %w", err)
	}
	return r.MerchantConfig.SetDestructiveAdmissions(r.DestructiveAdmissions, key)
}

// projectDestructiveHold is transactional: durable authority, the merchant's
// finding, the host event and alert queue either commit together or not at all.
func (r *Runtime) projectDestructiveHold(ctx context.Context, tx pgx.Tx, hold destructive.Hold) error {
	q := r.DB.NewWithPgxTx(tx).Gen(ctx)
	eventType := billing.HostEventDestructiveHoldOpened
	at := hold.OpenedAt
	if hold.ClearedAt == nil {
		// Tenant-visible evidence deliberately excludes the global actor key.
		evidence, err := json.Marshal(hold.View())
		if err != nil {
			return err
		}
		action := "Ask an authorized operator to review and explicitly clear this incident."
		if _, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: hold.MerchantID, FindingType: "life.destructive.hold", SubjectKey: hold.ID.String(), Severity: "critical", Status: "requires_review", RecommendedAction: &action, Evidence: evidence}); err != nil {
			return fmt.Errorf("project destructive finding: %w", err)
		}
		args := riverjobs.DestructiveAlertArgs{MerchantID: hold.MerchantID, HoldID: hold.ID, Scope: string(hold.Scope), OpenedAt: hold.OpenedAt}
		if _, err = r.DB.InsertRiverJobTx(ctx, tx, args, riverjobs.DestructiveAlertOptions()); err != nil {
			return fmt.Errorf("queue destructive incident alert: %w", err)
		}
	} else {
		eventType = billing.HostEventDestructiveHoldCleared
		at = *hold.ClearedAt
		if err := q.ResolveDestructiveHoldFinding(ctx, gen.ResolveDestructiveHoldFindingParams{MerchantID: hold.MerchantID, HoldID: hold.ID.String(), Now: at, Actor: hold.ClearedBy.Issuer + ":" + hold.ClearedBy.ID, Reason: hold.ClearReason}); err != nil {
			return err
		}
	}
	data, err := json.Marshal(billing.DestructiveHoldEvent{HoldID: billing.DestructiveHoldID(hold.ID), Scope: string(hold.Scope)})
	if err != nil {
		return err
	}
	return q.EnqueueDestructiveHoldEvent(ctx, gen.EnqueueDestructiveHoldEventParams{MerchantID: hold.MerchantID, EventType: string(eventType), HoldID: hold.ID, OccurredAt: at, Data: data, DedupeKey: string(eventType) + ":" + hold.ID.String()})
}
