package postgresmigrations

import "strings"

// ID-11 (was GAP-10): every UNIQUE index on a merchant-owned table must
// constrain uniqueness WITHIN a merchant. A cross-merchant unique is an
// existence oracle — under RLS the conflicting row is invisible, so the victim
// merchant sees only an opaque insert failure and cannot tell a collision from
// a bug.
//
// This is the ONE exemption list. It is deliberately a non-test file so both
// guards can import it and cannot drift apart:
//
//   - TestUniqueIndexesAreMerchantScoped (migrations/postgres) derives the
//     index inventory from the migration TEXT and so runs with no database;
//   - TestGAP10_UniqueIndexesAreMerchantScoped (internal/invariantaudit) reads
//     pg_indexes on a LIVE database as openrails_app, and so catches an index
//     that reached production by some route other than a migration.
//
// Two checks of the same rule are worth keeping. Two different opinions about
// what the rule exempts are not.

// CrossMerchantUniqueExemptions names every unique index allowed to span
// merchants, with the reason. Anything not here — and not a surrogate-id
// primary key — is a violation.
var CrossMerchantUniqueExemptions = map[string]string{
	"merchants_pkey":                   "the tenant directory itself — its rows ARE the merchants",
	"merchants_slug_key":               "slug is the global tenant address (ID-10)",
	"uq_merchants_api_host_live":       "api_host routes unauthenticated webhooks; it must be globally unique (ID-10)",
	"uq_merchants_permission_group_id": "org<->merchant is 1:1 across the whole install (GAP-9)",
	"probe_verdicts_pkey":              "RLS-exempt: instance-level credential state",
	"worker_health_pkey":               "RLS-exempt: per-worker-kind process health",
	"destructive_action_switch_pkey":   "RLS-exempt: instance-level operator kill switch",
	"schema_migrations_pkey":           "migration ledger",

	// Keyed on a surrogate uuid that is ITSELF merchant-owned. The FK target
	// row cannot be seen, let alone referenced, across a tenant boundary, so
	// the unique cannot collide between merchants and is not an oracle.
	"uq_grants_termination":                       "supersedes_id FKs a merchant-owned grant",
	"uq_payment_settlement_events_payment":        "payment_id FKs a merchant-owned payment",
	"uq_subscription_reprices_one_scheduled":      "subscription_id FKs a merchant-owned subscription",
	"uq_subscriptions_customer_tier_group_active": "customer_id FKs a merchant-owned customer (ID-3's one-live-per-tier-group rule)",
	"unique_prices_product_amount_window":         "product_id FKs a merchant-owned product (ID-5 price financial substance)",
	// uq_destructive_run_before_images_identity was here on that reasoning and
	// is NOT any more (or#902): 0003 leads it with merchant_id, so it needs no
	// exemption. The reasoning was not wrong, it was second-hand — the index was
	// safe because destructive_runs.id happens to be a GLOBAL primary key, and
	// because destructive_run_before_images_run_fk references destructive_runs(id)
	// rather than (merchant_id, id) the safety was never enforced on this table
	// at all. An entry in this class is only sound while the FK it leans on
	// cannot be satisfied across the tenant boundary; check that before adding
	// one, and prefer leading with merchant_id, which needs no such argument.

	// Genuinely global identifiers, unique across the install by construction.
	"solana_subscriptions_subscription_pda_key": "a Solana PDA is globally unique on-chain; two merchants CANNOT share one",
	"uq_psps_identity":                          "one (rail, environment, account_id) gateway account belongs to exactly one merchant — operator-declared config, deliberately install-wide",
	"uq_custodians_identity":                    "the custody sibling (or#880, replacing uq_psps_custodian_identity): one (kind, environment, account_id) custodian tenant belongs to exactly one merchant, for the same reason and by the same operator declaration — and inbound custodian webhooks route by it before any merchant context exists",
}

// UniqueScopeExempt reports whether a unique index that lacks merchant_id is a
// reviewed exception. columns is the index's key column list (a COALESCE(x, …)
// wrapper counts as x); pass it nil when only the name is known.
func UniqueScopeExempt(indexName string, columns []string) bool {
	if _, ok := CrossMerchantUniqueExemptions[indexName]; ok {
		return true
	}
	// A single-column unique on the surrogate `id` is a primary key over a
	// uuid. uuid collisions are not a tenancy question.
	return len(columns) == 1 && columns[0] == "id"
}

// UniqueScopeExemptDef is UniqueScopeExempt for callers holding Postgres'
// rendered `indexdef` rather than a parsed column list.
func UniqueScopeExemptDef(indexName, indexDef string) bool {
	cols := []string(nil)
	if strings.HasSuffix(strings.TrimSpace(indexDef), "USING btree (id)") {
		cols = []string{"id"}
	}
	return UniqueScopeExempt(indexName, cols)
}
