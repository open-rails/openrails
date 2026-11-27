package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/open-rails/openrails/server/internal/hostdeps"
)

func newUsageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "usage", Short: "Operate the usage event store"}
	attach := &cobra.Command{Use: "attach", Args: cobra.NoArgs, Short: "Attach the configured ClickHouse as the event store in place of the one this database recorded",
		Long: `Attaches the configured ClickHouse (clickhouse.addr, clickhouse.database) as
this database's event store, creating its tables, when the database recorded
another: an empty or different ClickHouse that New refuses.

Usage the ledger has not taken is in PostgreSQL and posts as before; the
attached store has none of the recorded one's usage history or limit counts.
Restore the recorded store's backup instead where one exists.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCLIDB(cmd, func(ctx context.Context, database *db.DB) error {
				store, err := openAttachedStore(ctx, database)
				if err != nil {
					return err
				}
				if err := app.AttachEventStore(ctx, database, store); err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), map[string]any{"store": store.ID()})
			})
		}}
	cmd.AddCommand(attach)
	return cmd
}

// openAttachedStore opens the configured ClickHouse as an event store,
// creating or upgrading its database.
func openAttachedStore(ctx context.Context, database *db.DB) (*eventstore.Store, error) {
	h := hostconfig.FromContext(ctx)
	if h.ClickHouse == nil {
		return nil, fmt.Errorf("no clickhouse section: there is no event store to attach")
	}
	conn, err := hostdeps.ClickHouse(ctx, slog.Default(), h.ClickHouse)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, fmt.Errorf("the default ClickHouse does not answer: there is no event store to attach")
	}
	cli.mu.Lock()
	cli.closes = append(cli.closes, func() { _ = conn.Close() })
	cli.mu.Unlock()
	chdb := config.ClickHouseDatabase(&h.Server.Engine)
	return eventstore.Open(ctx, conn, chdb, eventstore.Options{Logger: slog.Default(), Serialize: migrate.ClickHouseLock(database.Pool().Config(), chdb)})
}
