# Operations

Running an OpenRails deployment: what it needs, the safety levers, what runs by
itself, what to do when it drifts, and how long it keeps what.
[Dunning](dunning.md), [provider uncertainty](provider-uncertainty.md),
[destructive-action limits](destructive-actions.md) and [Vault](vault.md) have
their own pages.

## Requirements

| Service | Needed | Holds | Losing it |
|---|---|---|---|
| PostgreSQL 18+ | yes | everything: the ledger, grants, subscriptions, access, catalog, provider intents and River's queue. It can share an instance with the host app; OpenRails owns one schema (`billing`) and its River schema (`billing_river`). | data loss. Provider-owned facts can be re-read with a PSP import; the ledger, credits, access and catalog exist nowhere else. |
| Redis (or Garnet) | for more than one instance | rate limits, admin lockouts, captcha challenges, card-testing declines, spent DPoP proofs and admission-denial counters | without it these live in each process's memory, right for one instance only. No money path needs Redis. |
| Vault | optional | with `vault.kv_mount`, every merchant's configuration, credentials included; a Transit mount signs for Solana | without a KV mount the merchant manifest is the configuration, read-only. With one, startup needs Vault; afterwards each replica rides out an outage from its cache. |

The standalone server's signing keys are `keys.json` in `auth.keys.path`
(`AUTH_KEYS_PATH`, file-watched) or `AUTHKIT_ACTIVE_KEY_ID` /
`AUTHKIT_ACTIVE_PRIVATE_KEY_PEM` / `AUTHKIT_PUBLIC_KEYS`. The same directory
holds `totp.key`; boot refuses a control plane with neither `totp.key` nor an
email or SMS sender, because the root owner needs a second factor.

There is no row-level security: every tenant query carries its `merchant_id`
(or `psp_id`) predicate, backed by composite foreign keys. Run migrations and
the server as one role, which owns what it creates; two apps sharing a schema
connect as that role or `SET ROLE` to it. Locally, `task docker-up` starts
PostgreSQL, Redis and the server on `:3053` (`task docker-down`,
`task docker-reset`).

## Who owns what

OpenRails owns the catalog; the provider owns money state (is a subscription
alive, what was charged); access is derived from both. So there are four ways to
diverge:

