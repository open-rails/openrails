package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/logx"
)

// attachEventStore migrates (or validates) the event store's database and
// matches its identity with the one this database recorded, recording it on
// first attach. Another identity is an empty or different ClickHouse under a
// database that has used one: refused.
func attachEventStore(ctx context.Context, cfg *config.Config, database *db.DB, conn driver.Conn, logger *slog.Logger) (*eventstore.Store, error) {
	store, err := eventstore.Open(ctx, conn, config.ClickHouseDatabase(cfg), eventstore.Options{Validate: cfg.Database.SkipMigrations, Logger: logger})
	if err != nil {
		return nil, err
	}
	recorded, err := database.GenDirectory().AttachEventStore(ctx, store.ID())
	if errors.Is(err, pgx.ErrNoRows) {
		// A replica recorded it after this statement's snapshot: read it now.
		recorded, err = database.GenDirectory().AttachEventStore(ctx, store.ID())
	}
	if err != nil {
		return nil, fmt.Errorf("record the event store's identity: %w", err)
	}
	if recorded != store.ID() {
		return nil, fmt.Errorf("openrails: ClickHouse database %s is event store %s, but this database's usage is in event store %s: "+
			"Deps.ClickHouse names an empty or different ClickHouse; restore that one, or attach this one with openrails usage attach", config.ClickHouseDatabase(cfg), store.ID(), recorded)
	}
	return store, nil
}

// refuseUnpostedUsage refuses to start without an event store when this
// database has usage the ledger has not taken: only the store can post it,
// and starting would let customers spend balance their usage already used.
func refuseUnpostedUsage(ctx context.Context, database *db.DB) error {
	rows, err := database.GenDirectory().CountUsageUnpostedByMerchant(ctx)
	if err != nil {
		return fmt.Errorf("read unposted usage: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	named := make([]string, 0, len(rows))
	for _, r := range rows {
		named = append(named, fmt.Sprintf("%s (%d customers)", r.MerchantID, r.Customers))
	}
	return fmt.Errorf("%w: usage the ledger has not taken is in the event store, and Deps.ClickHouse is unset; attach the store "+
		"(openrails usage attach --discard-unposted discards it instead): %s", billing.ErrEventStoreRequired, strings.Join(named, ", "))
}

// RequireEventStore refuses what needs the event store, named by what, on a
// deployment without one: 409 event_store_required.
func (r *Runtime) RequireEventStore(what string) error {
	if r != nil && r.EventStore != nil {
		return nil
	}
	return eventstore.Required(what)
}

// ReportDormantUsage names, in one line and a finding per merchant, the
// meters, limits, rate cards and product limits a deployment without an
// event store keeps: they serve nothing until one is attached, which
// resolves the findings.
func (r *Runtime) ReportDormantUsage(ctx context.Context) {
	if r == nil || r.DB == nil {
		return
	}
	rows, err := r.DB.GenDirectory().ListUsageCatalogObjects(ctx)
	if err != nil {
		r.Log.WarnContext(ctx, "event store: cannot list the usage catalog objects without a store", logx.Err(err))
		return
	}
	if len(rows) == 0 {
		return
	}
	if r.EventStore != nil {
		// A store attached: what was dormant serves again.
		for _, row := range rows {
			_, _ = r.DB.GenDirectory().ResolveConfigFinding(ctx, gen.ResolveConfigFindingParams{MerchantID: row.MerchantID,
				FindingType: eventstore.DormantFindingType, SubjectKey: eventstore.DormantFindingSubject})
		}
		return
	}
	counts := map[string]int64{}
	var merchants []string
	for _, row := range rows {
		counts[row.Kind] += row.Objects
		if id := row.MerchantID.String(); !slices.Contains(merchants, id) {
			merchants = append(merchants, id)
		}
	}
	r.Log.WarnContext(ctx, "no event store (Deps.ClickHouse): these merchants' meters, limits and rate cards are dormant",
		"merchants", strings.Join(merchants, ","), "meters", counts["meters"], "limits", counts["limits"],
		"rate_cards", counts["rate_cards"], "product_limits", counts["product_limits"])
	for _, id := range merchants {
		mid := billing.MerchantID(uuid.MustParse(id))
		err := r.DB.RunInMerchantScope(ctx, mid, "dormant usage finding", func(ctx context.Context) error {
			return r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				return recordDormantUsage(ctx, gen.New(tx), mid)
			})
		})
		if err != nil {
			r.Log.WarnContext(ctx, "event store: cannot record the dormant usage finding", logx.Err(err), "merchant_id", id)
		}
	}
}

