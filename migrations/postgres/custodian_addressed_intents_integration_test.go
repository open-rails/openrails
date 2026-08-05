//go:build integration

package postgresmigrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// insertIntent writes one rail_intents row addressed however the caller says.
// Both address columns are nullable and constrained as a pair, so the test can
// exercise every combination the schema is supposed to admit or refuse.
func (f pspFixture) insertIntent(t *testing.T, key string, psp, custodian *uuid.UUID) error {
	t.Helper()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO openrails.rail_intents
		   (merchant_id, rail, intent_type, idempotency_key, status, next_attempt_at,
		    origin, psp_id, custodian_id)
		 VALUES ($1, 'nmi', 'bt_account_updater_batch', $2, 'pending', now(), 'system', $3, $4)`,
		f.merchant, key, psp, custodian)
	return err
}

func (f pspFixture) newCustodian(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO openrails.custodians (id, merchant_id, key, kind, environment, account_id)
		 VALUES ($1, $2, $3, 'basis_theory', 'live', $4)`,
		id, f.merchant, "bt-"+uuid.NewString()[:8], "tnt_"+uuid.NewString()[:8])
	require.NoError(t, err)
	return id
}

// or#893 × or#795. 0063 made psp_id NOT NULL on rail_intents because every
// intent then in existence was addressed to a gateway account. The batch
// account updater is addressed to a CUSTODIAN — one custodian backs many PSPs,
// so no single psp_id names the write. 0077 admits that class explicitly
// instead of weakening the invariant: an intent still has to name the account
// it will execute against, and now there are two kinds of account.
func TestAnIntentMustNameTheAccountItExecutesAgainst(t *testing.T) {
	f := newPSPFixture(t)
	cust := f.newCustodian(t)

	require.NoError(t, f.insertIntent(t, "psp-addressed-"+uuid.NewString()[:8], &f.pspA, nil),
		"the ordinary PSP-addressed intent")
	require.NoError(t, f.insertIntent(t, "custodian-addressed-"+uuid.NewString()[:8], nil, &cust),
		"or#795: a batch account-updater submit names its custodian, not a PSP")
	// A custodian-proxy write is addressed to a PSP whose card sits at a
	// custodian; knowing both is more provenance, not less.
	require.NoError(t, f.insertIntent(t, "both-"+uuid.NewString()[:8], &f.pspA, &cust))

	err := f.insertIntent(t, "unaddressed-"+uuid.NewString()[:8], nil, nil)
	require.Error(t, err, "an intent addressed to nothing is not representable")
	require.Contains(t, strings.ToLower(err.Error()), "rail_intents_addressed",
		"the refusal names the invariant, not a bare NOT NULL")
}

// The custodian reference cannot cross a merchant boundary — the same composite
// FK psps.custodian_id carries (0053).
func TestAnIntentCannotNameAnotherMerchantsCustodian(t *testing.T) {
	f := newPSPFixture(t)
	other := newPSPFixture(t)
	foreign := other.newCustodian(t)

	err := f.insertIntent(t, "cross-merchant-"+uuid.NewString()[:8], nil, &foreign)
	require.Error(t, err, "a custodian belonging to another merchant is unreferenceable")
	require.Contains(t, strings.ToLower(err.Error()), "rail_intents_custodian_fk")
}

// The mutation log records one row per attempt for EVERY intent, so it needs
// the same two-address vocabulary — otherwise a custodian-addressed attempt
// could not be recorded at all.
func TestAMutationLogCarriesEitherAddress(t *testing.T) {
	f := newPSPFixture(t)
	cust := f.newCustodian(t)
	ctx := context.Background()

	insert := func(psp, custodian *uuid.UUID) error {
		_, err := f.pool.Exec(ctx,
			`INSERT INTO openrails.rail_mutation_logs
			   (merchant_id, rail, psp_id, custodian_id, intent_type, attempt, phase, created_at)
			 VALUES ($1, 'nmi', $2, $3, 'bt_account_updater_batch', 1, 'attempting', $4)`,
			f.merchant, psp, custodian, time.Now().UTC())
		return err
	}
	require.NoError(t, insert(&f.pspA, nil))
	require.NoError(t, insert(nil, &cust))
	err := insert(nil, nil)
	require.Error(t, err, "a mutation nobody can attribute is not recordable")
	require.Contains(t, strings.ToLower(err.Error()), "rail_mutation_logs_addressed")
}
