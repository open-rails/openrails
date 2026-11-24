// Package eventstore is OpenRails' event store on ClickHouse (Deps.ClickHouse):
// raw events and the rollups ClickHouse materializes from them, while money
// stays in PostgreSQL. Usage is its first event family. A family is a table on
// one envelope (merchant_id, event_id, customer_id, occurred_at, ingested_at,
// source, source_id, a type and properties) with its own sort key, TTL and
// rollups; every key leads with merchant_id and every query names it, taken
// from the context.
//
// Its DDL is embedded migrations, applied by New (or Migrate) and recorded in
// the database's own schema_migrations. A store has an identity, created with
// it, that PostgreSQL records when it first attaches.
package eventstore

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// databaseToken is how a migration names the store's database.
const databaseToken = "{database}"

// Migration is one embedded migration: its statements name the store's
// database {database}.
type Migration struct {
	Version    uint32
	Name       string
	Checksum   string
	Statements []string
}

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// Migrations are the embedded migrations in order.
func Migrations() ([]Migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	out := make([]Migration, 0, len(names))
	for _, path := range names {
		name := strings.TrimPrefix(path, "migrations/")
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("eventstore: migration %s is not NNNN_name.sql", name)
		}
		version, _ := strconv.ParseUint(m[1], 10, 32)
		body, err := migrationFiles.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		statements := Statements(string(body))
		if len(statements) == 0 {
			return nil, fmt.Errorf("eventstore: migration %s has no statement", name)
		}
		out = append(out, Migration{Version: uint32(version), Name: name, Checksum: hex.EncodeToString(sum[:]), Statements: statements})
	}
	return out, nil
}

