package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
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
			"Deps.ClickHouse names an empty or different ClickHouse", config.ClickHouseDatabase(cfg), store.ID(), recorded)
	}
	return store, nil
}

// RequireEventStore refuses what needs the event store, named by what, on a
// deployment without one: 409 event_store_required.
func (r *Runtime) RequireEventStore(what string) error {
	if r != nil && r.EventStore != nil {
		return nil
	}
	return eventstore.Required(what)
}

// ReportDormantUsage names, in one line, the meters, limits, rate cards and
// product limits a deployment without an event store keeps: they serve
// nothing until one is attached.
func (r *Runtime) ReportDormantUsage(ctx context.Context) {
	if r == nil || r.EventStore != nil || r.DB == nil {
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
}
