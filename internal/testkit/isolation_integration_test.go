//go:build integration

package testkit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMain(m *testing.M) { RunMain(m) }

// TestMerchantCustomerIsolation is the first focused-suite scenario. It uses
// one process-owned database, two independently seeded merchants, and the
// unprivileged application pool. Every customer lookup carries the merchant
// predicate used by production SQL; an attempted cross-merchant lookup must
// return no row while each owner can read its own row.
func TestMerchantCustomerIsolation(t *testing.T) {
	ctx := context.Background()
	p := NewPostgres(t)

	merchantA := merchant.ID(uuid.New())
	merchantB := merchant.ID(uuid.New())
	customerA := uuid.New()
	customerB := uuid.New()
	seedMerchant(t, p, merchantA, "focused-isolation-a")
	seedMerchant(t, p, merchantB, "focused-isolation-b")
	p.EnsureCustomer(ctx, t, merchantA, customerA)
	p.EnsureCustomer(ctx, t, merchantB, customerB)

	assertVisible := func(owner merchant.ID, customer uuid.UUID, want bool) {
		t.Helper()
		var got uuid.UUID
		err := p.Pool().QueryRow(ctx,
			`SELECT id FROM `+qualified(p.Schema(), "customers")+` WHERE merchant_id=$1 AND id=$2`,
			owner.UUID(), customer).Scan(&got)
		if want {
			require.NoError(t, err)
			require.Equal(t, customer, got)
		} else {
			require.ErrorIs(t, err, pgx.ErrNoRows)
		}
	}
	assertVisible(merchantA, customerA, true)
	assertVisible(merchantB, customerB, true)
	assertVisible(merchantA, customerB, false)
	assertVisible(merchantB, customerA, false)

	var rows int
	require.NoError(t, p.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM `+qualified(p.Schema(), "customers")+` WHERE merchant_id = ANY($1::uuid[])`,
		[]uuid.UUID{merchantA.UUID(), merchantB.UUID()}).Scan(&rows))
	require.Equal(t, 2, rows)

	receipt, err := json.Marshal(map[string]any{
		"scenario":          "merchant_customer_isolation",
		"merchants":         2,
		"customers":         2,
		"cross_owner_reads": 0,
	})
	require.NoError(t, err)
	t.Logf("focused receipt: %s", receipt)
}

func qualified(schema, table string) string {
	return pgx.Identifier{schema, table}.Sanitize()
}

func seedMerchant(t testing.TB, p *Postgres, id merchant.ID, slug string) {
	t.Helper()
	_, err := p.AdminPool().Exec(context.Background(),
		`INSERT INTO `+qualified(p.Schema(), "merchants")+` (id, slug, status) VALUES ($1, $2, 'active')`,
		id.UUID(), slug)
	require.NoError(t, err)
}
