# Embedded ↔ standalone mode switch (#544)

OpenRails runs in two modes against the same core data model:

- **Embedded** — OpenRails is a library inside a host app (e.g. doujins). The host
  owns identity/auth; OpenRails runs **no** AuthKit.
- **Standalone (remote)** — OpenRails runs as its own service and owns an AuthKit
  control plane.

A deployment must be able to switch between them by **moving data only** — no
application changes. The contract that makes this possible: the OpenRails core
billing schema is **byte-for-byte identical** in both modes; the only difference
is a set of standalone-only auth tables.

## The three table classes

| Class | Where | Portable? | On a mode switch |
|---|---|---|---|
| **Core billing** | the `openrails` schema (configurable, `db.schema`) | **Yes — the whole schema** | moved as-is |
| **Standalone-only auth** | the `profiles` schema (AuthKit) | No | recreated (embedded→standalone) / dropped (standalone→embedded) |
| **Runtime/infra** | `public` — `river_*` (job queue, #545) + `public.migrations` (ledger) | No | rebuilt by migrations on the target |

Because River (#545) and the migration ledger live in `public`, and AuthKit lives
in `profiles`, the `openrails` schema contains **only** portable billing data.

Two invariants keep it that way, enforced by `TestPortabilityInvariant`
(`migrations/postgres/portability_guard_test.go`):

1. **No core table references the `profiles`/`authkit` schema** (no cross-schema
   FK). The only merchant↔org link is `merchants.owner_org_id` — a bare `text`
   column with **no FK**, the deliberate opaque cross-mode **bridge** (#541): it
   holds the OpenRails-AuthKit org id in standalone, and the host's org id in
   embedded.
2. **River tables are never authored in OpenRails migrations** — they live in
   `public` and are managed by rivermigrate.

Related ownership facts: the **permission catalog is owned by OpenRails**, never
the host (#542) — the host only signs delegated tokens, which OpenRails validates
against its own catalog. OpenRails defines **no org role of its own** (#543);
standalone admins are AuthKit org `owner`s (`org:*`).

## Procedures

### Standalone → embedded (drop auth)

```bash
# On the standalone source:
openrails data export --data-only --out billing.dump   # portable openrails schema only

# On the embedded target (after the host has run embedded migrations):
openrails data import --data-only --in billing.dump
```

The embedded migrator (`RunPostgresEmbedded`) never creates the `profiles` schema,
so the auth tables are simply absent — the host owns auth. `owner_org_id` survives
as an opaque value (no FK to resolve).

### Embedded → standalone (recreate auth)

```bash
# On the embedded source:
openrails data export --data-only --out billing.dump

# On the standalone target (after `openrails migrate up` creates the schema):
openrails data import --data-only --in billing.dump

# Federated hosts (e.g. tensorhub): one command rebuilds org + issuer-as-owner.
openrails auth recreate --issuers merchants.yaml        # backing org + host issuer per merchant
# Non-federated merchants (admin via API key): omit --issuers, add --mint-keys.
openrails auth recreate --mint-keys
```

`import --data-only` loads into the freshly-migrated schema and passes
`--disable-triggers` so the billing schema's circular foreign keys (e.g. `grants`)
restore cleanly (requires a superuser/owner restore role).

`auth recreate` creates a backing AuthKit org for every imported merchant (slug ==
merchant slug, #548), rewrites `merchants.owner_org_id` to the new standalone org
id, and — with `--issuers <merchant-config.yaml>` (#547) — registers each declared
host issuer as the org `owner` remote-application, so the host's delegated tokens
administer that merchant. Without an issuer a merchant gets an admin API key
(`--mint-keys`). The imported `owner_org_id` was the host's org and is
meaningless here. It reuses the idempotent control-plane bootstrap, so it is safe
to re-run.

## Notes

- `data export` is `pg_dump --schema=<openrails schema>` (custom format by
  default); no `--exclude-table` is needed because the schema is already clean.
- River lives in `public` from day one (#545). This is a greenfield hard cut —
  there is no legacy `<schema>.river_*` to drain or drop.
- This flow is exercised end-to-end by a live round-trip against the docker-compose
  Postgres (export → migrate → import → `auth recreate`).