// recordDormantUsage opens the merchant's finding naming its dormant meters
// and limits.
func recordDormantUsage(ctx context.Context, q *gen.Queries, mid billing.MerchantID) error {
	counts, err := q.CountMerchantUsageCatalog(ctx, mid.UUID())
	if err != nil {
		return err
	}
	if counts.Meters == 0 && counts.Limits == 0 {
		_, err := q.ResolveConfigFinding(ctx, gen.ResolveConfigFindingParams{MerchantID: mid.UUID(), FindingType: eventstore.DormantFindingType, SubjectKey: eventstore.DormantFindingSubject})
		return err
	}
	action := fmt.Sprintf("The catalog declares %d meters and %d limits, which serve nothing: the deployment has no event store (Deps.ClickHouse). "+
		"Attach one, or apply a catalog document without meters and limits to remove them.", counts.Meters, counts.Limits)
	evidence, err := json.Marshal(map[string]int64{"meters": counts.Meters, "limits": counts.Limits})
	if err != nil {
		return err
	}
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: mid.UUID(), FindingType: eventstore.DormantFindingType,
		SubjectKey: eventstore.DormantFindingSubject, Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: evidence})
	return err
}

// UsageDiscard is what attaching another event store discarded of one
// merchant: its customers' unposted usage, recorded as a usage_discard
// maintenance run.
type UsageDiscard struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	RunID      uuid.UUID `json:"run_id"`
	Customers  int       `json:"customers"`
}

// AttachEventStore records store as the database's event store in place of
// the one it recorded: an empty or different ClickHouse. Usage the ledger has
// not taken is only in the old store, so with any, it refuses unless
// discard; then each merchant's unposted usage is deleted and recorded, with
// actor and reason, in the same transaction as the new identity.
func AttachEventStore(ctx context.Context, database *db.DB, store *eventstore.Store, actor, reason string, discard bool) ([]UsageDiscard, error) {
	actor, reason = strings.TrimSpace(actor), strings.TrimSpace(reason)
	if actor == "" || reason == "" {
		return nil, errors.New("attaching an event store names its actor and reason")
	}
	var out []UsageDiscard
	err := database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := database.NewWithPgxTx(tx).Gen(ctx)
		recorded, err := q.LockEventStore(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = q.AttachEventStore(ctx, store.ID())
			return err
		}
		if err != nil || recorded == store.ID() {
			return err
		}
		merchants, err := q.CountUsageUnpostedByMerchant(ctx)
		if err != nil {
			return err
		}
		if len(merchants) > 0 && !discard {
			return fmt.Errorf("%w: %d merchants have usage the ledger has not taken in event store %s; attaching %s discards it (--discard-unposted)",
				billing.ErrEventStoreRequired, len(merchants), recorded, store.ID())
		}
		now := time.Now().UTC()
		for _, m := range merchants {
			rows, err := q.DiscardUsageUnposted(ctx, m.MerchantID)
			if err != nil {
				return err
			}
			affected, err := json.Marshal(map[string]any{"previous_store": recorded, "store": store.ID(), "unposted": rows})
			if err != nil {
				return err
			}
			run, err := q.RecordUsageDiscard(ctx, gen.RecordUsageDiscardParams{MerchantID: m.MerchantID, Actor: actor, Now: now, Affected: affected,
				Note: "attached event store " + store.ID().String() + ": " + reason})
			if err != nil {
				return err
			}
			out = append(out, UsageDiscard{MerchantID: m.MerchantID, RunID: run, Customers: len(rows)})
		}
		return q.ReattachEventStore(ctx, gen.ReattachEventStoreParams{StoreID: store.ID(), Now: now})
	})
	return out, err
}