// Statements splits a migration into its statements: whole-line -- comments
// are dropped, and a statement ends with a semicolon.
func Statements(body string) []string {
	var kept []string
	for line := range strings.Lines(body) {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	var out []string
	for stmt := range strings.SplitSeq(strings.Join(kept, ""), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// Store is the event store in one ClickHouse database. The host opens and
// closes its connection.
type Store struct {
	conn     driver.Conn
	database string
	id       uuid.UUID
}

// Options is how Open prepares the database.
type Options struct {
	// Validate checks that every migration is applied and applies none
	// (Config.Database.SkipMigrations).
	Validate bool
	Logger   *slog.Logger
}

// Open creates or upgrades database on conn (with opts.Validate, checks it)
// and reads the store's identity.
func Open(ctx context.Context, conn driver.Conn, database string, opts Options) (*Store, error) {
	if conn == nil {
		return nil, errors.New("eventstore: a ClickHouse connection is required")
	}
	s := &Store{conn: conn, database: database}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	var err error
	if opts.Validate {
		err = s.validate(ctx)
	} else {
		err = s.migrate(ctx, logger)
	}
	if err != nil {
		return nil, err
	}
	if s.id, err = s.identity(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Migrate creates or upgrades database on conn: openrails.MigrateClickHouse.
func Migrate(ctx context.Context, conn driver.Conn, database string, logger *slog.Logger) error {
	_, err := Open(ctx, conn, database, Options{Logger: logger})
	return err
}

// ID is the store's identity: created with its database, kept by a backup and
// restore of it, new when the database is created again.
func (s *Store) ID() uuid.UUID { return s.id }

// The finding a merchant's dormant usage catalog opens on a deployment
// without an event store: meters and limits that serve nothing until one is
// attached, or a document without them removes them.
const (
	DormantFindingType    = "consistency.dormant_usage"
	DormantFindingSubject = "usage_catalog"
)

// Required is the refusal of what needs the event store, on a deployment
// without one.
func Required(what string) error {
	return apperr.New(http.StatusConflict, billing.CodeEventStoreRequired, what+" needs the event store: the deployment has no ClickHouse (Deps.ClickHouse)")
}

// resolve names the store's database in a statement.
func (s *Store) resolve(stmt string) string {
	return strings.ReplaceAll(stmt, databaseToken, "`"+s.database+"`")
}

// exec runs a statement. Racing replicas both create: the loser of IF NOT
// EXISTS may be told it exists.
func (s *Store) exec(ctx context.Context, stmt string) error {
	err := s.conn.Exec(ctx, s.resolve(stmt))
	if exists(err) {
		return nil
	}
	return err
}

// ClickHouse's TABLE_ALREADY_EXISTS and DATABASE_ALREADY_EXISTS; and the codes
// of a database or table that is not there.
const (
	codeTableExists     = 57
	codeDatabaseExists  = 82
	codeUnknownTable    = 60
	codeUnknownDatabase = 81
)

func exists(err error) bool {
	var ex *clickhouse.Exception
	return errors.As(err, &ex) && (ex.Code == codeTableExists || ex.Code == codeDatabaseExists)
}

func missing(err error) bool {
	var ex *clickhouse.Exception
	return errors.As(err, &ex) && (ex.Code == codeUnknownTable || ex.Code == codeUnknownDatabase)
}

const migrationsTable = `CREATE TABLE IF NOT EXISTS {database}.schema_migrations (
    version UInt32,
    name String,
    checksum String,
    applied_at DateTime64(6, 'UTC')
) ENGINE = ReplacingMergeTree(applied_at)
ORDER BY version`

// applied is each recorded migration's checksum, by version.
func (s *Store) applied(ctx context.Context) (map[uint32]string, error) {
	rows, err := s.conn.Query(ctx, s.resolve(`SELECT version, checksum FROM {database}.schema_migrations FINAL`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32]string{}
	for rows.Next() {
		var version uint32
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, err
		}
		out[version] = checksum
	}
	return out, rows.Err()
}

func (s *Store) migrate(ctx context.Context, logger *slog.Logger) error {
	migrations, err := Migrations()
	if err != nil {
		return err
	}
	if err := s.exec(ctx, "CREATE DATABASE IF NOT EXISTS "+databaseToken); err != nil {
		return fmt.Errorf("eventstore: create database %s: %w", s.database, err)
	}
	if err := s.exec(ctx, migrationsTable); err != nil {
		return fmt.Errorf("eventstore: %w", err)
	}
	applied, err := s.applied(ctx)
	if err != nil {
		return fmt.Errorf("eventstore: read migrations: %w", err)
	}
	for _, m := range migrations {
		if checksum, ok := applied[m.Version]; ok {
			if checksum != m.Checksum {
				return fmt.Errorf("eventstore: ClickHouse database %s recorded migration %s with other content", s.database, m.Name)
			}
			continue
		}
		for _, stmt := range m.Statements {
			if err := s.exec(ctx, stmt); err != nil {
				return fmt.Errorf("eventstore: migration %s: %w", m.Name, err)
			}
		}
		if err := s.conn.Exec(ctx, s.resolve(`INSERT INTO {database}.schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`),
			m.Version, m.Name, m.Checksum, time.Now().UTC()); err != nil {
			return fmt.Errorf("eventstore: record migration %s: %w", m.Name, err)
		}
		logger.InfoContext(ctx, "event store: applied ClickHouse migration", "database", s.database, "migration", m.Name)
	}
	// The first to create the view names the store; later ones find it.
	return s.exec(ctx, fmt.Sprintf("CREATE VIEW IF NOT EXISTS %s.store AS SELECT toUUID('%s') AS id", databaseToken, uuid.New()))
}

// validate refuses a database Migrate has not prepared for this build.
func (s *Store) validate(ctx context.Context) error {
	migrations, err := Migrations()
	if err != nil {
		return err
	}
	applied, err := s.applied(ctx)
	if missing(err) {
		return required(fmt.Errorf("ClickHouse database %s is not migrated", s.database))
	}
	if err != nil {
		return fmt.Errorf("eventstore: read migrations: %w", err)
	}
	for _, m := range migrations {
		switch checksum, ok := applied[m.Version]; {
		case !ok:
			return required(fmt.Errorf("ClickHouse migration %s is not applied", m.Name))
		case checksum != m.Checksum:
			return required(fmt.Errorf("ClickHouse migration %s was applied with other content", m.Name))
		}
	}
	return nil
}

func required(err error) error {
	return fmt.Errorf("%w; run openrails.MigrateClickHouse before application startup: %w", billing.ErrMigrationsRequired, err)
}

func (s *Store) identity(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.conn.QueryRow(ctx, s.resolve(`SELECT id FROM {database}.store`)).Scan(&id)
	switch {
	case missing(err):
		return uuid.Nil, required(fmt.Errorf("ClickHouse database %s has no store identity", s.database))
	case err != nil:
		return uuid.Nil, fmt.Errorf("eventstore: read identity: %w", err)
	}
	return id, nil
}