| Divergence | Mechanism |
|---|---|
| Catalog wrong at the provider | none needed: `catalog apply` only reads existing provider bindings (and may publish a recurring Solana price's on-chain plan). Nothing watches provider catalogs. |
| Money state wrong locally | webhooks; [Provider Refresh](#provider-refresh) every 2 h; a manual PSP import (`POST /v1/merchant/psp-imports`) whose differences become proposals |
| Outbound action never executed | the [provider intent ledger](#provider-intents) |
| Access inconsistent | the [Convergence Engine](#convergence) re-derives it |

## One billing database

All replicas serving a book share one PostgreSQL database with one writable
primary; locks, unique operation keys and executor claims coordinate them.
Independently writable copies using the same PSP accounts are unsupported: each
would take its own locks and collect the same renewal. Provider idempotency
does not make them safe, and a provider lookup followed by a charge is not a
lock. Keep replicas NTP-synchronized; leases and provider deadlines need it.

Two fences stop a copy from billing:

- **Book identity.** `billing.book_identity` records the cluster, database and
  schema the book was armed in. Anywhere else (a dump restored elsewhere, a
  re-initialized cluster) every provider write is readonly and
  `openrails book status` says so. `openrails book arm --by NAME` arms it once
  every other copy is stopped. A promoted physical replica keeps the identity,
  and so does a physical clone: rotate the PSP credentials before running one.
- **Merchant write posture.** `billing.merchant_write_posture` lowers one
  merchant below `provider_write_mode`. A billing export leaves its source
  readonly and an import lands readonly, until
  `openrails merchant arm --merchant NAME --by NAME`;
  `openrails merchant hold --mode readonly|limited` holds one merchant.

Move a book only by the [offline transfer procedure](merchant-portability.md).
Never restart a stale source as a rollback.

## Several instances

Any number of instances can serve one book: host replicas that each call
`openrails.New` and `Start`, or `openrails run-server` and `run-worker`
processes. There is no instance count, primary or sticky session.

- **Shared:** PostgreSQL holds every record, River's queue and leader election,
  idempotency claims, webhook deduplication and charge admission. Redis
  (`Config.Redis`, an address or `redis://`/`rediss://` URL, or `Deps.Redis`,
  any `redis.UniversalClient`) holds the abuse state above, never PostgreSQL. A
  declared Redis that stops answering costs sharing, not service: each instance
  falls back to memory, logs, counts `openrails_abuse_state_fallbacks_total`
  and reports Redis degraded; what it counted meanwhile still counts when Redis
  returns.
- **Identical everywhere:** schemas, `ProviderWriteMode`, merchant
  configuration (the same `Config.Merchants` or Vault mount), `RateLimits`,
  `Captcha`, `TrustedProxies`, `ReturnOrigins`, the mounted `Routes` (a route
  some instances lack is a 404 on them), the standalone signing key and
  `totp.key` (`auth.keys.allow_ephemeral_dev_keys` is one instance only), the
  captcha secret, and the version outside a rolling upgrade.
- **Background work:** every instance that runs workers takes River jobs, each
  job on one instance. River's leader alone schedules periodic work; a crashed
  leader is replaced within about 20 seconds, and deadlines survive leadership
  changes and restarts. A host fleet (`openrails.WithRiverClient`) must carry
  OpenRails' jobs on every replica. Outside River, every instance runs the
  Solana Pay poller (leased claims), checks its PSP credentials at start, on
  change and before the first provider write after an arming verdict is an hour
  old, and refreshes FX rates every 2 hours from its first cross-currency quote.
- **Caches:** catalog, merchants, write posture, the kill switch and access are
  read per request. Merchant configuration is cached per instance: an edit
  through OpenRails shows everywhere at once (`NOTIFY`), one made in Vault
  directly within 30 seconds, a file at the next start. An FX rate published 48
  hours ago is never quoted, and one more than 1.25 times (either way) the rate
  it replaces is held back until a later day's publication repeats it.
  `GET /v1/config` is cacheable for 5 minutes.
- **Scaling:** webhooks take effect once per provider event (a concurrent copy
  waits up to 10 s, then answers 500 so the provider retries); an idempotent
  request runs once per key (a retry elsewhere replays or answers 409). Each
  standalone instance works up to 20 billing jobs at once. Size PostgreSQL
  `max_connections` for every pool, plus one connection per instance with Vault
  configuration (it listens for edits).
- **Upgrades:** `New` and `run-server` migrate under an advisory lock before
  serving: one instance migrates, the rest wait. Migrations are additive, so the
  previous build runs on the migrated schema. `openrails migrate up` can run
  first as a deploy step; `openrails migrate status` compares the applied ledger
  with the binary's. From v1 every release upgrades any earlier v1 database in
  place ([compatibility](compatibility.md)).

## Safety levers

Declare `provider_write_mode` (`PROVIDER_WRITE_MODE`, `--provider-write-mode`)
explicitly; unset, it fails closed to `readonly`. Flags beat the environment,
which beats YAML. Every PSP declares an `environment`
([PSP environments](psp-environments.md)); omitted means its rail's real-money
one. The provider must agree before the PSP is used. A merchant may mix live and
test PSPs; test seams (`provider_sandbox`, `Deps` fakes) are refused beside a
live PSP. A non-live PSP relaxes no issuer, signing, storage or proxy
protection.

| Operation | `full` | `limited` | `readonly` |
|---|---|---|---|
| Customer purchase, card save, tier change, resume, refund | yes | yes | refused |
| Customer or staff cancel, and its provider delete | yes | yes | intent parks |
| Dunning charges and window-expiry cancellations | yes | intents park | no |
| Usage-order collection, Solana pulls | yes | no | no |
| `catalog apply` publishing a recurring Solana plan | yes | binds an existing plan, else refuses the price | same as `limited` |
| `catalog apply` local changes, provider reads, webhooks | yes | yes | yes |

`limited` stops what the system initiates; `readonly` stops the wire, enforced in
each rail's transport. Verified receipt recovery runs in every mode. The lower
of `provider_write_mode`, the merchant's posture and the book identity applies.
`engine_admission_hold: true` additionally pauses admission and first submission
of new engine payments; already-submitted ones are still verified.

Every subscription has one collector, fixed when it is created or imported
(`subscriptions.collection_policy`):

| Policy | Charges | Retries a decline |
|---|---|---|
| `engine` | OpenRails: a saved card (NMI, Stripe) or an on-chain pull (Solana) | OpenRails ([dunning](dunning.md)) |
| `nmi_schedule` | NMI, on its schedule | OpenRails |
| `provider` | Stripe or CCBill | the provider |

New subscriptions are `engine`; a rail or term OpenRails cannot collect (CCBill,
a trial phase) is refused before payment. Configuration never moves a
subscription to another collector.

### Booting against production credentials

Set the mode before the first start: imported `past_due` subscriptions are due
at once. For a cutover that needs review first:

1. Boot `limited` (the site works; system writes park) or `readonly`.
2. Let provider reads catch up. Dunning materializes in-window charges as parked
   `manual_rebill` intents, bounded by the dunning window, and parks
   window-expired cases `unverified`. It cancels nothing in this mode.
3. `openrails intents --merchant=<slug>` prints the drain forecast ("N execute
   under limited, M require full"). Resolve what should not fire.
4. Review `GET /v1/merchant/findings` and `openrails proposals list`. Raising
   the mode never touches findings.
5. Set `full`. Missed whole periods are never back-billed: stale dunning parks
   for verification, and a Solana subscription that skipped periods gets one
   pull.

## The kill switch

`billing.destructive_action_switch` is one row read before every destructive
act, so one `UPDATE` reaches every node at its next check. It ships off: a fresh
deployment makes no cancellation, revocation, provider delete or pull overwrite
until enabled. Detection, findings, additive repairs and notices run either way.
A held retraction stays `reconcile_required` with `held_by: kill_switch`; a
provider-confirmed cancel it holds parks the subscription `unverified` with access
kept, and the first Provider Refresh after enabling applies it.

```sql
-- stop everything now; parked intents resume when it is enabled again
UPDATE billing.destructive_action_switch SET enabled = false,
       updated_by = 'you', reason = 'incident: mass cancellation observed';

-- confirm
SELECT enabled, updated_by, reason, updated_at FROM billing.destructive_action_switch;
SELECT count(*) FROM billing.subscriptions
 WHERE canceled_at > (SELECT updated_at FROM billing.destructive_action_switch);
SELECT status, count(*) FROM billing.provider_intents
 WHERE intent_type = 'nmi_delete_subscription' GROUP BY status;
```

Workers log `retractions held — instance kill switch is OFF`.

**First-enforce gate.** A merchant with no `billing.merchant_destructive_policy`
row, or `enforce_armed_at IS NULL`, pulls in advisory mode: findings are stored,
nothing is changed, and `first_pull_completed_at` marks the survey ready. Review
it, then arm both halves:

```sql
SELECT finding_type, status, count(*) FROM billing.reconciliation_findings
 WHERE merchant_id = :merchant GROUP BY 1, 2 ORDER BY 3 DESC;

INSERT INTO billing.merchant_destructive_policy
       (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason)
VALUES (:merchant, true, now(), 'you', 'reviewed first-pull findings')
ON CONFLICT (merchant_id) DO UPDATE
   SET enforce_armed_at = now(), destructive_actions_enabled = true;
UPDATE billing.destructive_action_switch SET enabled = true, updated_by = 'you';
```

**Cancellation caps.** A pull-plane pass (pull, verification, unknown cohort)
may cancel at most `min(25, max(3, 5% of the PSP's live linked book))`
subscriptions per PSP, or the whole book when it has at most 5. Over that it
cancels **none**, halts the merchant's pass and raises
`pull.cancellation.capped`. The usual causes are a misdeclared `account_id`, a
credential rotated onto a sibling sub-account, or a provider returning a short
page.

## Commands

Every command takes `--config/-c` (default `config.yaml`) and
`--provider-write-mode`. One-off commands open only what they need, start no
server or workers, and keep standard output parseable.

| Command | Does |
|---|---|
| `run-server [--no-workers]` / `run-worker` | serve the API and work jobs (`--no-workers` is live but never ready) / jobs only |
| `migrate up`, `migrate pg`, `migrate status [--json]` | apply OpenRails' and River's migrations / report the applied ledger (non-zero on any mismatch) |
| `push-auth-bootstrap [--file] [--dry-run]` | create AuthKit root authority from a bootstrap manifest; first-run only |
| `push-merchant-config [--file] --insert` | create missing merchants from a manifest; bare, it only reports. `--overwrite`/`--prune` are refused |
| `get-merchant-config`, `apply-merchant-config --merchant NAME --file PATH` | read or apply merchant metadata by revision ([merchant configuration](merchant-configuration.md)) |
| `dump-merchant-config --slug`, `dump-merchant-catalog --slug` | export configuration / the active catalog as YAML |
| `catalog apply FILE --merchant NAME [--force-conflicts]` | apply a catalog document ([catalog](catalog.md)); startup never does |
| `catalog export` / `catalog import` | full catalog snapshot, IDs and history kept ([catalog export](catalog-export.md)) |
| `billing export` / `import` / `prepare-target` | move one merchant's billing ([portability](merchant-portability.md)) |
| `merchants list\|get\|create\|rename\|delete\|restore` | the merchant directory; delete is soft |
| `merchant arm`, `merchant hold`, `book status`, `book arm` | write posture and book identity (above) |
| `merchant purge --merchant NAME --by NAME [--inventory RUN --expect-rows N --confirm PHRASE]` | alone, record a purge inventory; with it typed back, delete the merchant's rows permanently (the kill switch must be on) |
| `admin-lockouts unlock <user-id>` | clear an administrator lockout |
| `destructive-holds list`, `destructive-holds clear` | review destructive-action incidents ([limits](destructive-actions.md)) |
| `intents`, `intents-log`, `intents resolve` | the provider intent ledger (below) |
| `prune`, `prune list`, `proposals …`, `undo-run` | [drift tools](#drift) |
| `ledger-audit --merchant=id:<uuid>` | check ledger conservation and counters; non-zero on a finding. There is no scheduled audit |
| `nmi decline-report --merchant --since [--until] [--psp]` | read-only NMI decline baseline by month and reason |
| `solana-pay resolve --merchant --signature --resolution` | close a reviewed Solana Pay receipt |
| `access-cutover preflight [--approve NAME]` | list and approve the access changes the product-access cutover migration makes |
| `workers [--json]` | job health from River's tables |

A private standalone first run, as an init job:

```bash
openrails push-auth-bootstrap --file /run/openrails/bootstrap.yaml
openrails push-merchant-config --file /run/openrails/merchants.yaml --insert
openrails catalog apply /run/openrails/catalog.yaml --merchant your-merchant
```

A server started with `--merchant-manifest` does the merchant step at every boot,
creating what is missing and keeping later API edits. With people signing in at
a trusted issuer, `push-auth-bootstrap` has nothing to do.

## Provider intents

Every outbound provider mutation is first a row in `billing.provider_intents`:
an idempotency key, an origin (`user`, `admin`, `system`), the PSP it targets and
a relevance window. Its own River job (`openrails.provider_operation`) claims it
under a lease, rechecks relevance, gates on mode × origin, executes and
classifies the outcome. Every attempt is appended to
`billing.provider_mutation_logs`. Covered: NMI deletes, NMI and Stripe refunds,
dunning `manual_rebill` charges, CCBill cancels, payment-method swaps, vault
deletes, NMI sales, Solana pulls and Stripe payment-method domain registrations.
A manual card removal is recorded too but never runs by itself: the person
retries it.

Statuses: `pending`, `in_flight`, `succeeded`, `failed_retryable`,
`failed_terminal`, `unknown_needs_verify`, `superseded`, `expired`. A blocked
intent (provider down, mode, unarmable credentials) stays pending with its
reason and drains when the blocker lifts; one past its relevance window is
superseded or expires instead of firing stale. Execution is effectively-once:
an ambiguous money movement is resolved by reading the provider before any
retry, deletes verify then execute, Stripe calls send `Idempotency-Key`.
Renewal identities are deterministic per merchant, PSP, subscription, period and
attempt; how each rail recovers an uncertain submission is in
[provider uncertainty](provider-uncertainty.md).

```
openrails intents [--status=active|succeeded|failed|superseded|expired|all] [--rail] [--type] [--merchant] [--format table|json]
openrails intents-log [--rail] [--intent] [--psp] [--phase=attempting|succeeded|failed|unknown|parked]
openrails intents resolve --merchant --intent [--step] (--receipt=<provider id> | --not-executed) --actor --reason
```

`intents` shows each row's `executes_under` mode and the drain forecast.
`resolve` closes an `unknown_needs_verify` operation from an exact provider
receipt, or from provider-supported non-execution (an empty NMI search is never
enough); it never resends.

Inbound durability is the provider's: webhooks are at-least-once and handlers
idempotent. There is no local inbound queue; Provider Refresh backfills what a
long outage loses.

### PSPs and credentials

`billing.psps` is operator-declared: one row per merchant account on a rail,
with an opaque `account_id`. A Stripe key is checked against it
(`GET /v1/account` in the posture probe and thin webhooks); a key of another
account is `mismatched` and never armed. Two live PSPs on a rail with the same
credential are one account declared twice: the later stays disarmed and
`consistency.duplicate_gateway_account` names it.

- **Rotate within a PSP:** `PATCH /v1/merchant/psps/{id}` (console **Rotate**)
  probes the new credential first (NMI, CCBill); a failure stores nothing and the
  old one keeps serving. A committed rotation reloads every replica at once.
  `expected_revision` refuses a concurrent edit (`409 revision_mismatch`). With a
  manifest, edit the file and restart.
- **Move to another account:** never repoint a PSP. Declare a new one and archive
  the old (`{archived: true}`, no provider call). An archived PSP takes no new
  sales; its subscriptions keep renewing there until they lapse.
- **Intents follow the PSP id, not its key.** Queued intents never execute
  against another account: they wait for the old PSP's credentials or expire.
  Renaming a key keeps the PSP. A key redeclared with another account registers
  a new PSP; the undeclared one keeps its records, its webhooks are refused, and
  while it holds cards or live subscriptions readiness reports it and
  `consistency.merchant_config.undeclared_account` stays open.
- **Custodians** (`billing.custodians`, Basis Theory) follow the same rules one
  axis over; a PSP names one with `custodian: <key>`
  ([payment-method custody](payment-method-custody.md)).

Adding or changing a PSP pulls nothing: [pull it](psp-import.md#pulling) with a
PSP import.

### Processor routing

Purchase picks one PSP before the attempt exists. A request naming `payment.psp`
gets it; otherwise the `psp_routing` setting decides (ordered rules, first match
wins, conditions `currency`, `product`, `price`, `mode`; a rule's `prefer` list
is ranking and allowlist; a rule with no conditions is the catch-all and comes
last). Without it: stripe, nmi, ccbill, solana, the first that can serve the
price. Country conditions are refused.

```yaml
psp_routing:
  - match: { currency: EUR, mode: subscription }
    prefer: [ccbill, nmi-main]
  - prefer: [nmi-main, ccbill, solana]
```

Fallback is availability-only and pre-charge (`not_armed`,
`credentials_missing`, `link_missing`, `mode_unsupported`,
`service_unavailable`, `ambiguous_selector`, `unknown_selector`,
`resolve_failed`): a decline never moves to a second processor.
`POST /v1/merchant/psps/routing-preview` returns the decision without charging.

## Convergence

`Converge(scope)` re-derives one customer, in a job committed with each mutation
that changed them, or a whole merchant after an applied prune. Writers commit a
change and its effects together, so there is no sweep; deadlines with no event
are indexed due work:

| Deadline | Picked up by | Result |
|---|---|---|
| a provider-billed `past_due` grace ends with no retry | hourly billing pass | `unverified`; the provider is read, access kept |
| a provider-billed renewal has no payment 48 h after its period | hourly billing pass | `unverified`, as above |
| a timed access window lapses | hourly health pass | access-ended notice |
| an engine renewal past its allowance, `unverified` past 72 h, a stuck or abandoned provider operation | health pass | `life.renewal.held`, `life.unverified.unresolved`, `life.provider_intent.stuck` / `.abandoned` |

A clean scope writes nothing. Repairs converge to the current state and never
replay side effects (three missed dunning months become one transition), while
grant effects are rebuilt from the grant log.

Findings are `<plane>.<subject>.<condition>` in
`billing.reconciliation_findings`:

| Plane | Checks | Repaired by |
|---|---|---|
| `pull.*` | a local mirror row against the provider | an approved proposal |
| `derive.*` | a grant effect against its source | replay or retract |
| `life.*` | a record against its clock and state machine | converge forward |
| `consistency.*` | duplicates, mismatches, unbooked events | data fix or review |

States: `reconcile_required`, `requires_review`, `auto_fixed`, `fixed`,
`ignored`. Identity is stable across runs and a finding resolves itself when the
divergence goes.

- **Confirmed absence.** Retractions from a pull wait until the domain
  (subscriptions, payments, grants) is proven fully reconciled for the merchant
  (`billing.reconciliation_state`, set after a complete mutating pull). During an
  import, "not local" means "not imported yet".
- **Stuck intents:** `pending` 24 h past its next attempt (one scheduled ahead
  or deferred waits for its instant), parked `pending` or `failed_retryable`
  over 24 h, `in_flight` / `unknown_needs_verify` over 2 h. Mode-parked ones are
  informational.
- **Unlandable provider subscriptions** park (`GET /v1/merchant/parked-subscriptions`)
  with `catalog.price.unmapped` (map or retire the price) or
  `life.subscription.unattributed` (link the customer). Mapping or linking wakes
  the park; released parks are deleted after 90 days.
- `consistency.ledger.unbooked` is a provider event OpenRails could not book;
  `life.solana.cranker_low_gas` a cranker below 0.05 SOL.

Findings raise console notifications and alerts ([events](events.md)).

## Provider Refresh

Every 2 hours (and on start) one refresh job per merchant with PSPs reads each
PSP in bounded windows from its watermark (`billing.psp_refresh_watermarks`) to
five minutes ago; NMI and Stripe start 24 hours early for late indexing. A
watermark advances only after a complete read. Before each Stripe window, a
reversals read takes the PSP's refunds and disputes alone, from the window start
or eight days ago, whichever is later, to five minutes ago: it reads no
subscriptions, cards or invoices, records only refunds that match a recorded
payment, moves no watermark and proves nothing about absence, so a book too
large for the full read still has its refunds recorded. Refresh records qualified
payments, refunds and provider-owned records, never writes to a provider and
never charges. Its unknown-cohort lane resolves `unverified` subscriptions with
the PSP that owns them:

| Provider evidence | Resolution |
|---|---|
| verified renewal charge | renewed; the charge recorded once |
| declined or stalled, inside the dunning window | `past_due`; dunning takes it |
| declined or stalled, past the window | canceled; the provider delete queued |
| no charge, remote alive with a future billing date | adopt the remote period end (never for `past_due`) |
| remote absent or terminal | canceled locally, access revoked |
| inconclusive or unreachable | stays `unverified` |

Reads run in every mode; overwrites, convergence and the cohort lane need the
merchant armed, the switch on and a posture above `readonly`. An auto-renewing
subscription's access is open-ended from creation and closes only on evidence; an
`unverified` one keeps access. Findings and refresh age report; they never gate
an operation, which still proves its own authority. NMI merchant-defined field 20
and Stripe `openrails_book` mark this book's objects: an unknown object without
the mark is a quiet, low finding (`evidence.quiet_foreign`).

## Drift

All manual, read-only or plan-only by default.

**Proposals.** Every difference a provider read could apply is a `pull.*`
finding with a `change`:

```
openrails proposals list    --merchant=<slug> [--psp=KEY] [--pull=pim_…] [--kind=K] [--source=pull|verification]
openrails proposals show    --merchant=<slug> <fnd_id>
openrails proposals approve --merchant=<slug> <fnd_id>… | --psp=KEY --kind=K --expect=N
openrails proposals dismiss|foreign --merchant=<slug> <fnd_id>… --notes=N
openrails proposals runs    --merchant=<slug>
```

Each id applies in its own transaction (1–100 per call; a filtered set needs
`--expect` equal to its size). One whose basis moved, or an end of access on
evidence older than 7 days, is `superseded`. The kill switch holds changes that
end access or write to a provider; a readonly posture holds those that move money
or write to a provider. A dismissed proposal returns only with new evidence.
`foreign` closes a `pull.subscription.missing` schedule that is not this
merchant's, refused while any local record links to it. Over HTTP:
`/v1/merchant/proposals`; approving and dismissing need a person or the in-process
host (`person_required`).

**Prune** retires the local subscriptions and payments a PSP no longer lists:

```
openrails prune --merchant=<slug> [--psp=<id|key>] [--rail=…] [--since … --until …] [--log-dir=…] [--expect-rows=N]
```

Without `--expect-rows` it is a plan; applying needs N equal to the plan, one PSP
at a time. Rows are soft-deleted and stamped with the run. An empty roster is
refused outright, grant-entangled rows are skipped, and NMI absence conclusions
are refused when the remote active set is implausibly small. It waits for the
merchant's pull lock and then converges the merchant.

**Undo.** `prune` and `approval` runs (approvals capture before-images) reverse
with one command; a `merchant_purge` run is refused by name:

```
openrails prune list --merchant=<slug>                                    # or proposals runs
openrails undo-run --merchant=<slug> --run=<id>                           # plan
openrails undo-run --merchant=<slug> --run=<id> --apply --expect-rows=N
```

An apply quiesces the merchant, supersedes the unfired intents the run queued,
restores the rows in the same transaction (a row changed since keeps its newer
state), soft-deletes the access windows the run closed and lets `Converge`
rebuild them, resets the proven-domain flags, and reports what it could not undo:
provider writes already sent, and in-flight ones left to their executor. It never
touches the ledger, grants or logs. Then re-read the PSP with an import; the
refresh stays advisory until the merchant is armed again.

## Background jobs

Everything runs under River once a server or worker is up. A periodic pass
catches up at start when its last retained run is older than its interval.

| Job | When |
|---|---|
| `openrails.billing_pass`: due renewals and retries, failed balance looks, arrears delinquency, order expiry (abandoned card saves included), automatic usage billing (opt-in), rebill watch, the daily NMI history read, undeclared-account re-alerts, stored-card refresh before renewal, provider operations an hour past due whose wake was lost; each scan in turn | top of each UTC hour, and on start |
| retention cleanup; merchant health; admission-denial flush (with Redis) | hourly |
| provider refresh (one job per merchant) | 2 h |
| Stripe webhook endpoint backstop | 24 h |
| a provider operation (`openrails.provider_operation`) | queued with its intent, sleeping to each attempt |
| a customer's convergence (`openrails.customer_converge`) | committed with the change |
| a balance's look | committed with a change crossing one of its lines, and at the instant its last look named |
| an operator alert (`openrails.operator_alert`), one delivery job per alert channel | committed with a destructive-action hold or a finding's notice |
| a PSP import, its charge attribution, a purge's Vault cleanup, a Stripe PSP's webhook setup | queued by that action |

A scan is a function the billing pass runs, not a job kind; usage billing alone
is also a job, for its page continuations and threshold wake-ups. The pass takes
everything due by its start, and one that starts while another still runs exits;
one scan's failure never stops another, and renewals wait at most an hour (an
engine renewal's allowance is at least two). It queues a wake again for a live
provider operation more than an hour past its due time, whose wake may be lost
(its River job deleted, its River schema reset, its fleet stopped); such an
operation gains at most one extra wake. An expired order is expired for every
read at once; the pass writes it, paging through each merchant's backlog, so
`order.expired` may follow up to an hour later. Jobs time out after 10 minutes,
an hour for `cleanup_expired_data`, `provider_refresh_merchant`,
`merchant_health` and an import's charge attribution, two for `psp_import`. The
billing pass runs each scan under its own budget, 10 minutes or an hour for the
rebill watch, the NMI history read and stored-card refresh (which stops opening
pages after 10 minutes), and takes their sum. River's rescuer returns a job whose
process died once it passes its timeout and `RescueStuckJobsAfter` (10 minutes).
Passes bounded per job (cleanup's 200 merchants, 200 usage accounts) continue in a
new job.

## Health and observability

- `GET /health/live` and `GET /health/ready`, public and detail-free (a failure is
  logged and answers 503). Readiness needs PostgreSQL, the merchants service, the
  River producer and a local River consumer; Redis, Vault, PSP posture and refused
  configuration are degraded, never failing. Embedded hosts use `Ready(ctx)` and
  register `Probes()`.
- On SIGTERM the server fails readiness at once, keeps serving for `drain_delay`
  (5 s), then stops within `shutdown_timeout` (20 s). Keep the sum under the
  orchestrator's grace period.
- `GET /metrics` on the private listener (`private_port`, or mount
  `PrivateHandler`): `openrails_dependency_up{dependency,class}` and, per job
  kind, `openrails_job_overrunning`, `openrails_job_retryable`,
  `openrails_job_discarded` and `openrails_job_last_completed_timestamp_seconds`. `/v1/metrics` is merchant business
  analytics ([metrics](metrics-for-llms.md)).
- `openrails workers` and the `openrails_job_progress` probe read River's tables:
  the probe fails with no leader, an overrunning job, or a billing pass that has
  not finished within a day or left critical work (renewals and retries, balance
  watches, delinquency, order expiry, usage billing) due since before the start
  of yesterday (UTC).
- Logs are structured to stdout (`logger.level`). Healthy is a near-empty
  `openrails intents`, no open findings and no overrunning or discarded jobs.

### Network egress

Besides configured hosts (database, Redis, Vault, SMTP, trusted issuers' JWKS,
`llm.base_url`):

| Destination | For |
|---|---|
| `latest.currency-api.pages.dev`, then `cdn.jsdelivr.net` | FX rates, every 2 h from an instance's first cross-currency quote; a failure fails only that quote. Both hosts serve one publisher's files, so neither checks the other; a rate is checked only against the one the instance already holds |
| `hermes.pyth.network` | Solana token prices |
| `api.stripe.com`; `secure.nmi.com`, `sandbox.nmi.com`; `api.ccbill.com`, `sandbox-api.ccbill.com`, `datalink.ccbill.com` | PSPs |
| `api.basistheory.com`, `cdn.basistheory.com` | Basis Theory custodians |
| `api.mainnet-beta.solana.com`, `api.devnet.solana.com`, `*.helius-rpc.com` | Solana RPC (`provider_sandbox.solana_rpc_url` replaces it) |
| `challenges.cloudflare.com`, `www.google.com`, `hcaptcha.com` | captcha, when set |
| `api.anthropic.com`, `api.openai.com` | LLM features, with `llm.api_key` and no `llm.base_url` |

### One API host and CORS

One API host serves every merchant: a credentialed request acts for its
credential's merchant ([auth](auth.md#merchant-binding)), a Solana Pay request
names its order, and provider callbacks land on
`<prefix>/v1/webhooks/{rail}/{account_id}`. `billing.ReservedMerchantSlugs` is an
advisory list for hosted products. OpenRails sets no cookies, so CORS is fixed
and not configurable: the standalone API routes and an embedded host's purchase
and self-service routes answer any origin (`Access-Control-Allow-Origin: *`,
`WWW-Authenticate` exposed, 12 h max-age, never credentials); the console,
webhooks, the server's AuthKit routes and embedded staff routes send no CORS
headers.

## Data retention

Periods are constants in `internal/retention`, the same for every merchant,
except the event log's `event_retention`. Each table's comment states its class:
**permanent** (the ledger, grants, payments, owed usage, receipts, operation
authorizations, refusals, running charges, maintenance runs and the before-images of
destructive runs),
**partitioned** (monthly partitions dropped whole), **rows** (deleted by the
hourly cleanup) or **state** (one row per thing that exists).

| Table | Deleted |
|---|---|
| `usage_events` (by `occurred_at`) | partition dropped 24 months after its usage was billed, about 26 months after the month began |
| `admission_operations` (by `admitted_at`) | partition dropped after 61 days |
| `subscription_status_transitions`, `subscription_billing_changes`, `provider_mutation_logs`, `payment_attempts`, `rebill_cycles`, `nmi_history_months` | 25 months |
| `provider_intents` | instruction-only intents (cancel, payment-method and source update, card vault, network token, catalog archive, account-updater request) 25 months after they last changed; the rest are permanent |
| `reconciliation_findings` | resolved ones, 12 months after resolved and last seen |
| `order_attempts`; `orders`, `order_lines` | attempts that never reached a provider, 90 days after `expires_at`; unpaid closed orders with no attempt, 90 days |
| `parked_provider_subscriptions` | 90 days after release |
| `carts` | 30 days after `last_changed_at` |
| `notification_events`, `host_outbox`, the event log | 90 days, never before `event_retention` (90 days by default) |
| `webhook_events` | 90 days after completion |
| `idempotency_keys`, `solana_pay_references` | at expiry, after the 7-day watch |
| `user_destructive_reservations` | 24 hours |
| `failed_usage_windows` | when the window ends |
| `meter_period_quantities` | 100 days after `period_starts_at` |
| `admission_denials_hourly`, `webhook_health_daily` | 13 months |
| `periodic_leases` | a kind unscheduled for 30 days |

An order attempt that reached a provider is permanent, and so is a cost
observation while its operation is open. The cleanup job creates partitions two
months ahead and drops expired ones (giving up after 2 s rather than queue behind
a long reader), then deletes 1,000 rows per statement and at most 50,000 per
merchant per pass. `subscription_status_transitions` refuses other deletes (`billing.guard_retention_delete`, keyed on the
`openrails.retention_table` setting).

What integrators see: a usage event may be up to 35 days old, and its `source` /
`source_id` key holds for those 35 days (a charged failed event's key holds for
good: its ledger debit carries it, and a later retry charges nothing); a hold lasts at most 30 days and a spend
window 31; after 61 days a request id reads as never admitted; usage reports reach
back about 26 months.

## Backup and recovery

Back up two things: PostgreSQL, and the merchant configuration (Vault with its
`credential_fingerprint_key`, or the manifest). PostgreSQL holds no credential, so
a database alone cannot charge anyone.

Turn on point-in-time recovery (`wal_level = replica`, `archive_mode = on`, an
off-host `archive_command`, periodic base backups); managed providers expose it,
but check the retention. PITR is whole-cluster: right for disaster, wrong for one
damaged merchant. A restore never undoes what happened at a provider (a charge, a
deleted vault, a cancel); it creates divergence that convergence then repairs.
Money is reversed by compensating entries, never deletion. The append-only tables
(`ledger_transfers`, `ledger_accounts`, `grants`, the status, billing-change and
mutation logs, `webhook_events`, findings) are restored whole or not at all;
access and balances are recomputed from the grant log.

1. Stop and drain the source writers and disable their restart.
2. Restore PostgreSQL; make the configuration available.
3. Start it. A restore into another cluster, database or schema stays readonly
   until `openrails book arm`, once the source is gone for good. Start in
   `readonly` to inspect first.
4. Review unresolved findings; ignoring one does not settle it.

Not backups: the purge inventory (counts and document names only,
`"is_backup": false`; after a purge, PITR is the only way back), a provider pull
(providers hold no access, grants or ledger) and `undo-run` (one operation).
Drill restores into a scratch instance: migrations current, PSPs arm, a pull
produces sane findings.
