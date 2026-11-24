package config

import (
	"time"

	"github.com/open-rails/openrails/internal/retention"
)

// The engine's defaults: the value each setting takes when it is left unset.
// openrails.New and the standalone loader apply them; every other default of
// these settings refers here.
const (
	// DefaultSchema is the Postgres schema OpenRails' tables live in. All SQL
	// is authored in it; any other schema is reached by internal/sqlschema.
	DefaultSchema = "billing"
	// DefaultLeaseConns caps the lease pool (db.lease_conns).
	DefaultLeaseConns = 4
	// DefaultClickHouseDatabase is the event store's database.
	DefaultClickHouseDatabase = "openrails"
	// DefaultEventRetention is how long the event log keeps facts: 90 days.
	DefaultEventRetention = retention.EventLog
	// DefaultWebhookSecretOverlap is how long a rotated-out webhook signing
	// secret keeps verifying.
	DefaultWebhookSecretOverlap = 24 * time.Hour
	// DefaultVaultScopePrefix is where merchant documents live in the KV mount.
	DefaultVaultScopePrefix = "openrails"
	// DefaultVaultTransitMount is the Transit mount Solana signing keys use.
	DefaultVaultTransitMount = "transit"
	// Merchant names: renaming is on, at most once per DefaultRenameInterval,
	// and a former name forwards for DefaultFormerNamesDuration.
	DefaultNamingEnabled       = true
	DefaultRenameInterval      = 72 * time.Hour
	DefaultFormerNamesMode     = FormerNamesFinite
	DefaultFormerNamesDuration = 2160 * time.Hour
)

// RiverSchemaFor is the River schema of an engine in schema when none is
// named: the schema plus "_river".
func RiverSchemaFor(schema string) string { return schema + "_river" }
