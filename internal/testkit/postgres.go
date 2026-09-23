//go:build integration

// Package testkit contains the small, focused fixtures used by the accelerated
// integration suite. It deliberately sits below the production API: fixtures
// are not part of the OpenRails runtime or consumer SDK.
package testkit

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestMerchantID and TestMerchantSlug identify the one deterministic merchant
// shared by focused tests. A test that needs cross-merchant isolation should
// derive a fresh merchant ID and use its own explicit fixture rows.
var TestMerchantID = dbtest.TestMerchantID

var TestMerchantSlug = dbtest.TestMerchantSlug

// TestCustomerID is the deterministic payer used by the default fixture. Tests
// that exercise isolation should use a fresh UUID instead of sharing it.
var TestCustomerID = uuid.MustParse("a5a5a5a5-0000-4000-8000-000000000002")

// Postgres exposes both sides of the integration boundary:
//
//   - Pool is the ordinary application connection and enforces production RLS.
//   - AdminPool is the privileged fixture connection used only to seed and
//     inspect rows that cross merchant boundaries.
//
// The underlying database is process-owned. A package using this fixture must
// call RunMain from its TestMain; that lets the shared dbtest lifecycle tear
// down its container or per-run external database exactly once after the test
// process exits.
type Postgres struct {
	adminDSN string
	appDSN   string
	schema   string
	admin    *pgxpool.Pool
	app      *pgxpool.Pool
}

// NewPostgres opens the process-owned integration database and two pools. The
// first call provisions a fresh isolated database (or a testcontainer) through
// dbtest. Subsequent calls in the same test process reuse that database while
// each fixture owns and closes its own pools.
func NewPostgres(t *testing.T) *Postgres {
	t.Helper()
	ctx := context.Background()
	adminDSN := dbtest.SharedSuperuserDSN(t)
	appDSN := dbtest.SharedPostgresDSN(t)
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("open testkit admin postgres pool: %v", err)
	}
	app, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		admin.Close()
		t.Fatalf("open testkit application postgres pool: %v", err)
	}
	p := &Postgres{adminDSN: adminDSN, appDSN: appDSN, schema: config.DefaultSchema, admin: admin, app: app}
	t.Cleanup(func() {
		app.Close()
		admin.Close()
	})
	return p
}

// RunMain is the process lifecycle hook for focused integration packages.
//
//	func TestMain(m *testing.M) { testkit.RunMain(m) }
func RunMain(m *testing.M) { dbtest.RunMain(m) }

// DSN returns the ordinary application DSN. It is suitable for starting a
// server or runtime under test; it is intentionally not the privileged DSN.
func (p *Postgres) DSN() string { return p.appDSN }

// AdminDSN returns the privileged fixture DSN. Keep it in fixture setup and
// assertions only; application code must use DSN instead.
func (p *Postgres) AdminDSN() string { return p.adminDSN }

// Pool returns the RLS-enforcing application pool.
func (p *Postgres) Pool() *pgxpool.Pool { return p.app }

// AdminPool returns the privileged fixture pool.
func (p *Postgres) AdminPool() *pgxpool.Pool { return p.admin }

// Schema returns the configured OpenRails schema used by the fixture. SQL in a
// focused scenario must use this value rather than the authored "openrails"
// namespace: migrations are allowed to rewrite that canonical namespace (and
// the default deployment stores the physical tables in billing).
func (p *Postgres) Schema() string { return p.schema }

// Fixture is the deterministic merchant/customer pair used by focused tests.
type Fixture struct {
	MerchantID   merchant.ID
	MerchantSlug string
	CustomerID   uuid.UUID
}

// EnsureFixture materializes the deterministic merchant and customer rows and
// returns their immutable identifiers. The fixture deliberately uses the
// privileged pool because it is also the setup path for tests of RLS itself.
func (p *Postgres) EnsureFixture(ctx context.Context, t testing.TB) Fixture {
	t.Helper()
	dbtest.EnsureTestMerchant(ctx, t, p.admin)
	dbtest.EnsureCustomerIDPgxFor(ctx, t, p.admin, TestMerchantID.UUID(), TestCustomerID.String())
	return Fixture{MerchantID: TestMerchantID, MerchantSlug: TestMerchantSlug, CustomerID: TestCustomerID}
}

// EnsureCustomer materializes a customer under merchantID and returns the
// canonical UUID. It is idempotent and is intended for fixture setup only.
func (p *Postgres) EnsureCustomer(ctx context.Context, t testing.TB, merchantID merchant.ID, customerID uuid.UUID) uuid.UUID {
	t.Helper()
	return dbtest.EnsureCustomerIDPgxFor(ctx, t, p.admin, merchantID.UUID(), customerID.String())
}
