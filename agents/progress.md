<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 732

---

# #698: merchant-config key `rail_merchant_accounts` → `accounts` (config/env surface only)

**Completed:** no
**Status:** IMPLEMENTED, uncommitted (2026-07-02) — openrails-side hard cut done; cross-repo lockstep bump
remains (operator edits below). Original rationale: shorten the operator-facing config key. Inside
`merchants.<slug>.` nothing else "accounts" could mean; the env var drops from
`BILLING_MERCHANTS_DOUJINS_RAIL_MERCHANT_ACCOUNTS_CCBILL_CCBILL_SECRETS_DATALINK_USERNAME` to
`BILLING_MERCHANTS_DOUJINS_ACCOUNTS_CCBILL_CCBILL_SECRETS_DATALINK_USERNAME`. DELIBERATE DIVERGENCE, named:
the DB table stays `rail_merchant_accounts` (#683 vocabulary uniformity, distinguishes rail_customer_accounts)
— operators type config keys, nobody types table names. Hard cut in config space, no aliases (house style).

## Metadata
- Category: config
- Status: implemented (uncommitted); cross-repo bump pending
- Passes: true (openrails side)

## Scope

- [x] YAML shape: `merchants.<slug>.rail_merchant_accounts.<key>.<rail>` → `merchants.<slug>.accounts.<key>.<rail>`
      — parser structs/tags (internal/bootstrap merchant manifest), validation messages, and REJECT the old key
      with a clear "renamed to accounts" error (hard cut, not silent-ignore — a manifest using the old key must
      fail loudly, not apply empty).
- [x] Env mapper anchor (internal/bootstrap/merchant_env.go): `RAIL_MERCHANT_ACCOUNTS` → `ACCOUNTS` as the fixed
      section token; keep the schema-aware single-underscore parsing property (ACCOUNTS is still a fixed anchor
      between the merchant key span and the account key span); update mapper doc examples + tests.
- [x] dump-merchant-config emits `accounts:`; round-trip load⇄dump tests updated.
- [x] Examples + docs sweep: config examples, .env.example (openrails has NO BILLING_MERCHANTS_* vars — nothing
      to change there; doujins/.env.example fix rides the host bump), docs/*, CLAUDE.md merchant-config mentions.
- [x] SECRET NAMES + DB UNCHANGED: `merchant_secrets` names keep the `rail_merchant_accounts/...` prefix and the
      table keeps its name — this issue is ONLY the koanf/YAML/env surface. State this in the parser comment so
      the asymmetry is documented, not rediscovered.
- [ ] Cross-repo lockstep: doujins (+ hentai0 if it grows a manifest) openrails-merchant.yaml key rename +
      .env var renames ride the next openrails release bump. Coordinate with #697's account_id dash edit —
      one operator YAML touch for both.
- [x] Sequencing: implemented concurrently with #697/#699 by agreement (2026-07-02); no conflicts — #697's
      ValidateRailAccountID call in bootstrap_manifest.go was preserved.

Acceptance: manifests and env overlays use `accounts`; the old key/anchor fails loudly with a rename-pointing
error; dump round-trips the new shape; DB/secret-name vocabulary untouched and the divergence documented;
hosts bumped in lockstep.

## Progress

- 2026-07-02 IMPLEMENTED (uncommitted). Changes:
  - `internal/bootstrap/merchant_manifest.go`: `MerchantConfig.RailMerchantAccounts` tags →
    `yaml:"accounts,omitempty" koanf:"accounts"` + divergence doc comment on the field; new
    `rejectRenamedMerchantConfigKeys` pre-check in `ParseMerchantConfigManifest` — old keys error with
    `merchants.<slug>.rail_merchant_accounts was renamed to accounts` (also rejects pre-#683
    `provider_accounts` the same way); `LoadMerchantConfigManifestBytes` calls
    `rejectRenamedMerchantEnvVars()` before loading the env overlay.
  - `internal/bootstrap/merchant_env.go`: section anchor `RAIL_MERCHANT_ACCOUNTS` → `ACCOUNTS`; doc examples
    updated; new `rejectRenamedMerchantEnvVars` — retired anchors (`RAIL_MERCHANT_ACCOUNTS`,
    `PROVIDER_ACCOUNTS`) are poison token sequences in any `BILLING_MERCHANTS_*` name and fail loudly
    (necessary: the single-token ACCOUNTS anchor would otherwise mis-split old vars into a wrong merchant
    key like `doujins-rail-merchant` and silently overlay undeclared config). Side effect, documented in
    code: a merchant key span itself may not contain those sequences (e.g. slug `provider` can't use env
    overlays).
  - `internal/bootstrap/bootstrap_manifest.go`: validation error paths `rail_merchant_accounts.…` → `accounts.…`.
  - `config/merchants_config.example.yaml` manifest key → `accounts:` (+ divergence comment); the top-of-file
    RUNTIME rail-set comment (`RailMerchantAccountSet` surface) deliberately untouched.
  - `config/config.go` retired-rails warn message → `merchants[].accounts`.
  - Docs: `docs/merchant-provisioning.md` (secret-prefix divergence note + manifest example modernized to
    current map shape with `accounts:`), `docs/e2e-mobius-sandbox.md` (stale pre-#683 example → current
    shape), `.claude/CLAUDE.md` (short-key + divergence note in the rail identity section). Secret-name docs
    (operations.md, vault-secret-ops.md) untouched on purpose.
  - Tests: mapper table updated to ACCOUNTS (incl. the CCBill datalink example); new
    `TestLoadMerchantConfigManifestBytesRejectsRenamedEnvAnchor` (both retired anchors);
    `TestParseMerchantConfigManifestValidationErrors` gained rename-rejection cases (verbatim messages);
    new `TestMarshalMerchantManifestEmitsAccountsKey` pins dump emission + round-trip.
  - Validation: `go build ./...` green; full `go test ./...` green; `go test -tags integration -count=1
    ./internal/bootstrap/...` green (64s, includes dump⇄load round-trip).
- OPERATOR EDITS AT THE NEXT LOCKSTEP BUMP (doujins; hentai0 has no manifest today):
  - `config/openrails-merchant.yaml`: rename `rail_merchant_accounts:` → `accounts:` under each merchant
    (same touch as #697's ccbill account_id dash edit).
  - `.env` / `.env.example`: rename `BILLING_MERCHANTS_DOUJINS_RAIL_MERCHANT_ACCOUNTS_*` →
    `BILLING_MERCHANTS_DOUJINS_ACCOUNTS_*` (e.g. `…_ACCOUNTS_CCBILL_CCBILL_SECRETS_DATALINK_USERNAME`,
    `…_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY`); any pre-#683 `…_PROVIDER_ACCOUNTS_*` spellings get the
    same treatment. Old spellings now fail boot loudly with a rename-pointing error instead of no-opping.

---

# #697: CCBill account_id format — `945280-0000` (dash), matching CCBill's own convention

**Completed:** no
**Status:** IMPLEMENTED, uncommitted tails only (2026-07-02) — dash form is canonical everywhere; migration 068
rewrites existing slash-form rows + secret names forward-only. Original note: CCBill itself writes account
identity as `clientAccnum-clientSubacc` with a DASH (e.g. `945280-0000`, seen in their own dashboard/support
conventions). OpenRails previously stored `945280/0000` with a slash.

## Metadata
- Category: config / data-integrity
- Status: implemented
- Passes: true

## Why (beyond matching the provider)

The slash is actively harmful in our own machinery:
- **Secret-name paths**: merchant secret names are slash-delimited
  (`rail_merchant_accounts/<rail>/<env>/<account_id>/<field>`); a slash INSIDE account_id makes CCBill secret
  names ambiguous to parse (`.../ccbill/live/945280/0000/salt` — which segments are the id?). The dash removes
  the only account_id that embeds the delimiter.
- **#662 deterministic UUIDs** cited "account_id contains `/`" as the delimiter-collision hazard forcing
  length-prefixed encoding — still keep injective encoding, but the hazard example disappears.
- Operator ergonomics: what you read in CCBill's UI is what you paste into the manifest.

## Scope (hard cut, no aliases — pre-launch discipline)

- [x] Canonical format: `<clientAccnum>-<clientSubacc>` (e.g. `945280-0000`). Validation on the ccbill
      rail block: reject `/` with a clear "use dash" error.
- [x] Where the composite is built/parsed in code: webhook auth/routing (clientAccnum+clientSubacc →
      account lookup), reconcile account bindings, catalog/config push + dump-merchant-config output,
      merchant-config parser, any Sprintf joining acc/subacc — grep `client_acc_num|ClientSubAcc|945280`
      and the `%s/%s` joins.
- [x] Forward-only migration: rewrite existing `rail_merchant_accounts.account_id` values for rail=ccbill
      (`s|/|-|`), plus `merchant_secrets`/`merchant_credential_audit` NAME segments containing the old form
      (pattern: 059's secret-name rewrite).
- [x] Config surfaces: doujins `config/openrails-merchant.yaml` (operator edits alongside the bump — see
      Progress), openrails examples, CLAUDE.md rail-identity note (`client_acc_num[/client_sub_acc]` → dash
      form), docs/glossary-rails.md if it spells the format (it doesn't; vault-secret-ops/merchant-provisioning/
      operations updated instead).
- [x] Tests: round-trip config load⇄dump with dash ids; webhook routing resolves the dash id; migration
      rewrites existing rows + secret names; validation rejects slash.
- [ ] Cross-repo: doujins manifest + any hentai0 manifest bump in lockstep with the release carrying this.

Acceptance: grep for `945280/0000`-style slash ids finds nothing outside historical tracker text; CCBill
account identity everywhere (DB, secrets, config, dump, webhook resolution) is `clientAccnum-clientSubacc`;
existing rows/secrets migrated forward-only; slash ids fail validation loudly.

## Progress

- 2026-07-02 — IMPLEMENTED (partly swept into 22daeff8; load-time manifest validation + round-trip/strict test
  tails still uncommitted alongside the #698 accounts-key work).
  - **Join/parse sites flipped to dash**: `ccbillWebhookAccountID` (internal/http/handlers/webhook.go — the ONE
    composite build site: `clientAccnum + "-" + clientSubacc`, no-subacc still → bare accnum) and
    `resolveScopedCCBillConfig` (internal/modules/checkout/merchant_rail_secrets.go — the ONE parse site:
    `strings.Cut(id, "-")`, both parts still required). Reconcile bindings / dump / secret names treat
    account_id as opaque — no other build/parse existed. Wire fields unchanged: clientAccnum/clientSubacc are
    still SEPARATE form/query fields to CCBill.
  - **Validation** (shared helper `config.ValidateRailAccountID`, error: `CCBill account_id uses a dash:
    clientAccnum-clientSubacc, e.g. 945280-0000`): enforced in `validateCCBillRail` (even in dev — format bug,
    not missing credential), in the manifest apply path (reconcileManifestRailMerchantAccount), and at manifest
    load time (validateManifestRailMerchantAccount in bootstrap_manifest.go).
  - **Migration 068** (`068_ccbill_account_id_dash.up.sql`, forward-only, 059 pattern):
    `rail_merchant_accounts.account_id` s|/|-| for rail='ccbill'; `merchant_secrets`/`merchant_credential_audit`
    names LIKE 'rail_merchant_accounts/ccbill/%' — canonical `%2F`-escaped 5-segment form (writer URL-escapes)
    gets `%2F`→`-` in the id segment, defensive raw-slash 6-segment form collapses the two id segments into one
    dash-joined segment. Non-ccbill rows/names untouched. Vault KV moves = operator step (none pre-launch).
  - **Docs**: .claude/CLAUDE.md rail-identity bullet, config/merchants_config.example.yaml, docs/vault-secret-ops.md
    (+ stale provider_accounts→rail_merchant_accounts prefix fixed), docs/merchant-provisioning.md,
    docs/operations.md. #662's "account_id contains `/`" hazard example lives only in ITS tracker spec (no code
    comment exists yet — DeterministicID not implemented); keep injective length-prefixed encoding when building it.
  - **Tests** (green): TestCCBillWebhookAccountIDIsDashJoined + msg tests (handlers);
    routes_merchant_webhook_integration_test resolves a dash-stored account from wire accnum+subacc;
    TestCheckoutCCBillSlashAccountIDRejected + dash fixtures (checkout);
    TestCCBillAccountIDRejectsSlash (config); TestParseMerchantConfigManifestRejectsCCBillSlashAccountID
    (load-time); TestReconcileMerchantManifestRejectsCCBillSlashAccountID (apply);
    TestMerchantConfigPushDumpRoundTrip extended with a dash ccbill account (push⇄dump⇄reparse verbatim);
    TestMigration068RewritesCCBillSlashAccountIDs (internal/merchants, real 068 file against Postgres: rewrites
    slash rows + both secret-name shapes in both tables, leaves canonical/non-ccbill untouched, parser round-trip).
  - **OPERATOR (doujins, rides the next lockstep release)**: one-line edit in doujins
    `config/openrails-merchant.yaml`: `account_id: "945280/0000"` → `account_id: "945280-0000"` (hentai0's
    manifest likewise if it declares ccbill). Deploy order: bump openrails (068 runs) + edit manifest together;
    old slash form now fails validation loudly at boot/push.
  - NOT done here (out of scope): ~/doujins/~hentai0 edits themselves; #662 DeterministicID.

---

# #696: CCBill subscription management API — per-subscription probe + merchant-initiated cancel

**Completed:** no
**Status:** IMPLEMENTED pending Phase 0 wire verification (2026-07-02) — Phases 1+2 coded against the
best-documented wire shape; Phase 0 (live probe) still blocked on DataLink credentials. See Progress below.
Original research note: CCBill's DataLink suite DOES have a per-subscription
API, `https://datalink.ccbill.com/utils/subscriptionManagement.cgi`, with per-DataLink-USER subsystem toggles:
`viewSubscriptionStatus` (read one sub's status/expiry by subscriptionId, `returnXML=1`) and
`cancelSubscription` (merchant-initiated cancel by subscriptionId). Two codebase premises are therefore FALSE:
`unknown_probe.go` ("CCBill has no per-record read API") and `user_service.go` `CCBillCancelError` ("CCBill does
not have a public API for merchant-initiated cancellation"). Product impact (Paul): users can cancel CCBill
subs from OUR account page and admins from OUR portal — no more routing people to CCBill's support portal.

EVIDENCE CAVEAT: sourced from convergent long-lived third-party integrations (s2member Pro, YourMembers, Magic
Members), not official docs (portal-gated). Per the NMI-v5 lesson (docs lie): TASK 0 IS A LIVE WIRE PROBE against
the real DataLink account before any code.

## Operator prerequisites (Paul, in the CCBill dashboard)

- CCBill Admin Portal -> Account Info -> Data Link Services Suite: create/inspect the DataLink user openrails
  will use; enable `viewSubscriptionStatus` + `cancelSubscription` subsystems (support enables if not visible).
  Subsystems are PER DATALINK USER, account-wide — never per subscription.
- Note: a cancel-enabled DataLink credential can cancel ANY subscription on the account. Our layers below
  (write-ahead intents, provider_write_mode transport gate, destructive-volume breaker) are the guardrails;
  if CCBill offers separate users per subsystem, consider a read-only user for pulls + a cancel-enabled user
  held for the write path.

## Phase 0 — live wire probe (before any code)

**PARTIAL RESULTS 2026-07-02 (read-leg attempted with real DataLink creds):**
- Endpoint + envelope CONFIRMED: POST `https://datalink.ccbill.com/utils/subscriptionManagement.cgi`
  (form-encoded) answers HTTP 200 with `<?xml version='1.0' standalone='yes'?><results>N</results>`.
- WIRE ASSUMPTION #5 WRONG: auth failure is NOT HTTP 401/403 — it is HTTP 200 + `<results>-7</results>`
  (bare code inside the XML envelope). Deliberately-wrong-password control returns the SAME -7, so -7 is the
  collapsed auth-rejection class (bad creds / IP not whitelisted / subsystem not enabled are
  indistinguishable from outside). Client's ErrDataLinkAuth detection must match results=-7, not HTTP status.
- `clientSubacc` vs `usingSubacc` still unresolved (both variants returned -7 pre-auth; re-test after auth
  works).
- BLOCKED on CCBill dashboard config: likely the DataLink user IP whitelist (this host = 70.92.85.180) and/or
  the SMS viewSubscriptionStatus subsystem enablement. Probe subscription ids picked from the dump: active
  218264501000000012 / 216124201000000978, expired 118177201000050535.


- [ ] curl `viewSubscriptionStatus` + (on a throwaway/expired sub) `cancelSubscription` against the real
      account; capture exact request params (username/password, clientAccnum/clientSubacc, subscriptionId,
      returnXML) and VERBATIM responses incl. error shapes (auth failure, unknown sub, already-cancelled).
      Record findings in this section — the response schema drives the client structs.
- [ ] While probing: check whether the SMS subsystem also exposes refund/void actions (would complete #692's
      cancel_and_refund for ccbill); note availability either way.

## Phase 1 — probe (read): CCBillSubscriptionProber

- [x] New choke-pointed client method(s) on `internal/integrations/ccbill` (same package as DataLink — ALL
      CCBill HTTP stays in the choke point): `ViewSubscriptionStatus(ctx, subscriptionID)` -> typed status
      (active/cancelled/expired + expiry date), honoring readonly transport discipline (a status view is a
      read — allowed under readonly). → `subscription_management.go` (ONE file owns the SMS wire shape).
- [x] `CCBillSubscriptionProber` in the #665 prober seam (`internal/reconcile/unknown_probe.go`): maps the
      response onto the standard probe->snapshot shape (roster entry with normalized status + NextBillingAt);
      wire into `BuildSubscriptionProbers` keyed off configured ccbill DataLink creds. CORRECT the false
      "no per-record read API" comment.
- [x] Unknown-cohort effect: bulk export remains primary; the prober covers rows the windowed export can't
      answer (and makes `verification_pressure` drain even between full exports). Integration test with a fake
      DataLink HTTP server (the NMI prober test pattern; the HTTP wire is the only permitted fake).
      → unknown_probe_ccbill_integration_test.go: parked-unknown ccbill rows resolve via the probe alone
      (adopt + cancel), zero mutations.

## Phase 2 — cancel (write): merchant-initiated CCBill cancellation

- [x] Choke-point client method `CancelSubscription(ctx, subscriptionID)` — a MUTATION: blocked at transport
      under `provider_write_mode=readonly` (ErrProviderReadOnly pattern, like NMI). → DataLinkClient.ReadOnly
      set from cfg.IsProviderReadOnly() in build_runtime, checked BEFORE any HTTP.
- [x] Durable-first (#674/#679): new rail-intent type `ccbill_cancel_subscription`, queued ALWAYS at the
      decision point (mode/credentials gate EXECUTION only); executor drains it; verify path confirms via
      `viewSubscriptionStatus` (ambiguity => verify-not-decline); ADD to the destructive-types set in
      `internal/intents/breaker.go` so the volume breaker covers it. → intents/ccbill_cancel.go
      (verify-then-execute like nmi_delete: already-not-rebilling IS success, no write sent).
- [x] User self-service cancel: replace the `CCBillCancelError` portal-redirect path — a user cancelling a
      CCBill sub now gets the SAME semantics as NMI: local cancel with paid runway (#691 closure at period
      end) + the durable remote-cancel intent. User-initiated => allowed under `limited` mode.
      → CCBillCancelError DELETED (type + pkg/service branch + the HTTP handler's hardcoded 422); ccbill
      rail descriptor CancelMode flipped external_portal -> destructive, CancelPortalURL removed.
- [x] Admin cancel: extend #692's `cancel_and_refund` executor ccbill branch from not-supported to the intent
      path (refund leg stays not-supported unless Phase 0 found a refund action — then wire it the same way).
      → cancel leg = ApplyLocalCancellation + admin-origin intent; refund leg errors clearly, naming the
      manual CCBill-portal path.
- [x] Webhook interplay: CCBill sends its own cancellation webhook when the cancel lands — verify the webhook
      handler treats the provider-confirmed cancel idempotently against our already-cancelled local row (no
      double revoke, no finding flap). → handleCancel was NOT idempotent (would overwrite cancel_type/
      cancelled_at + re-revoke + re-notify); now no-ops on an already-cancelled row; integration test added.
- [x] Lifecycle correctness: CCBill subs keep access through the paid period after cancel (cancel = stop
      rebilling, not kill access — CCBill's own semantics for cancelSubscription; confirm in Phase 0 whether
      it returns the final expiry). #691 runway closure uses that date. → runway closure uses the LOCAL
      period end (proof we already hold); the provider's expirationDate is recorded verbatim in intent
      result_evidence (provider_expires_at) — Phase 0 confirms the field before it is ever adopted locally.
- [x] Wire-pinning tests per the money-boundary test wall: known inputs => exact request wire; fake-server
      integration tests for cancel success/already-cancelled/auth-failure/unknown-sub; breaker integration
      test extended to the new destructive type. (Unknown-sub response shape is Phase-0-uncaptured; until
      then it surfaces as an ERROR — retried, never resolved off a guess.)

## Out of scope

- Refund execution if Phase 0 finds no refund action (stays operator-manual in CCBill's portal).
- Migrating the FlexForm checkout or webhook auth (unchanged).
- Solana admin-cancel (subscriber-signed by design — different problem).

Acceptance: with subsystems enabled and real credentials, (a) a parked-unknown CCBill sub resolves via the
per-sub probe without waiting for a bulk export; (b) a user cancels a CCBill sub on OUR site — local runway
cancel + durable intent + remote cancel executed + webhook-confirmed, never routed to CCBill's portal; (c) an
admin cancels via the #692 findings queue; (d) readonly mode blocks the mutation at transport, limited mode
still queues, the volume breaker counts CCBill cancels; (e) both false "no API" comments are gone.

## Progress

- 2026-07-02 — PHASES 1+2 IMPLEMENTED (uncommitted), pending Phase 0 wire verification. The ENTIRE SMS wire
  shape lives in ONE file — `internal/integrations/ccbill/subscription_management.go` (request form builder +
  response parser + error classification) — so post-probe adjustments are one-file; every assumption below is
  pinned by unit tests in `subscription_management_test.go` so a correction is a visible diff.
  PROVISIONAL WIRE ASSUMPTIONS PHASE 0 MUST CONFIRM:
  (1) endpoint POST `{base}/utils/subscriptionManagement.cgi`, form-encoded;
  (2) params `clientAccnum`, `clientSubacc` (OPEN QUESTION: s2member uses `usingSubacc` instead — confirm
      which the live gateway honors), `username`, `password`, `action`, `subscriptionId`, `returnXML=1`;
  (3) viewSubscriptionStatus returns XML with `subscriptionStatus` vocabulary "2"=active recurring,
      "1"=active non-recurring (no future rebill), "0"=inactive — unrecognized values are ERRORS by design;
  (4) expiry field name `expirationDate` (fallbacks tried: `expireDate`, `nextRenewalDate`) in
      YYYYMMDD / "YYYY-MM-DD[ HH:MM:SS]" / MM/DD/YYYY;
  (5) cancelSubscription success answer `<results>1</results>` (bare "1"/quoted accepted), definite reject
      = any other parsed results token (classified ErrCancelRejected => intent verifies, never declines);
  (6) auth failure = HTTP 401/403 or an "authentication failed"/"access denied"/"invalid username" text body;
  (7) UNKNOWN-SUBSCRIPTION response shape UNCAPTURED — currently surfaces as an ERROR (prober row stays
      unknown + retried; cancel intent retries); after Phase 0 captures it, map it to authoritative absence
      in ViewSubscriptionStatus (one function) so the prober can resolve deleted-at-provider rows.
  Build: choke point `DataLinkClient.{ViewSubscriptionStatus,CancelSubscription}` (+ ReadOnly transport gate
  from cfg.IsProviderReadOnly() in build_runtime); `reconcile.CCBillSubscriptionProber` wired into
  BuildSubscriptionProbers off the configured DataLink client (false "no per-record read API" comment gone);
  intent `ccbill_cancel_subscription` (intents/ccbill_cancel.go — handler + CCBillCancelScheduler, registered
  in river_register buildIntentRegistry, added to the breaker destructive set); user cancel path rewritten in
  user_service.go (CCBillCancelError deleted everywhere; ccbill descriptor destructive/no portal URL; the
  HTTP handler's hardcoded ccbill 422 removed — ccbill cancels queue the normal River cancel job); admin
  #692 executor ccbill branch = local cancel + admin-origin intent, refund leg = clear manual-portal error;
  ccbill Cancellation webhook now no-ops on an already-cancelled row.
  Tests (all green): unit wire-pinning (exact form encode, XML fixtures, status vocabulary, error classes,
  readonly-zero-HTTP) + prober verdict suite (fake DataLink HTTP) + integration (testcontainers PG):
  user cancel => runway cancel + atomic intent => executor drains against fake SMS; readonly parks w/ zero
  HTTP then drains on mode lift; limited executes user-origin; already-cancelled = idempotent success (no
  mutation); ambiguous (500) => unknown_needs_verify => verifier resolves via read; definite reject =>
  verify => "verified not executed" retry; reactivation supersedes; breaker holds ccbill cancels over budget
  w/ ONE held_bulk finding; parked-unknown ccbill rows resolve via probe end-to-end; admin findings approve
  cancels + queues + drains, refund leg 400s naming the portal; webhook-after-cancel no-op. Full
  `go build ./...` + `go test ./...` green; `-tags integration -count=1` green on ccbill, intents, reconcile,
  subscriptions, webhooks, handlers, rails, pkg/service. NOTE: also repaired two tests broken by the
  concurrent provider_write_mode fail-closed change (stripeapi TestNonReadOnlyAllowsWrites — bare config now
  expects blocked; catalog stripe_webhook_registration_test — explicit ProviderWriteModeFull).

---

# #662: deterministic natural-key UUIDs for products, prices, provider accounts (replace random NewV7)

**Completed:** no
**Status:** PLANNED (2026-07-01) — not started. Design settled: derive each entity's uuid PK deterministically
(uuidv5) from its IMMUTABLE natural key instead of `uuidutil.NewV7()`. Keep the uuid column type — this is
"encode the natural key as a uuid", NOT a text-PK conversion (that was evaluated and rejected, see Out of scope).

## Metadata
- Category: catalog
- Status: planned
- Passes: false

## Problem
Product, price, and provider-account primary keys are random UUIDv7 (`uuidutil.NewV7()`), disconnected from each
entity's natural key. Consequences:
- The same logical product/price/account gets a DIFFERENT uuid in every environment and every fresh DB rebuild.
- Seeding is idempotent only WITHIN one DB (via get-or-create by the natural key), never reproducible across DBs.
- The id can't be computed without a DB lookup — e.g. doujins legacy-migrate must look up the shared premium price
  id (`resolveSharedLegacyPremiumPrice`) rather than compute it and stamp it on 40k subscriptions.
- The uuid is a second, arbitrary identity when a deterministic one derivable from the natural key is strictly
  better and costs nothing extra.

All three entities ALREADY have an immutable natural key with a unique constraint:
- products: `UNIQUE (merchant_id, key)`; `key` immutable by contract (changing it = a new product).
- prices: `unique_prices_product_amount_window (product_id, amount, currency, access_duration_hours, auto_renew,
  trial_unit_amount, trial_duration_hours)`; prices are immutable on financial fields — a reprice creates a new row
  and archives the old (`PriceService.Update` errors "prices are immutable"; converge re-mints via
  `matchPrice`/`PriceCreate`/`PriceArchive`).
- payment_provider_accounts: natural key `(rail, environment, account_id)`, global.

## Target design
Add `uuidutil.DeterministicID(namespace uuid.UUID, parts ...string) uuid.UUID` = uuidv5 over a canonical,
injective encoding of `parts` (length-prefixed join, so no delimiter-collision — `account_id` contains `/`), under
ONE fixed package-level namespace constant that must never change (changing it re-mints every id).

Mint catalog/provider ids from the immutable natural key:
- `product.id  = DeterministicID(ns, merchant_id, key)`
- `price.id    = DeterministicID(ns, product_id, amount, currency, access_duration_hours, auto_renew,
  trial_unit_amount, trial_duration_hours)` — exactly the `unique_prices_product_amount_window` columns.
- `provider.id = DeterministicID(ns, rail, environment, account_id)`

EXCLUDE every mutable field from the derivation — `rails` (rotated in place by `UpdateRails`), `status`,
`display_name`, timestamps. Deriving from a mutable field would orphan FK references when it changes. The uuid
becomes a content-hash of the entity's frozen identity: same key → same id everywhere; a change to identity (a
reprice) correctly hashes to a NEW id while the old row (still referenced by subscriptions/payments) is untouched;
collisions are impossible because the unique constraint already forbids two rows with the same tuple.

## Tasks
- [ ] Add `uuidutil.DeterministicID(namespace, parts...)` (uuidv5; canonical length-prefixed encoding; document that
      the namespace constant is permanent). Unit-test injectivity + stability.
- [ ] Product create path: replace `NewV7` with `DeterministicID(ns, merchant_id, key)`. Now that the id is
      computable, the create can be `ON CONFLICT (id) DO NOTHING/UPDATE` (simpler than the GetProductByKey-then-Create
      dance) — confirm re-seed stays idempotent.
- [ ] Price create path (`pkg/service/service_definition_catalog.go:490`): replace `NewV7` with `DeterministicID`
      over the frozen tuple. Canonicalize each field the SAME way the unique key / `matchPrice` compares them
      (amount as int64, currency lowercased, nullable trial fields → a stable sentinel) so equal prices always hash
      equal and never violate `unique_prices_product_amount_window`.
- [ ] Provider-account create/upsert path: replace `NewV7` with `DeterministicID(ns, rail, environment, account_id)`,
      canonicalized to match the `(rail, environment, account_id)` unique index (`lower(rail)` etc.).
- [ ] Structurally enforce price money-immutability: split the raw sqlc `UpdatePrice` (`internal/db/queries/prices.sql`)
      into status-only + rails-only queries so the money/identity columns are not settable at the DB layer (today they
      are immutable only by caller convention — `PriceService.Update` blocks it, but the query text can still SET them).
- [ ] Tests: (a) same natural key → same id across two fresh seeds; (b) reprice → new id, old row archived/untouched;
      (c) provider-account re-import → same id; (d) product re-seed → same id; (e) mutating a mutable field
      (`rails`/`status`/`display_name`) does NOT change the id.
- [ ] Rollout note: NO schema change (uuid type unchanged; FKs/RLS/sqlc untouched). Existing DBs keep their random
      ids — get-or-create won't re-mint — so adoption is via fresh seed (doujins is pre-cutover / re-seeding, so it
      picks up deterministic ids with no backfill). A prod backfill to deterministic ids is a SEPARATE, explicitly
      scoped migration (rewrite FKs) and is NOT in this issue.

## Out of scope (named and rejected)
- Converting any PK to a text or composite natural key. Natural keys here are composite (products `(merchant_id,
  key)`; provider `(rail, environment, account_id)`; price 7-col) — a text/composite PK propagates into ~25 child FK
  columns + every index + RLS join + sqlc type, `account_id` carries a `/`, and it breaks openrails' uniform
  single-column-uuid keying. A deterministic uuid gives the SAME identity semantics at ~zero blast radius.
- Adding a price `lookup_key`. Unnecessary: prices are immutable, so the frozen attribute tuple is already the
  natural key.
- Including `rails` (or any mutable field) in the derivation — would orphan references when a provider link rotates.
- Backfilling deterministic ids onto existing prod rows (separate migration if ever wanted).

Acceptance: product/price/provider-account ids are a pure, stable function of each entity's immutable natural key —
identical across environments and fresh rebuilds, computable without a DB read; a reprice mints a new id (old row
archived, untouched); rotating a provider link (`rails`) or flipping `status` leaves the id unchanged; no
FK/RLS/sqlc/type churn; the raw `UpdatePrice` can no longer SET money/identity columns.

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account NMI recurring test has been added and compiles/skips locally because `NMI_SANDBOX_SECURITY_KEY` is not set. Remaining work: run real NMI credential test and repair/replay Paul2 after fixed code is deployed.

Fix the NMI subscription checkout path where an immediately approved recurring checkout is persisted as pending, leaving host apps such as Doujins stuck on the pending-subscription screen even though NMI accepted the transaction.

## Metadata

- Category: bug
- Status: in_progress
- Passes: false

## Live findings from Doujins/Paul2 on 2026-06-08

- OpenRails received the checkout request and `POST /v1/self/checkout` returned HTTP 200.
- NMI accepted the card vault and transaction: payment vault `314825442`, provider transaction `12162933364`, provider subscription `12162933429`.
- Local checkout session `019ea96b-221b-7274-a341-8cdc85cb72d6` was marked `succeeded` with processor `mobius` and amount `2300 usd`.
- Local subscription `019ea96b-223b-75b1-926b-35956d10eba5` stayed `pending`, with no current period start/end timestamps.
- Local payments only had a pending attempt row keyed as `nmi_sub_attempt:sub_cb204b034d2c3ec46be93b0470ff44df`; there was no completed payment row keyed by the real NMI transaction id.
- No billing entitlement rows were created for the user, so Doujins correctly kept showing the pending-subscription state.

## Root cause

`processNMISubscription` called `AddRecurringSubscription` successfully, but `completeNMISubscriptionRegistration` intentionally created a local pending subscription and returned a pending checkout response. The initial NMI response was not used to synchronously activate an immediate subscription, set the current billing period, create a completed payment, or grant entitlements. Webhooks should not be required for the initial happy path when NMI immediately approves the transaction; delayed/future starts can remain pending.

## Desired behavior

For immediate NMI subscription approvals, OpenRails should finish the local checkout atomically from the direct provider response: mark the subscription active, set current period timestamps, record the completed payment against the real provider transaction id, grant entitlements, and return a succeeded checkout response. Delayed-start subscriptions and genuinely asynchronous provider states should remain pending and rely on follow-up provider events/reconciliation.

**Tasks:**
- [x] Capture live-stack evidence for the Paul2 failure: NMI accepted the transaction, checkout succeeded, subscription stayed pending, payment stayed as a pending attempt, and no entitlement was granted.
- [x] Identify root cause in the NMI checkout finalization path.
- [x] Patch immediate NMI subscription finalization to activate the subscription, set period dates, create the completed payment, and grant entitlements without waiting for a webhook.
- [x] Preserve pending behavior for delayed/future-start NMI subscriptions.
- [x] Fix stale integration-test helpers that still query billing tables by `user_id` instead of `tenant_subject_id`.
- [x] Add/validate focused integration coverage proving an immediate NMI subscription checkout grants the premium entitlement synchronously.
- [x] Add an actual NMI test-account integration path so the live provider contract is exercised, guarded by NMI test credentials.
- [ ] Run the actual NMI configured-account recurring-subscription integration with `NMI_SANDBOX_SECURITY_KEY` set.
- [x] Run focused OpenRails checkout/module tests and NMI regression tests.
- [ ] After deployment/restart, repair or replay the affected live pending subscription row for Paul2 if it still exists.

---

# #111: admin-rate-limiting

**Completed:** no

Add rate limiting to admin endpoints to limit blast radius of compromised JWT

## Metadata

- Category: security
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] PROBLEM: If admin JWT is leaked, attacker could cancel/refund thousands of users before detection
- [ ] Implement per-admin-user rate limiting using Redis (keyed by admin user ID from JWT)
- [ ] Define rate limits for destructive operations:
-     - Cancellations: 5/minute, 10/hour, 50/day
-     - Refunds: 5/minute, 10/hour, 50/day
-     - Entitlement revocations: 5/minute, 10/hour, 50/day
- [ ] Define rate limits for bulk/expensive operations:
-     - Extend operations: 3/minute
-     - Off-channel payments: 10/minute
-     - Admin grants: 10/minute
- [ ] On rate limit exceeded: lock out admin for extended period (e.g., 1 hour) and alert
- [ ] Add alerting/notification when rate limits are approached (e.g., 80% threshold)
- [ ] Create admin rate limit middleware that wraps destructive endpoints
- [ ] Allow super-admin or manual override to unlock rate-limited admin if legitimate
- [ ] Log all rate limit events for security audit
- BENEFIT: Limits blast radius - attacker can only affect ~5-10 users before getting locked out

---

# #158: ccbill-dunning-grace-entitlements

**Completed:** no

Adjust CCBill subscription entitlement logic to model paid term + retries (dunning) + end-of-term expiration. Use CCBill webhook fields like nextRetryDate/nextRenewalDate to drive subscription status and finite entitlement windows, and cap any grace extensions to avoid unlimited free access.

**Tasks:**
- SPEC:
- [ ] Verify CCBill webhook sequence in sandbox: cancel mid-term -> Expiration at end-of-term (and timing)
- [ ] Decide grace policy during retries: disabled vs enabled; cap strategy (extend-to-nextRetryDate vs fixed extra days)
- [ ] Decide policy for CCBill `Cancellation.source=failedRB`: treat as terminal end (no more retries) vs still paid-through end-of-term
-
- DATA MODEL:
- [x] Ensure we can store current_period_ends_at (from nextRenewalDate) for CCBill subscriptions
- [x] Store next_retry_at (from nextRetryDate) and optional grace_ends_at (policy derived)
- [x] Decide whether to reuse existing `subscriptions.next_retry_at` fields for CCBill (recommended) vs add CCBill-specific columns
-
- WEBHOOK -> STATE MACHINE:
- [x] NewSaleSuccess: set paid-term end; grant entitlements for [now, paid_term_end)
- [x] RenewalSuccess: extend paid-term end; extend entitlement windows
- [x] RenewalFailure: set past_due/dunning; persist next_retry_at from webhook; optionally extend entitlements within grace cap
- [x] Cancellation: mark cancelled but keep access until paid_term_end (do NOT revoke immediately)
- [x] Expiration: mark ended/expired and end/revoke entitlements
-
- CODE CHANGES (expected touch points):
- [x] `internal/services/webhook_ccbill.go`: parse and apply `nextRenewalDate` on `NewSaleSuccess` and `RenewalSuccess` (don’t rely solely on `price.billing_cycle_days`)
- [x] `internal/services/webhook_ccbill.go`: on `RenewalFailure`, parse `nextRetryDate` and update `subscriptions.status=past_due` + `subscriptions.next_retry_at` (avoid calling `SubscriptionLifecycleService.FailMembership`, which schedules NMI-style retries)
- [x] `internal/services/webhook_ccbill.go`: on `Cancellation`, call `CancelMembership` with `RevokeAccess=false` (today it passes true)
- [x] `internal/services/lifecycle_service.go`: update `CreateMembership` entitlement grant behavior for CCBill to create finite windows ending at `current_period_ends_at` (instead of indefinite `end_at=NULL`)
- [x] `internal/services/lifecycle_service.go`: update `RenewMembership` for CCBill to extend entitlements to match the new `current_period_ends_at` (today renewal doesn’t extend entitlements at all unless a downgrade changes entitlement set)
- [x] `internal/db/repo/entitlement.go`: make `IsEntitled` / “active entitlement” queries explicitly exclude `deleted_at` (don’t rely on implicit soft-delete behavior)
- [x] `config/config.go`: add a feature flag / config for CCBill grace behavior (enable/disable + max cap)
-
- ANALYTICS:
- [x] Ensure ClickHouse subscription_events reflect dunning/cancelled/expired transitions for CCBill
-
- TESTS:
- [x] Add integration test: NewSaleSuccess -> RenewalFailure(nextRetryDate) keeps access until paid_term_end (+ optional grace) -> RenewalSuccess extends window
- [x] Add integration test: Cancellation mid-term does NOT revoke immediately; entitlement ends at paid_term_end; verify Expiration handling if CCBill sends it
- [x] Add unit test for `EntitlementRepo.IsEntitled` to ensure deleted rows are excluded (regression guard)

---

# #164: cloudflared-managed-dev-tunnel

**Completed:** no

Make `Config.Cloudflared` an actual supported dev feature: OpenRails can (optionally) run and manage a Cloudflare Tunnel for local/dev, so a full local stack (e.g., multiple host apps + billing) can be accessed from an external device (phone) over HTTPS.

## Metadata

- Category: devx
- Status: planned
- Passes: false

## Goal

- With `cloudflared.tunnel_token` configured, a developer can start OpenRails and get a stable public hostname that routes to the local billing instance, usable from a phone browser/app.

## Non-goals

- Do not require Cloudflared in production.
- Do not make Cloudflared a hard dependency for boot.

## Notes

- This is for local/dev convenience (public ingress), not webhook signature bypass.
- Prefer explicit opt-in (config flag or separate command) so production never spawns subprocesses unexpectedly.

**Tasks:**
- DESIGN:
- [ ] Decide opt-in mechanism: (A) standalone CLI subcommand `openrails cloudflared up` that supervises both OpenRails + cloudflared, or (B) OpenRails spawns cloudflared when `cloudflared.tunnel_token` is set and `env=dev`
- [ ] Decide what constitutes “success”: tunnel established + /health/ready reachable via public hostname
-
- IMPLEMENTATION (if spawning subprocess):
- [ ] Add a small `pkg/cloudflared` supervisor that starts `cloudflared tunnel run` with the configured token and captures stdout/stderr into structured logs
- [ ] Ensure clean shutdown: propagate SIGINT/SIGTERM, kill child process group, avoid zombies
- [ ] Add a readiness check that probes the configured `cloudflared.public_hostname` (or local route) and reports status
-
- CONFIG / DOCS:
- [ ] Clarify config semantics in config.example.yaml (token is secret; hostname is non-secret; tunnel name optional)
- [ ] Document a phone-access workflow in docs (prereqs: cloudflared installed or image available; set APIURL/CORS origins appropriately)
-
- SECURITY / SAFETY:
- [ ] Ensure the service API (port 8060) is not accidentally exposed publicly by default; only expose the public API unless explicitly configured
- [ ] Ensure `api_key` and auth verification are still required for protected routes
-
- VERIFY:
- [ ] Add a lightweight unit test for supervisor command construction (no subprocess exec)
- [ ] Manual dev verification: start stack + tunnel and hit /health/ready from an external device

---

# #288: processor-routing-and-fallback-policy

**Completed:** no

Add deterministic processor selection and fallback policy so checkout can choose the best available rail for a merchant/product/tier/user context before creating a one-processor checkout session.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Motivation

- Hyperswitch wins on payment orchestration. OpenRails should not chase broad smart-routing, but customers do need basic processor redundancy and product/merchant-aware routing, especially high-risk merchants with Stripe plus NMI/CCBill/Solana.
- Keep checkout sessions one-processor-at-a-time; route before session creation.

## Non-goals

- No ML routing.
- No 200-connector orchestration platform.
- No automatic retry to a second processor after a successful authorization attempt unless the processor contract makes that safe.

**Tasks:**
- DESIGN:
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, non-archived provider-account eligibility, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > merchant policy > product/tier_group policy > global default.
- [ ] Decide failure classes that can trigger fallback before checkout finalization: processor unavailable, unsupported capability, credential missing, sandbox/live mismatch, hard validation failure. Do not fallback after a successful charge.
-
- DATA MODEL / CONFIG:
- [ ] Add routing policy representation in DB or catalog manifest: allowed processors, preferred order, archived-account exclusion, and optional per-tier overrides.
- [ ] Extend catalog-as-code manifest only if needed; prefer using existing provider lists as the first version.
- [ ] Record selected processor and routing reason on checkout_sessions for auditability.
-
- IMPLEMENTATION:
- [ ] Add a ProcessorRouter service used before checkout session creation and legacy checkout flows.
- [ ] Integrate with processor health/capability metadata so unsupported processors are filtered with explicit errors.
- [ ] Add dry-run endpoint/CLI to explain routing for a price/user/merchant without creating a checkout.
- [ ] Ensure idempotency keys bind to the resolved processor/policy so retries do not unexpectedly switch rails.
-
- VERIFY:
- [ ] Unit-test policy precedence and fallback filtering.
- [ ] Integration-test Stripe primary -> NMI fallback when Stripe is disabled/unavailable before charge.
- [ ] Integration-test product constrained to NMI does not route to Stripe even if NMI is configured.

---

# #290: provider-certification-matrix

**Completed:** no

Publish and maintain a provider certification matrix for Stripe, NMI, CCBill, and Solana that records exactly which customer-visible flows are supported, sandbox/devnet tested, live/test-mode tested, and known-limited.

## Metadata

- Category: product
- Status: planned
- Passes: false

## Motivation

- OpenRails' strongest differentiator is deep support for real non-Stripe rails, especially NMI-compatible high-risk gateways. Customers need confidence that the specific flows they care about actually work.
- This should become both documentation and an executable certification harness where practical.

**Tasks:**
- MATRIX DESIGN:
- [ ] Define provider capabilities and certification statuses: not_supported, manual_only, unit_tested, integration_tested, sandbox_certified, live_test_mode_certified, devnet_certified, production_certified.
- [ ] Define flows to track: catalog/product push, price/recurring-plan push, one-time checkout, recurring checkout, vault/tokenization, rebill, cancellation, deferred cancellation, refund, dispute/chargeback, webhook handling, subscription sync/backfill, catalog drift detection.
- [ ] Include processor-specific notes: NMI product/prices are local while recurring prices push as NMI recurring plans; CCBill catalog actions may be manual; Solana recurring requires on-chain readback/devnet certification.
-
- DOCS:
- [ ] Add docs/providers.md or equivalent with the current matrix and exact tested commands.
- [ ] Add NMI-compatible gateway guidance: required security_key, Collect.js/tokenization key if needed, direct/query endpoints, test_mode behavior, and how white-label NMI accounts map to the same interface.
- [ ] Add troubleshooting for common provider failures: bad endpoint, key belongs to different gateway user, sandbox URL not supported, recurring plan query mismatch, webhook signature failure.
-
- EXECUTABLE CERTIFICATION:
- [ ] Add or formalize focused integration tests for NMI sale, vault, recurring plan create/readback, and query API.
- [ ] Add Stripe test-mode catalog command + subscription sync certification steps.
- [ ] Add Solana devnet read-back certification for plan accounts and recurring lifecycle.
- [ ] Add CCBill manual-action verification path so unsupported remote catalog operations surface as pending_manual_actions rather than errors.
-
- PROCESS:
- [ ] Make certification matrix updates part of provider-related PRs.
- [ ] Record last verified date, environment, and command for each provider flow without exposing secrets.

---

# #702: [DB] drop 7 dead tables (2026-07-02 pre-lock audit)

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — all 7 tables dropped from the 0001 baseline
(full blocks incl. RLS/policies/grants); `entitlement_features.sql` queries, FeatureService + EntitlementFeatureRepo +
models deleted; `GET /self/entitlements/active` now reads EntitlementService.ListActiveRecords directly and returns
{id, customer_id, lookup_key, start_at, end_at, source_type, source_id} — no Feature object. Anchors test and
credential-audit shims removed; schema guard tests updated. Dev DBs need a reset (baseline edited in place).

- `catalog_credit_purchases` — superseded by `catalog_credit_balances` + `catalog_credit_purchase_prices`
  (live quote path `internal/modules/money/credit_purchase.go` and manifest push `pkg/service/catalog_sidecars.go`
  use only the new pair). Survived the squash by accident.
- `customer_anchors` + `merchant_anchors` — #591 platform-anchor design whose runtime never landed; only
  usage is one insert-and-count integration test (`catalog_platform_sidecars_integration_test.go`).
- `bootstrap_state` — zero references anywhere.
- `merchant_credential_audit` — feature deliberately removed as platform slop (future.md records
  "do not recreate"); no INSERT exists; the table was forgotten. NOTE: it was an append-only credential
  put/rotate/delete/test *audit trail*, not a credentials-work checker — dropping it removes nothing live.
- `entitlement_features` + `product_entitlement_features` — #245 Stripe-shaped feature registry; full CRUD
  (`internal/modules/entitlements/feature_service.go`) has NO route caller, production tables permanently
  empty; the Stripe Feature mirror (#586) reads `product.entitlements_spec` strings directly. Drop tables +
  FeatureService CRUD + queries; simplify `GET /self/entitlements/active` (already treats missing feature
  rows as fine).

Also update `migrations/postgres/merchant_aware_schema_test.go` merchantOwnedTables and drop the
lifecycle-test CREATE TABLE shim for merchant_credential_audit.

---

# #703: [DB] drop dead / unenforced columns + dead enum values

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — all listed columns dropped from the baseline +
queries + Go (money_settings ten incl. deleting suspension.go; webhook_events.status AND .rail — no reader, op
embeds the rail; checkout_sessions.idempotency_key — model field kept request-scoped for Redis idempotency;
rail_merchant_accounts.owner; invoices.sent_at). Judgment calls: invoices.collection_method KEPT with CHECK
narrowed to 'charge_automatically'; tier_schedules.owner dropped ENTIRELY (constant 'platform'; removed from both
unique indexes); merchant_exports lost `location`, status kept with CHECK narrowed to 'completed'.
GetPaymentMethodByInitialTransactionID query+service+repo deleted. Self/customer settings PUT surface shrank
(max_spend_per_day/month fields gone). Dev DBs need a reset.

- `money_settings`: ten columns — `max_spend_per_day`, `max_spend_per_month`, `max_outstanding_owed_amount`,
  `hard_stop_on_breach`, `alert_threshold_pct` (admission reads only billing_mode + credit_limit_amount +
  ledger balance; real caps live in payer/invoker_spend_limits), `outstanding_owed_amount` (never UPDATEd,
  always 0 — API derives from open invoices), and the suspension quartet `verified_payment_method` /
  `verified_at` / `suspended_at` / `suspend_reason` (whole `internal/modules/money/suspension.go` surface has
  zero production callers; schema comments admit "legacy ... not consulted"). Drop all ten + suspension.go +
  queries, or explicitly decide to enforce them. Live in the same table (keep): billing_mode, currency,
  credit_limit_amount, tier/tier_source, low_balance_threshold, auto_topup_*, last_alert_at, last_topup_at,
  default_credit_expiry_hours.
- `webhook_events.status` — CHECK-constrained to exactly one value ('completed'); inserts omit it. Drop
  column + CHECK. `webhook_events.rail` is write-only (op key already embeds the rail) — keep only if wanted
  for ops SQL.
- `checkout_sessions.idempotency_key` — written, never read, no unique index (real idempotency is Redis).
  Drop, or give it the partial unique index and make it the constraint.
- `rail_merchant_accounts.owner` — never written (always default 'merchant'), never branched on; 'platform'
  is the not-built platform-vault future. Drop, reintroduce with the feature.
- `invoices`: `sent_at` never set; `collection_method='send_invoice'` never written. Dead column + dead enum arm.
- `tier_schedules.owner` — 'subject' never written (graduation writes only 'platform'); dead enum value
  widening two unique indexes.
- `payment_methods`: `GetPaymentMethodByInitialTransactionID` read path has no production caller — drop the
  query/service method (column itself is Stripe-alias provenance, keep).
- `merchant_exports` — trim to what the export-before-delete gate uses: `location` never written; statuses
  'pending'/'failed' never occur.

---

# #704: [DB] rail_merchant_account_id provenance: stamp-or-drop decision (legacy lanes)

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — rail_customer_accounts: column + FK + 3 dead
indexes DROPPED; the `_legacy` uniques (the only live lane) renamed to uq_rail_customer_accounts_customer_rail /
uq_rail_customer_accounts_merchant_rail_customer (now full uniques, no WHERE); upsert ON CONFLICT updated.
payments/payment_methods/subscriptions: took the PREFERRED path — checkout-time stamping wired.
CheckoutService.ResolveRailMerchantAccountID (merchant_rail_secrets.go) resolves the ACTIVE account via the
existing RailMerchantAccountScopeResolver and stamps ctx (db.WithRailMerchantAccountID) in Checkout(),
RegisterPurchase(), session creation (session row now carries the id) and ConfirmSession (session-pinned id
preferred). Nil when unresolvable — never invented; solana/admin paths stay NULL. The `*_legacy` partial
uniques on those three tables keep their names: the NULL lane still exists (unstamped writers), it just stops
growing from checkout.

- `rail_customer_accounts.rail_merchant_account_id`: the ONLY writer (`UpsertRailCustomerAccount`) never
  includes it — every row NULL, so `uq_rail_customer_accounts_customer_rail_merchant_account`,
  `uq_rail_customer_accounts_rail_merchant_account_customer`, `idx_rail_customer_accounts_rail_merchant_account`
  can never contain a row and the `_legacy` indexes are the only live lane. Stamp on upsert or drop column + 3 indexes.
- `payments` / `payment_methods` / `subscriptions`: provenance stamping happens ONLY in webhook handling
  (`WithRailMerchantAccountID` call sites in `internal/http/handlers/webhook.go`); checkout/solana/admin
  inserts land in the NULL lane by deliberate policy ("nil is better than inventing provenance",
  `internal/db/provider_account_stamp.go`). So the `*_legacy` partial uniques are doing the real dedup work
  and NOT NULL is unenforceable. Either build checkout-time account resolution
  (`GetActiveRailMerchantAccountForNewWork` already exists) so all new work is stamped and the lanes converge,
  or keep the two-lane design and rename the misleading `_legacy` suffix.

---

# #705: [DB] naming sweep: post-#512/#603/#683 stragglers

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — usage_events.money_transaction_id →
ledger_transfer_id and invoice_payments.money_transaction_id → ledger_transfer_id (+ uq_invoice_payments_ledger_transfer);
BOTH got FKs to ledger_transfers(id) — delete.go tolerates them (neither table is purged; merchant delete is a
tombstone) — and the JSON field is renamed too (usage_events → `ledger_transfer_id`). money_settings
auto_topup_amount_cents → auto_topup_amount (Go field AutoTopupAmount everywhere incl. client.go SDK, whose
json tag `auto_topup_amount_cents` had NEVER matched the server's `auto_topup_amount` — fixed). subscriptions.rail
DEFAULT 'ccbill' dropped (all sqlc inserts pass rail; two raw test fixtures fixed). provider→rail axis:
rail_intents.rail, rail_mutation_logs.rail + rail_intent_id (+ FK/index renames), rail_refresh_watermarks.rail +
rail_merchant_account_key (+ _rail_check, idx_..._rail; identity_key constraint name kept — river ON CONFLICT
targets it), catalog_drift_events.rail (+ _rail_check), catalog_credit_purchase_prices.rails. Go wrapper structs
(intents.EnqueueParams/MutationLogParams, models.CatalogDriftEvent) keep their Provider field names and map at
the gen boundary; the analytics/ClickHouse plane (provider_mutation_events) deliberately untouched. Stale names:
products_key_not_null, idx_products_key, rail_merchant_accounts_rail_not_null,
rail_customer_accounts_account_id_not_null; uq_subscriptions_tenant_subject/user_tier_group_active comment drift
fixed (schema + subscription_repo.go + checkout/service.go). purchase_status enum left as-is (rename not cheap:
type name baked into gen + models).

- `usage_events.money_transaction_id`, `invoice_payments.money_transaction_id` (+ its unique index) — store
  LEDGER TRANSFER ids since #512 dropped money_transactions; rename `ledger_transfer_id` (+ optional FK).
- `money_settings.auto_topup_amount_cents` — value is micros; JSON name is already `auto_topup_amount`.
  Rename. (Only `_cents` column in the schema.)
- `subscriptions.rail DEFAULT 'ccbill'` — doujins-era default baked into a multi-rail schema; drop the
  default, require explicit rail.
- provider vs rail column axis (#683 survivors): `rail_intents.provider`, `rail_mutation_logs.provider` +
  `provider_intent_id`, `rail_refresh_watermarks.provider` (+ generated `provider_account_key`),
  `catalog_drift_events.provider`, `catalog_credit_purchase_prices.providers` text[]. Pick one word.
- Stale names from column renames: `products_slug_not_null` / `idx_products_slug` (column is `key`),
  `rail_merchant_accounts_provider_type_not_null` (column is `rail`),
  `rail_customer_accounts_rail_customer_id_not_null` (column is `account_id`).
- Comment drift: code comments referencing `uq_subscriptions_tenant_subject_tier_group_active` (old name);
  `purchase_status` enum predates purchases→payments rename (cosmetic; rename only if cheap).

---

# #706: [DB] custom_credit_types is a broken seam between #475 and #639/#640 credit systems

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — catalog sidecar push is now the
custom_credit_types WRITER: `syncCreditBalances` (pkg/service/catalog_sidecars.go,
`defineCustomCreditUnit`) auto-defines/activates a type row for every non-builtin
`catalog_credit_balances.unit` (name = unit, or the part after `slug/`; a qualified unit naming another
merchant's slug fails the push loudly). Decimals = 0 (whole units; decimals only scale display) and an
existing row's decimals are NOT clobbered on re-push (ON CONFLICT sets active=true only). Deactivation:
NEVER automatic — a removed balance leaves its type ACTIVE (grants/lots may still reference the unit;
entitlements must not be lost to catalog churn); schema comment documents the doctrine. Dead admin CRUD
deleted: MoneyService.DefineCustomCreditType/ListCustomCreditTypes/SetCustomCreditTypeActive + their sqlc
queries (GetCustomCreditType kept — ResolveUnit's read). Integration proof:
TestSyncCatalogSidecars_AutoDefinesCustomCreditTypes (push → active row → credit-purchase quote + deposit
resolve `slug/unit` → prune leaves type active) and TestNativeCatalogRemainingProductUseCasesHTTP now
relies on the publish auto-define (manual defines removed).

---

# #707: [DB] two live metered-pricing engines: catalog_price_metered (#599) vs catalog_rate_cards (#638)

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — UNIFIED on rate cards (clean-mapping branch);
**Post-completion note (session orchestrator):** the implementing agent was killed by a session limit before its final report; its work was complete and verified. Also landed in the same window (Paul's own planned rename, done concurrently — not agent work): `products.status` and `prices.status` (draft/active/archived 3-state) collapsed to `archived boolean` end-to-end (schema, sqlc, pkg/catalog manifest `archived:` flag, plan/apply counters) — `draft` had no users. Verified green alongside #707: fresh-DB apply, sqlc regen, full catalog/money/pkg-service integration.
`catalog_price_metered` DROPPED from the baseline. No host manifest (doujins config/catalog.yaml, hentai0,
cozy-art billing_catalog.yaml) declares `metered:` — zero wild users. Mapping: manifest validation
translates each legacy `metered:` price into a rate card (`translateMeteredPrices`, pkg/catalog/load.go):
per_unit {unit_amount: rate_micros, divide_by: per_units (× per-seconds for gauges), round: half_up
default} — bit-identical round-once big.Int math. A pure-usage price (unit_amount 0) loses its price row;
a base-fee metered price keeps the row minus the metered block. Aggregation bridge for legacy-shaped
meters ({key, kind}, no aggregation) in loadCatalogRateCards: counter → sum of dimensions[key] defaulting
each event to 1 (EXACT #599 hybrid, was 'count'), gauge → sum defaulting 0; explicit-aggregation meters
unchanged. Watermark identity fix: sweep source is now the STABLE `metered:<meter_key>` (was
`metered:<meter>:rate_card:<id>` — ids are regenerated on every delete+reinsert push, so a mid-period
re-push double-billed the prefix); one-usage-card-per-meter now also enforced by partial unique index
uq_catalog_rate_cards_meter + cross-shape manifest validation. Killed with the table: the pre-aggregated
push-rating lane (POST /v1/merchant/usage/metered route + ServiceMeteredUsage handler +
Service.AccrueMeteredUsage + money AccrueMeteredAggregate/AccrueCatalogMeteredAggregate/
RateMeteredUsageFromEvents/MeteredRate — no callers in any host repo); RecordUsage → invoice-time sweep is
the single rating path. Also fixed in passing: sweep's rate-card currency filter is now case-insensitive
(manifest pushes lowercase price.currency, money normalizes upper — manifest-pushed cards were silently
skipped). catalog_dump emits rate cards only (no metered reconstruction). Semantic deltas (documented, no
users affected): value property falls back to metadata[key] in addition to dimensions[key]; declared rate
cards on a kind-counter meter now sum quantities instead of counting rows.

---

# #708: [DB] missing hot-path indexes (payments txn lookup; webhook jsonb lookups)

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — added idx_payments_merchant_rail_transaction
(merchant_id, rail, transaction_id) covering GetPaymentByTransactionID across both provenance lanes. Webhook
jsonb lookups: expression indexes for the CONCRETE production keys only — payments (merchant_id,
metadata->>'stripe_invoice_id') and (merchant_id, metadata->>'nmi_subscription_order_id'), subscriptions
(merchant_id, rail, gateway_response->>'order_id') — each partial WHERE (expr) IS NOT NULL (provable from the
strict `=` qual, unlike `metadata ? key`). Limitation noted: the queries take the key as a PARAMETER, so only
custom plans (param inlined at plan time) match the expression; generic plans fall back to a scan. New keys
need new indexes.

- `GetPaymentByTransactionID` (renewal/dunning `lifecycle_service.go`, solana poller, checkout finalize) has
  no covering index for non-legacy rows: `idx_payments_rail` is rail-only, the partial uniques cover only
  their own lanes. Add full `(merchant_id, rail, transaction_id)`.
- Webhook convergence jsonb scans: `GetPaymentByMetadataValue` (`payments.metadata ->> 'stripe_invoice_id'` /
  `'nmi_subscription_order_id'`) and `GetSubscriptionByRailMetadataValue` (`subscriptions.gateway_response ->> key`)
  have no expression/GIN indexes — merchant-wide seq scans on the webhook path. Add expression indexes for the
  known keys.

---

# #709: [DB] constraint doctrine: merchants-FK split, findings→runs FKs, money_settings PK

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — doctrine adopted: EVERY merchant-scoped table now
has merchant_id → merchants(id) ON DELETE RESTRICT (25 FKs added: invoices, payments, subscriptions,
money_settings, usage_events, ledger_accounts, ledger_transfers, invoice_items, invoice_payments,
rail_customer_accounts, notification_queue, tier_schedules, payer/invoker_spend_limits, solana_subscriptions,
reconciliation_{runs,findings,state}, catalog_drift_events, custom_credit_types, merchant_{deks,secrets,exports},
entitlements, rail_intents); guard test extended (merchant_fk_backfill_test.go). RESTRICT cannot break
delete.go: merchant delete is a TOMBSTONE (status='deleted') + gated child purge, never a merchants row DELETE —
no purge-order change needed; lifecycle integration tests green. The 3 pre-existing CASCADE merchant FKs
(rail_merchant_accounts, rail_refresh_watermarks, rail_mutation_logs) left as-is (test cleanups rely on them;
rows are operator catalog/bookkeeping, not money). findings→runs: first_seen_run/last_seen_run made NULLABLE
with RESTRICT FKs — code decides it: the intents volume breaker raises findings OUTSIDE any run (was writing
uuid.Nil sentinel, now NULL); runs are never pruned in production so RESTRICT is safe; upsert keeps the prior
last_seen_run via COALESCE when a run-less writer refreshes. money_settings: surrogate id DROPPED (referenced by
nothing — verified gen + Go), PK = (merchant_id, customer_id, currency), uq_money_settings_payer subsumed.
ledger_transfers.grant_id comment documents the deliberate no-FK ledger-purity rule for grant_id/invoice_id/customer_id.
2026-07-03 gate run: internal/crypto DEK-store integration tests still minted unseeded random merchant ids and tripped the new merchant_deks_merchant_fk — dek_store_db_integration_test.go now seeds merchants rows; suite green.

- merchants-FK coverage is ~50/50: catalog/rail/config tables reference `merchants(id) ON DELETE RESTRICT`;
  the money core (invoices, payments, subscriptions, ledger_accounts, ledger_transfers, money_settings,
  entitlements, rail_intents, usage_events, ...) has bare `merchant_id` uuid, masked by the manual purge in
  `internal/merchants/delete.go`. Either all merchant-scoped tables get the FK, or none do and delete.go is
  the documented contract.
- `reconciliation_findings.first_seen_run`/`last_seen_run` — NOT NULL uuids referencing reconciliation_runs
  by convention, no FK. Add.
- `money_settings.id` surrogate PK referenced by nothing; natural key (merchant_id, customer_id, currency)
  already unique. Make it the PK or note why not.
- Deliberate and fine (document, do not "fix"): ledger_transfers.grant_id/invoice_id/customer_id have no FKs
  by ledger-purity design.

---

# #710: [CONFIG] dead/broken knobs: allowed_cidrs, SECRET_BACKEND env, dead overlay sections, NMI boot fields, mode alias

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — (1) allowed_cidrs DELETED from both planes: per-merchant
CIDR narrowing has no threat-model value (CCBill webhooks come from provider-wide published ranges, identical for
every merchant) and the HTTP IP gate runs pre-merchant-resolution by design; iputil.Configure seam +
config.ConfigureProcessGlobals removed, DefaultCCBillIPRanges IS the allowlist; a manifest
ccbill settings.allowed_cidrs now fails with a loud removal error. (2) envKeyToConfigKey rebuilt as a
reflection-derived table of Config's top-level koanf keys (exact match for scalars, one-level nesting for
struct/map fields) — SECRET_BACKEND works; env-path regression tests added (SECRET_BACKEND + CLICKHOUSE_HTTP_ADDR);
the never-working BILLING_SECRET_BACKEND doc claim dropped. (3) Overlay: dead ISSUER section deleted;
DELEGATED_INVOKER_WASTED_SPEND_WINDOWS routed (one JSON list value); overlay unmarshal now strict (ErrorUnused,
matching the file parser) and unroutable BILLING_MERCHANTS_* vars fail loudly. (4) NMI: NMIRailConfig.TokenizationKey
dropped; NMIProviderSettings = {SecurityKey, WebhookSecret} (Name/TestMode/TokenizationKey were never read);
dead NMIClient.config field removed; ToNMIProviderSettings() takes no name. (5) mode alias hard-cut: Config.Mode,
GetMode, ModeFull/Limited/ReadOnly, ValidModes, --mode flag all gone; a set mode/MODE/BILLING_MODE fails with a
rename error pointing at provider_write_mode.

- CCBill `allowed_cidrs` (BOTH planes) is parsed/validated/cloned and never enforced: the webhook IP check
  reads an `iputil` package global whose only production setter is `iputil.Configure(nil)` in
  `config.ConfigureProcessGlobals` — allowlist permanently = defaults; three comments claim otherwise.
  Wire the merchant-scoped list into the webhook check or delete the field from both planes (and the
  Configure seam).
- `SECRET_BACKEND` / `BILLING_SECRET_BACKEND` env vars DO NOT WORK: `envKeyToConfigKey` special-cases only
  five keys; `SECRET_BACKEND` maps to koanf `secret.backend` and never reaches the field (verified
  empirically against config.Load). Dangerous under #661 declared-intent: forcing `db` on a Vault deployment
  silently stays on Vault. Fix mapping + add an env-path regression test; longer term replace
  first-underscore splitting with an explicit key table.
- Dead merchant env-overlay sections: `BILLING_MERCHANTS_<M>_ISSUER_*` routes to a manifest field that does
  not exist (non-strict overlay silently drops it); `DELEGATED_INVOKER_WASTED_SPEND_WINDOWS` is listed as a
  section but the router has no case. Delete or route + strict-check the overlay.
- Dead NMI boot fields: `NMIRailConfig.TokenizationKey` (runtime reads only DB rail-account settings);
  `NMIProviderSettings.Name`/`TestMode` (the NMI client never reads its retained config struct). Shrink to
  {SecurityKey, WebhookSecret}.
- Deprecated `mode` alias surface has zero consumers (`Config.Mode`, `GetMode`, Mode* consts, `--mode` flag,
  env special-case). Hard-cut with a rename error, matching #698 house style.

---

# #711: [CONFIG] duplicated/confused surfaces + overengineering collapse

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — Solana knobs moved to per-merchant rail-account
**Addendum (Paul 2026-07-02):** `solana_pay_recurring_subscriptions` REMOVED entirely (config knob, settings key, boot-plane field, example yaml) — every merchant supports Solana Pay rebilling (v2 transaction system); rebillability is a property of the PRICE (catalog auto_renew), never merchant config. The /solana features echo now hardcodes solanaPayRecurringSubscriptions=true as a capability announcement. Hosts setting the removed field get a compile error on bump.
**Addendum (Claude 2026-07-02):** the deferred CI RAILS_STRIPE_SECRET_KEY leftover is DONE — last RAILS_* env names retired. Live rail invoice tests (internal/modules/money) now arm credentials through the merchant-secrets store like production (#699): manifest-mirror seeding helper (rail_merchant_accounts row + scoped secret, merchant_store_arming_integration_test.go) resolved back through LoadStripeCredentials / checkout's ActiveRailMerchantAccountSecretName+Get seam; the boot-plane RailMerchantAccountSet only carries the STORE-resolved key (catalog webhook-registration pattern); Stripe acct identity self-discovers via GET /v1/account with a logged opaque-label fallback for restricted rk_test_ keys; non-live TestRailCredentialStoreArming_ProductionResolutionPath proves the seam without provider network; BOTH live legs validated green against real Stripe test mode + NMI sandbox (surfaced+fixed bit-rot: the fail-closed provider_write_mode default blocked the invoice write — test now declares full). Env reads: OPENRAILS_TEST_STRIPE_SECRET_KEY only (drops RAILS_STRIPE_SECRET_KEY/BILLING_RAILS_STRIPE_SECRET_KEY), NMI_SANDBOX_SECURITY_KEY only (drops RAILS_MOBIUS_* aliases); pkg/service catalog live-test straggler now OPENRAILS_TEST_STRIPE_SECRET_KEY|STRIPE_SECRET_KEY. live-gated-integration.yml renamed to secrets.OPENRAILS_TEST_STRIPE_SECRET_KEY (both Stripe jobs) with ::warning:: annotations on missing secrets. OPERATOR STEP (Paul): create GitHub Actions secret OPENRAILS_TEST_STRIPE_SECRET_KEY (Stripe TEST-mode key) and delete RAILS_STRIPE_SECRET_KEY — until then the workflow's Stripe legs warn+self-skip.
`settings` (config/solana_settings.go typed parse/validate; strict at manifest push; store-wins overlay in the
/solana config+tokens handlers and the #699 pull-plane RPC; example yaml fixed, false "runtime config" claim
deleted). `solana_pay_recurring_subscriptions` DOCUMENTED as a client hint (not enforced; seam noted =
checkout-session solana-pay subscribe leg — contended this pass). CCBill boot identity declared once:
ClientAccNum/ClientSubAcc dropped from CCBillRailConfig, pair derived from dash-joined account_id
(config.SplitCCBillAccountID; account_id now required for ccbill rails even in dev). Boot-plane webhook fields
renamed to canonical WebhookSigningSecret(/Thin); koanf tags stripped from all rail structs (programmatic-only,
#521) so no boot koanf keys remain to alias. embed.ParseMerchantConfig strict (DisallowUnknownField +
renamed-key pointers) + test. Twin merges SKIPPED with reasons: CCBillConfig now differs (derived pair +
TestMode); NMIProviderSettings merge would ripple into contended checkout. environment:test|live comments fixed
to "assertion vs test_mode, not a selector" (config + manifest + example). embedded.New warns on TestMode
zero-value in dev-like Env (+ Options/README docs). Nesting-doll collapse: bootstrap.Options/NewApp DELETED
(embedded.New + serverboot call app.BootstrapWithOptions directly). CleanupConfig/CardAbuseConfig: ONE defaults
path (registration wires Default*; in-Work re-default + withDefaults deleted; zero cleanup config errs loudly).
tests/testcontainer_suite RAILS_MOBIUS_* → OPENRAILS_TEST_MOBIUS_*. Left as noted: CI RAILS_STRIPE_SECRET_KEY
(GitHub-secret rename + contended modules/money test); process-wide poller/RPC-client rpc knobs stay boot-plane
(RLS blocks a boot-time cross-merchant store read; per-merchant seam noted).

- Solana runtime knobs (`rpc_provider`, `rpc_api_key`, `tokens`, `solana_pay_recurring_subscriptions`) are
  unreachable in standalone (only embedded `Options.PaymentProviders` can set them) while
  `config/merchants_config.example.yaml` claims they live in runtime config and shows a
  `rail_merchant_accounts:` yaml shape no loader parses. Move them into per-merchant rail-account `settings`
  (where tokenization_key lives) and fix the example. Also `solana_pay_recurring_subscriptions` gates nothing
  server-side (echoed to browsers only) — enforce or rename as a client hint.
- CCBill identity declared twice in the boot plane (`account_id` AND `client_acc_num`/`client_sub_acc`, can
  contradict); the store path derives the pair from account_id — boot plane should too.
- Webhook-secret name drift: boot `webhook_secret`/`webhook_secret_thin` vs store/manifest canonical
  `webhook_signing_secret`/`_thin`. One canonical name in both planes.
- `embed.ParseMerchantConfig` uses non-strict yaml (typo'd `acounts:` provisions a merchant with no rails,
  silently) while manifest paths use DisallowUnknownField. Use the strict parser.
- Embedded `test_mode` zero-value silently means LIVE posture where standalone defaults dev→sandbox;
  embedded.New should warn or require explicitness when Env is dev-like and TestMode was left zero.
- Overengineering: four nesting-doll option structs relaying the same fields (embedded.Options →
  bootstrap.Options → app.BootstrapOptions → runtimeOverrides — collapse at least one, #688 altitude);
  `CleanupConfig`/`CardAbuseConfig` constants-dressed-as-config with redundant re-defaulting layers;
  vestigial koanf tags on rail structs nothing parses since #521; twin conversion structs (CCBillConfig vs
  CCBillRailConfig, NMIProviderSettings vs NMIRailConfig); `environment: test|live` is an assertion, not a
  selector — comments should say so; test harness resurrects the retired `RAILS_` prefix
  (tests/testcontainer_suite.go) — rename to OPENRAILS_TEST_*.

---

# #712: [DOCTRINE] no library env reads — env is read once at the binary boundary + guard test

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — catalog_reconciliation_interval moved into Config
(Load/Validate fail loudly on malformed; "0" disables; threaded via Runtime.Config to the periodic-job builder);
db.sql_trace bool on DBConfig replaces OPENRAILS_SQL_TRACE (threaded through NewDB); both retired OPENRAILS_* env
names fail with rename errors in config.Load. THIRD violator found+fixed: internal/integrations/vault ambient
VAULT_TOKEN fallback removed (vault.token config carries it; absence fails closed). Guard test
config/env_boundary_test.go greps the module for os.Getenv/os.LookupEnv/os.Environ/syscall.Getenv and fails outside
cmd/, config/, tests/, scripts/, internal/dbtest/, internal/bootstrap/merchant_env.go (each allowlist entry
justified inline); *_test.go exempt. Green.

Doctrine (decided with Paul 2026-07-02): libraries must NOT call os.Getenv / read ambient env vars
behind the host application's back. Env is read exactly once, at the process boundary, by the binary's
config-loading pipeline (cmd/openrails via config.Load / BILLING_* overlay — correct today); every importable
package receives the result as explicit config. In embedded mode the HOST is the process — its env belongs to
its config pipeline, not ours. Test binaries own their env (OPENRAILS_TEST_* fine). Where an env-derived
value gates something dangerous, shape the library field so absence fails closed.

Violations to fix:
- `internal/river/river_register.go`: `OPENRAILS_CATALOG_RECONCILIATION_INTERVAL` raw os.Getenv with a SILENT
  1h fallback on malformed input (the exact typo-picks-a-behavior class provider_write_mode validation
  prevents). Move into Config; fail loudly on parse error.
- `internal/db/db_pgx.go`: `OPENRAILS_SQL_TRACE` undocumented debug knob — move to config/debug surface.

Enforcement: add a guard test that fails on `os.Getenv`/`os.LookupEnv` outside cmd/, config/, and *_test.go
(same spirit as the existing schema guard tests). Mirror issue: authkit #231.

---

# #725: [BUG] arrears charger arms from the boot-plane rail set — must resolve rail credentials from the merchant-secrets store

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — ScopedCharger (the ONE charger behind the arrears
InvoiceWorker AND the #674 topup_charge intent handler) now takes a per-charge CollectionAdapterResolver:
`money.MerchantCollectionAdapterBuilder` (internal/modules/money/merchant_collection_wiring.go) arms Stripe +
NMI adapters PER MERCHANT from the merchant-secrets store AT CHARGE TIME (#699 precedence: store wins;
declared-account-with-missing-secret fails CLOSED; no store row → boot-plane adapter fallback, so embedded
hosts keep working; no caching → rotation-safe). Scope pick: the method's stamped #704 provenance account
first (archived stays chargeable for existing obligations, #655), else PullRailMerchantAccountScope.
Wired in build_runtime.go with late-bound Runtime.Merchants. Integration tests (money pkg): store-ONLY
Stripe + store-ONLY NMI charge via fake provider servers, boot fallback, fail-closed. NOTE: dunning/manual
NMI rebills were the one leftover — DONE as #730 (completed.md): ManualRebillHandler arms from the same
store resolver at charge time.

The collect-outstanding / arrears charging worker is constructed once at boot from the in-memory boot-plane
rail set (`internal/app/build_runtime.go` ~:350 builds the charging `StripeService`/collection adapters from
`config.RailMerchantAccountSet`). Webhooks, checkout, and the reconcile pullers all resolve per-merchant rail
credentials from the merchant-secrets store (store-wins, #699): `merchants.Service.LoadStripeCredentials`,
checkout's `ActiveRailMerchantAccountSecretName` + `Secrets().Get`. The arrears worker does NOT — so a
merchant whose Stripe/NMI key lives only in the manifest/secrets store (the intended model) has working
checkout + webhooks but an arrears worker that cannot charge. Works today only because embedded hosts also
pass keys at boot.

Fix: the charging path resolves credentials PER MERCHANT from the store at charge time (store-wins, boot
plane fallback), using the same production resolvers the other planes use. Cover every rail the
ChargeOutstanding/collection path serves (Stripe adapter; NMI vaulted rebills if that leg shares the
constructor). Integration test: merchant with store-only credentials -> arrears charge succeeds (the #711
store-arming test helpers in internal/modules/money/merchant_store_arming_integration_test.go are the
ready-made seeding harness).

---

# #726: [DB] invoices.line_items jsonb vs invoice_items table — one representation, or two documented roles

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — kept both with disjoint roles, deleted every
cross-copy. Trace showed all production invoice_items readers filter pending-only (nobody ever reads
attached 'invoiced' rows), while the jsonb is the only reader-facing itemization — so: `invoice_items` =
pending-accrual workspace ONLY (attach-at-close is just the consumed tombstone), `invoices.line_items` =
immutable as-billed statement. Deleted the write-only 'invoiced' copy writes (insertInvoiceItemsFromRollup
and the true-up row — the true-up is now a `minimum_spend_trueup` statement line) plus the 5 dead columns
(event_type, period_from, period_to, quantity, unit_amount); InsertInvoiceItem → InsertPendingInvoiceItem;
roles pinned by schema COMMENTs.

Paul 2026-07-02: full latitude — consolidate onto one representation, delete one, or keep both with the
roles made explicit; whatever the code says is right. Trace ALL readers first (invoice API DTOs in
pkg/service, arrears receivable machinery, metered rating writes, statement rendering) before choosing.
Schema edits go directly into migrations/postgres/0001_schema.up.sql (greenfield baseline).

---

# #727: [RENAME] finish the rail-vocabulary + payments-vocabulary renames skipped by #705

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — all three renames landed in the baseline (0001_schema.up.sql) + queries + sqlc regen: `openrails.purchase_status` → `payment_status` (gen type OpenrailsPurchaseStatus* → OpenrailsPaymentStatus*; zero hand-written Go referenced the type — only two comments swept), `probe_verdicts.provider` → `rail` (PK, table/column COMMENTs, probe_verdicts.sql, probe_cache.go params/log fields), `reconciliation_runs.providers` → `rails` (CreateReconciliationRun, reconcile/store.go + converge.go + one raw-SQL test). Fresh-DB apply, sqlc generate, build/vet(+integration), migrations guard tests and the named integration suites all green; status string VALUES untouched.

- `openrails.purchase_status` enum → `payment_status` (the payments table stopped being "purchases" long ago;
  the enum name is the last survivor). Baseline CREATE TYPE + every `::openrails.purchase_status` cast in
  schema/queries + the sqlc-generated Go type (PurchaseStatus → PaymentStatus) and its uses.
- `probe_verdicts.provider` → `rail`.
- `reconciliation_runs.providers` → `rails`.
Schema edits go directly into migrations/postgres/0001_schema.up.sql; sqlc regen; grep sweep.

---

# #728: [BUG] Solana process-wide poller/RPC client still arms from the boot plane

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — new `solanamodule.MerchantRPCBuilder` (#699/#725 pattern: store rail-account settings win, boot client fallback, malformed declared settings fail LOUD, nothing cached across passes) wired as Runtime.SolanaRPCResolver. The poller did NOT fan out per merchant before — pollPendingPayments was restructured: pending-set members are now merchant-attributed (`<mid>|<ref>`, stamped at store/register time from the request's merchant ctx; pre-cut bare members drop with a WARN), grouped per merchant, and each merchant's pass resolves its RPC + transaction service (WithRPC copy) and runs RLS-scoped under RunInMerchantConn; the boot-rail start gate in Runtime.RunWorkers is gone. Crank/recurring: signerSubmitter resolves RPC per merchant at submit time (NewSignerSubmitterWithResolver), the cranker builds even with no boot Solana rail, and the #674 pull-intent verify leg reads the chain via the resolver's merchant-scoped ChainReader. Integration tests (poller_store_arming_integration_test.go): store-only merchant polls via its store-armed endpoint, undeclared merchant via boot, malformed settings error — green, plus the full named suites.

Was: noted by #711 — the process-wide poller/crank/RPC client armed once at boot from SolanaRailConfig; a
store-only merchant got correct /solana config + pulls but a poller with wrong/no RPC settings.

---

# #729: [BUG] TestUpdateSubscriptionPaymentMethod* fail against the NMI mock (422) — pre-existing

**Status:** IMPLEMENTED (2026-07-02) — root cause was NOT the mock's v5 surface (it serves GET
/subscriptions/{id} fine): the #674 intent registry captures Runtime.NMIClients at BuildRuntime, and
SetupSuiteWithMockNMI REPLACED the map, so nmi_payment_source_update resolved the boot-time client and hit the
REAL Mobius sandbox (422 "The provided data is invalid." for non-numeric test ids) ⇒ Ambiguous ⇒ 409. Fix:
tests re-arm the inline intent plumbing after any client swap — TestContainerSuite.RearmIntentPlumbing()
(tests/testcontainer_suite.go), called from resetNMIClients + SetupSuiteWithMockNMI. Full tests/ package green.

---

# #724: MODE 2 — Vault-backed hosted mode proven against a REAL Vault (test-harness half)

**Status:** IMPLEMENTED (2026-07-02) — harness + scenario-table half. The #723 mode-gate boot
validation rows (merchant_source matrix, mode-2 boot shape with no manifest files) land with #723
by its own agent; Build's `merchant_source=manifest` refusal was already in place mid-flight and
the mode-2 tests here declare `MerchantSource: api` explicitly.

Shipped:
- **vaulttest harness** — `internal/integrations/vault/vaulttest` (integration tag): shared
  hashicorp/vault:1.21 dev-mode testcontainer per test-package process (sync.Once + TestMain
  teardown, dbtest pattern); VAULT_ADDR+VAULT_TOKEN env override reuses an external dev server the
  way OPENRAILS_TEST_DB_URL does. Helpers: policy-scoped child tokens (canned
  PolicyKVReadWrite/PolicyKVReadOnly/PolicyTransitOnly mirroring docs/vault.md), EnsureTransit,
  EnsureAppRole, RevokeToken, and StartDedicated (per-test container with docker pause/unpause for
  outage tests — memory intact across unpause).
- **Mocks killed**: merchantsecrets/store_integration_test.go fakeVault httptest server DELETED
  (Build now proven against the real container); integrations/vault/auth_test.go httptest Vault
  DELETED (token/approle login + policy scoping proven in auth_integration_test.go against real
  Vault; only pure no-HTTP error paths stay unit). KEPT deliberately: kv_test.go path arithmetic,
  store_test.go TestGateSecretBackend/TestDeriveRouteGates (our logic, not Vault), and the
  merchants in-memory VaultKV fakes for store-addressing/cache unit tests.
- **Un-skipped**: vault_integration_test.go + capabilities_integration_test.go retagged
  vaultint→integration; they run unconditionally via the container.
- **Scenario table** (internal/merchantsecrets/vault_scenarios_integration_test.go, all real
  Vault + real Postgres, green): store cycle w/ KV-v2 current_version bump on rotation + two-
  merchant isolation; full stack UpsertPaymentProviderConfig → KV at the
  RailMerchantAccountSecretName path → arming-resolver read → rotate-via-second-PUT → second
  merchant untouched; capability gating with REAL policies (read-only serves reads + refuses
  writes as ErrSecretBackendUnavailable + SecretWrite gate off; transit-only + vault backend
  refuses boot; transit-only + db degrades w/ signing on); outage via pause/unpause (boot
  unreachable ⇒ "vault capability probe" fail-loud; read-time ⇒ ErrSecretBackendUnavailable and
  NEVER ErrSecretNotFound; in-process TTL cache keeps serving previously-read values by design;
  unpause ⇒ recovery without restart, data intact); token lifecycle (approle token_period=5s +
  LifetimeWatcher keeps ops alive past TTL; revoke ⇒ loud 403; PINNED: no emergent re-login —
  supervisor/restart owns recovery); backend parity — the same cycle/rotation/isolation/status
  table runs on the DEK-encrypted Postgres backend and the configured-status sets must match.
- **Behavior fix** (needed by parity/status): `KVv2Adapter.ListSecrets`
  (internal/integrations/vault/kv.go) now recursively descends KV-v2 "dir/" entries and returns
  full relative LEAF names. Before, the vault store's List returned only top-level dirs
  ("rail_merchant_accounts/"), so ListSecretStatuses/PaymentProviderConfig reported every
  vault-held credential as unconfigured — a real mode-2 API bug.
- **ADR**: docs/adr-custodial-merchant-secrets.md — custodial posture (ONE process-global Vault
  token; isolation is OpenRails' addressing + RLS), threat model, non-goals (per-merchant tokens,
  merchant-visible Vault), revisit trigger (BYO-Vault). Enforced by the grep-style guard test
  TestNoAdHocSecretPathConstruction (internal/merchants/secret_path_guard_test.go): the durable
  path fragments may appear only in the canonical builders (allowlisted).
- Fixture fix: the pre-existing #667 DB round-trip test used a random unregistered merchant id —
  now trips merchant_deks_merchant_fk; tests register real merchant rows.

Verified: go build ./... + go vet ./... clean; internal/integrations/vault, internal/merchantsecrets,
internal/merchants integration suites all green against OPENRAILS_TEST_DB_URL + the vault container.

Deferred (out of scope here): mode-2 boot shape end-to-end (no-manifest boot + API-provisioned
catalog + full checkout) — lands with #723's mode gate; vault store Secret.Version stays a
constant 1 — CLOSED 2026-07-03: VaultKV Read/WriteSecret now return the KV-v2 version, the store
surfaces it on Secret (kvVersionOrOne floor), scenario tests assert 1→2 on rotation and strict
monotonicity on both backends (was: parity pinned value semantics + monotonic version,
not equal numbers); HTTP-handler-level PUT /v1/merchant/payment-providers duplicate of the
service-level full-stack test.

---

# #721: platform merchant directory + soft-delete (engine mechanism for openrails-saas #16)

**Completed:** no
**Status:** IMPLEMENTED, uncommitted (2026-07-02). Tests green.

Cross-merchant platform operator surface, standalone-only (mounted from internal/http/server.go;
no embedded analogue — an embedded host controls exactly one merchant).

## Routes (all under /v1/platform, human user sessions only — API keys/delegated/host principals 401)
- GET    /v1/platform/merchants            — root:merchants:read; ?status=active(default)|deleted|all, limit/offset
- GET    /v1/platform/merchants/:id        — root:merchants:read (any status; operators inspect tombstones)
- DELETE /v1/platform/merchants/:id        — root:merchants:delete; SOFT delete only, idempotent
- POST   /v1/platform/merchants/:id/restore — root:merchants:restore; idempotent
Explicitly NOT built (saas #16): POST create, PATCH, hard delete, export, credential edits, any
customer/payment/subscription sub-routes (probed 404/405 in tests).

## Decisions
- **Permission strings are `root:merchants:{read,delete,restore}`, NOT `platform:merchants:*`.**
  AuthKit #111 renamed the platform namespace to `root:` (its docs cite `openrails
  root:merchants:delete` as THE intended app extension; doujins already follows). Namespace purity
  is schema-enforced: a root-persona role literally cannot hold a `platform:` grant. saas #16's
  platform:merchants:* map 1:1. Constants in permissions/permissions.go; gate =
  ControlPlane.HasRootPermission (Can on the singleton root group). Two bounded root roles
  declared in controlplane.Groups(): `merchant-directory-viewer` (read) and
  `merchant-directory-admin` (read+delete+restore); root `owner` (root:*) covers both. No
  superadmin role invented.
- **Soft-delete representation: the EXISTING status ('active'|'deleted') + deleted_at columns.**
  No new flag. Scope limit honored (Paul axed lifecycle enforcement, ex-#719): directory state
  only — no engine-wide charge/renewal/webhook blocking, no suspension semantics. Merchant-scoped
  API auth failing closed on soft-deleted merchants is EMERGENT, not new code: credential→merchant
  resolution (controlplane merchantDirectoryRow/MerchantScope) already filters
  `deleted_at IS NULL AND status='active'` → 403 service_credential_merchant_unresolved; restore
  re-admits. Asserted in tests. The #225 gated purge (export-before-delete) stays the only
  row-destroying path; platform DELETE never touches business rows.
- **RLS forces per-row enrichment probes.** rail_merchant_accounts + payments are FORCE-RLS
  merchant-isolated; under openrails_app a single cross-merchant JOIN returns nothing. The list
  reads the GLOBAL merchants table in one query, then per row one MerchantTx (GUC-pinned) runs two
  index probes: rails_armed = DISTINCT non-archived rail_merchant_accounts.rail; last_payment_at =
  latest payments.created_at (new baseline index idx_payments_merchant_created makes it O(1)).
  Page-bounded (limit ≤ 200). Last-activity proxy = latest payment (money movement is the ops
  signal; subscriptions/webhooks would cost more probes for less meaning).
- Ordering: created_at DESC, id DESC. Envelope: SuccessJSONPaginated (object/data/total/has_more).
- Handlers call gen directly (#688) — no repo wrapper; queries in
  internal/db/queries/merchants_platform.sql.

## Files
permissions/permissions.go, internal/controlplane/{catalog,authority}.go,
internal/db/queries/merchants_platform.sql (+ gen), migrations/postgres/0001_schema.up.sql
(idx_payments_merchant_created), internal/http/routes/platform.go,
internal/http/handlers/platform_merchants.go, internal/http/routes_platform.go,
internal/http/server.go (mount), internal/integrationharness/platform_merchants_http_test.go.

## Tests (integration, real stack: RLS app role + real AuthKit root grants)
- TestPlatformMerchantDirectoryListHTTP — fields incl. rails_armed (archived excluded) +
  last_payment_at, ordering, pagination no-overlap, 404/400 ids, bad status filter.
- TestPlatformMerchantSoftDeleteRestoreHTTP — delete→default list excludes / status=deleted+all
  include / GET by id still 200; merchant API key 403 while deleted, works again after restore;
  idempotent re-delete (deleted_at unmoved) + re-restore; unknown ids 404.
- TestPlatformMerchantPermissionsHTTP — viewer reads but 403 on delete/restore; no-role user 403;
  API keys + unauthenticated 401; skipped routes (POST create, PATCH, customers/payments/
  subscriptions subpaths) 405/404.

# #723: MODE 1 — manifest-is-truth self-hosting: YAML in memory, no stores, reboot to change

**Status:** IMPLEMENTED 2026-07-02 (Claude; uncommitted) — full spec in future.md (section retained there with a moved pointer).

The two-mode doctrine is now code. ONE scalar `merchant_source: manifest | api` (koanf
`merchant_source`, env MERCHANT_SOURCE via the standard mapping; empty = manifest; unknown value =
load error; accessors `Config.MerchantSourceMode()` / `IsManifestMerchantSource()`). It governs
merchant config AND catalog — no separate catalog_source.

## What shipped

- **In-memory credential plane** — `merchants.ManifestSecretStore`
  (internal/merchants/secrets_manifest.go): merchant-namespaced memory store implementing the SAME
  `MerchantSecretStore` interface every consumer reads (checkout/vault via
  `Runtime.ArmMerchantsService`, webhooks, #699 pulls, #725/#730 charge resolvers) — the manifest
  IS the store, so "store wins" is vacuously true. Runtime Put/Delete return
  `ErrManifestSecretsReadOnly`; only boot provisioning writes through `Seeder()`. Lives on
  `app.Runtime.ManifestSecrets`, created at BootstrapWithOptions iff mode 1.
- **Store never constructed in mode 1** — `merchantsecrets.Build` REFUSES in manifest mode; new
  `merchantsecrets.BuildManifest` (store view: memory plane + optional Vault Transit for Solana
  signing; SolanaCanSign=true, SecretWrite=true so the write routes stay MOUNTED and serve the
  pointed 405, not a bare 404) + `BuildTransit`. Branched call sites: internal/http/server.go
  (standalone), Runtime.EnsureMerchantsService (embedded workers — also now arms checkout/vault the
  way standalone does), embed/provision.go UpsertMerchantConfig (arms Runtime.Merchants over the
  plane immediately), bootstrap manifest reconcile, merchant_dump (refuses in mode 1 — the YAML is
  the export), pkg/embedded pull_provider CLI (WARN + boot-plane only; the server's River pulls
  stay store-armed).
- **Provisioning** — `ProvisionMerchant` forces Insert+Overwrite+Prune in manifest mode (YAML
  steamrolls the DB projections + memory every apply; seed-once is a mode-2 semantic).
  `MerchantManifestReconcileOptions.SecretStore` injects the boot plane;
  `ReconcileMerchantManifestData` in mode 1 without an injected store validates secrets into an
  EPHEMERAL memory store (CLI runs converge DB projections, persist nothing, one INFO log).
  Embedded UpsertMerchantConfig in mode 1 = reconcile identity/config/accounts rows + seed memory;
  the empty-config read-side bind (hentai0) stays legal. Standalone boots load
  `/etc/openrails/merchants.yaml` when present (serverboot `reconcileBootMerchantManifest`;
  `Options.MerchantManifestPath` override — explicit path missing/unloadable ⇒ refuse boot).
- **Mutation choke point** — ONE middleware `manifestModeWriteGuardMW`
  (internal/http/routes/routes.go), prepended to the writeMW chains of
  registerCatalogActionRoutes + registerPaymentProviderActionRoutes (covers standalone AND the
  embedded mounts, which share these registrars): 405 + machine code `manifest_driven`
  ("edit the YAML/secret files and reboot"). Runs BEFORE auth (deployment posture, not merchant
  data). Plan-only POST /catalog/publish is ALSO rejected (deliberate: middleware doesn't parse
  bodies; the CLI dry-run remains). Reads stay served.
- **Boot validation matrix** — config.Validate: unknown merchant_source ⇒ load error; api mode +
  BILLING_MERCHANTS_* env/secret-files ⇒ refuse (two truths); api mode outside dev + no secret
  backend (no Vault, no ENCRYPTION_MASTER_KEY) ⇒ refuse (#667 extended to declared-mode time).
  serverboot: api mode + merchants.yaml present ⇒ refuse. embed: api mode + UpsertMerchantConfig
  declaring manifest truth (accounts/profile/invoice/windows/remote_application) ⇒ refuse; bare
  binds (empty / display-name-only) legal in both modes. Manifest mode ignores encryption posture
  (nothing persists). Embedded manifest mode with no Upsert call boots unbound (documented — same
  as bare standalone control-plane-only).
- **Catalog** — pkg/embedded.PushMerchantCatalog: mode 1 mutating push force-upgrades to full
  converge (Insert+Overwrite+Prune, YAML wins); mode 2 REFUSES a mutating push (two truths;
  plan-only stays as a read-only diff). CLI push-merchant-config refuses in api mode.
- **Consumers** — doujins BuildOpenRailsConfig sets MerchantSource=manifest explicitly (+ two
  pre-existing-drift fixes riding along: deleted the #710-removed `allowed_cidrs` key from
  merchant_config.yaml; renamed the #698-stale RAIL_MERCHANT_ACCOUNTS env anchors in its test).
  hentai0 unchanged (builds green against its pin; read-side bind covered by
  TestManifestMode_ReadSideBindKeepsWorking). integrationharness standalone surface pinned to
  MerchantSource=api (it IS the API-driven SaaS shape); bootstrap merchant-manifest
  store-semantics tests pinned to api via apiModeReconcileConfig().

## Tests (integration, real PG; fake provider HTTP only) — all green

- embed/manifest_mode_integration_test.go: **TestManifestMode_Loop** (the conformance loop: boot
  from manifest bytes + secret FILES via VAULT_SECRETS_PATH → NMI armed with the file key, catalog
  converged, charge through the #725 resolver hits fake NMI with that key, entitlement lands,
  runtime Put refused, zero openrails.merchant_secrets rows → rotate file + change price → reboot
  (new embed.New, same DB) → new price active/old archived, next charge carries the ROTATED key,
  still zero store rows → third boot idempotent), **TestManifestMode_MutationRoutesRejected405**
  (PUT/DELETE payment-providers + catalog create + publish ⇒ 405 `manifest_driven`; GET 200),
  **TestAPIMode_MutationRoutesWork** (same PUT persists to the store in api mode),
  **TestManifestMode_APIModeRefusesManifestTruth**, **TestManifestMode_MalformedManifestRefusesBoot**,
  **TestManifestMode_MissingSecretFailsClosed** (declared account, absent secret: boot proceeds,
  rail unarmed, charge fails closed naming the credential),
  **TestManifestMode_ReadSideBindKeepsWorking** (hentai0 shape).
- config/merchant_source_test.go: default=manifest, unknown-value refusal, api-needs-backend
  matrix, api-refuses-BILLING_MERCHANTS_* env, manifest-ignores-encryption-posture.
- Suites verified green: embed (full), internal/bootstrap, internal/integrationharness (except two
  failures owned by concurrent #721 platform-merchants work: route-surface golden +
  TestCoreDoesNotMountPlatformAdminRoutesHTTP — both diffs show only /v1/platform/merchants
  routes), internal/modules/money, internal/modules/catalog, internal/river, internal/db,
  pkg/service, internal/http. Unit tests of every touched package green. `go build ./...` green in
  openrails + doujins (go.work) + hentai0 (pin); repo-wide `go vet` noise is sibling in-flight work
  (internal/reconcile solana test fake), not #723.

## Docs

- docs/self-hosting-mode1.md: the three files, the secrets dir, precedence yaml < files < env,
  boot behavior, what 405s, rotation walkthrough, mode-2 one-liner.

## Deferred / follow-ups

- pull-provider CLI in mode 1 arms boot-plane only (no manifest plane in a one-off process);
  acceptable — the server's River pulls are the production pull plane. Documented in-code.
- Managed Stripe webhook registration in mode 1: a provider-created signing secret seeds the
  memory plane and is lost on reboot (endpoint then found without secret). Mode-1 Stripe hosts
  should declare `webhook_signing_secret` in the manifest; not newly regressed (find-existing
  behavior unchanged) — revisit if cozy-art adopts mode 1 with managed webhooks.
- #731 filed: migratekit version skew — FIXED 2026-07-03, see future.md #731.

## Spec tasks

- [x] Explicit mode switch `merchant_source: manifest | api` + validation matrix (fail-loud both ways)
- [x] Secrets from memory in manifest mode (no store reads/writes/seed-once; rotation = file + reboot)
- [x] Catalog boot-converged, Overwrite+Prune always on in mode 1; DB rows documented as FK projection
- [x] Conformance loop integration test (boot → charge/entitlement → rotate+reboot → idempotent third boot)
- [x] Failure modes: missing secret ⇒ rail unarmed + loud warn + fail-closed charge; malformed manifest ⇒ refuse; encryption posture irrelevant in mode 1 (asserted)
- [x] docs/self-hosting-mode1.md (three files, secrets dir, precedence yaml < files < env, edit + reboot)
