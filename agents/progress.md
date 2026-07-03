<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 734

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
**Status:** PHASE 0 COMPLETE — read+cancel wire LIVE-VERIFIED against the real production DataLink account
(2026-07-03); those paths shipped, production golden tests pin the shapes. REFUND FLOW BUILT (2026-07-03),
build+vet+unit green, but its wire is **PROVISIONAL** — it could NOT be probed live (real money) so the
refund request/response shape is modeled, not verified. REMAINING: the FIRST real refund must confirm the
wire (see the refund subsection); until then #696 stays open.

**PHASE 3 REFUND PROBE (2026-07-03, live — safe fail-test, NO money moved):** Paul authorized a
live refund on an OLD transaction where it MUST fail. Probed BOTH voidOrRefundTransaction AND
refundTransaction on the 2013-dead sub 113027706000000428 (0 rebills, 12yr past any refund window):
both returned HTTP 200 `<results>-7</results>`; a viewSubscriptionStatus control in the same session
returned a full status document (creds+IP fine). CORRECTED INTERPRETATION (Paul: both refund
subsystems ARE enabled for this DataLink user): -7 here is CCBill REFUSING THE OPERATION (transaction
too old / not refundable), NOT the subsystem being absent and NOT an auth failure. KEY FINDING: -7 is
CCBill's OVERLOADED denial code — the SAME code returned for a wrong password (Phase 0) AND for a
refund of a too-old transaction with valid creds. So classifyResultsCode(-7 → ErrDataLinkAuth) is too
narrow, and the refund handler's `-7 → Retryable("auth rejected: clean retry")` is a BUG: a
permanently-unrefundable transaction (too old) would retry forever with backoff (the runner does not
cap Retryable) and surface a misleading "auth" reason. FIX REQUIRED (handed to the refund agent):
treat a refund -7 as a DENIAL (did not execute — safe, no money moved) with BOUNDED retry →
failed_terminal + an operator-facing reason ("ccbill refund denied (-7): provider refused — not
permitted / not refundable / auth"), never infinite clean-retry; reframe the -7 doc from
"authentication rejected" to the overloaded "request denied" meaning. What the probe PROVED: the code
builds a well-formed request that reaches CCBill and the failure path moves no money. What still
CANNOT be verified without a real (small, reversible) refund: the SUCCESS result code, the amount
format (dollars vs cents), transaction targeting, and the counter increment.

**PHASE 0 VERIFIED RESULTS

**PHASE 0 VERIFIED RESULTS (2026-07-03, Paul enabled the DataLink subsystems + IP whitelist + live creds):**
- Endpoint/envelope confirmed: POST /utils/subscriptionManagement.cgi, form-encoded → HTTP 200
  `<?xml version='1.0' standalone='yes'?><results>…</results>`.
- viewSubscriptionStatus (ACTIVE, id 116206701000000779): subscriptionStatus=2, recurringSubscription=1,
  nextBillingDate=20260801 (8-digit), signupDate=20160724003047 (14-digit), timesRebilled=121,
  chargebacksIssued/refundsIssued/voidsIssued=0.
- viewSubscriptionStatus (DEAD since 2013, id 113027706000000428): subscriptionStatus=0,
  recurringSubscription=1 (!), expirationDate/cancelDate=20130131123315 (14-digit), returnsIssued=1.
- cancelSubscription success = bare `<results>1</results>` (verified on the 2013-dead sub — a no-op, no
  money, no active access affected).
- Auth/authz rejection = HTTP 200 + `<results>-7</results>` (confirmed from prior probe).
- FIXES LANDED (subscription_management.go): expiry field list → [nextBillingDate, expirationDate] (was
  guessed [expirationDate, expireDate, nextRenewalDate] — nextBillingDate was MISSING, so active-sub
  paid-through never parsed); date layouts → [20060102150405 (14-digit), 20060102 (8-digit)] (the real
  14-digit datetime was MISSING); rebill prediction keys off subscriptionStatus NOT recurringSubscription
  (a dead sub reports recurringSubscription=1); classifyResultsCode maps ONLY the verified -7 to
  ErrDataLinkAuth (no fabrication for unverified codes). Production golden tests added.
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
- [x] While probing: check whether the SMS subsystem also exposes refund/void actions (would complete #692's
      cancel_and_refund for ccbill); note availability either way. → could NOT probe the refund action live
      (real money — no live refund calls were made). Refund flow BUILT against a modeled wire
      (voidOrRefundTransaction), marked PROVISIONAL; first real refund confirms it.

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
      → cancel leg = ApplyLocalCancellation + admin-origin intent. Refund leg SUPERSEDED by Phase 3 below:
      the ccbill refund now rides the same intent path (WIRE PROVISIONAL) instead of the manual-portal error.
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

## Phase 3 — refund (write): admin "refund button" for CCBill payments (2026-07-03, WIRE PROVISIONAL)

Goal (Paul): a working refund button for CCBill in OUR admin instead of routing operators to CCBill's portal.
Built ON the existing refund infra (NMI/Stripe intents) + the #696 SMS choke point. NO live refund calls were
made (real money) — the refund wire is MODELED, marked PROVISIONAL everywhere, and pinned by unit tests.

- [x] Choke-point transport `DataLinkClient.RefundTransaction(ctx, subscriptionID, transactionID, amountCents)`
      (`subscription_management.go`) — action `voidOrRefundTransaction` (voids unsettled, else refunds; handles
      both), keyed by subscriptionId + original transactionId (precision), optional amount for partials. REUSES
      subscriptionManagementForm/parse/classifyResultsCode; ONE new form-based transport core
      (postSubscriptionManagementForm) serves view/cancel/refund. Same success(1)/auth(-7)/reject envelope as
      cancel: ErrProviderReadOnly / ErrDataLinkAuth / ErrRefundRejected / else-ambiguous. readonly blocks before
      any HTTP (verified: zero wire hits).
- [x] Intent `ccbill_refund` (`intents/refund.go`): CCBillRefundHandler mirrors NMI/Stripe refund reservation
      flow (producer reserves negative pending payment; finalize completes / release on terminal). CONTENT-
      ADDRESSED idempotency `CCBillRefundIdempotencyKey(subscription, transaction, amount)`. RefundPayload gains
      `provider_transaction_id` (CCBill needs subscriptionId AND transactionId). Registered in
      river_register buildIntentRegistry (nil client parks).
- [x] Admin API: admin_payments.go CCBill branch (prepare ~switch + issue ~switch) replaced the hard 400
      ("use CCBill's portal") with the real path — resolves subscriptionId off the subscription row
      (rail_subscription_id) + transactionId off the payment, reserves, enqueues the intent. The #692 findings-
      queue refund (admin_findings_actions.go) CCBill rejection removed too (same executeAdminRefund path).
- [x] Amount: micros→cents at the boundary (RefundPayload.AmountCents); wire amount encoded DECIMAL major units
      ("9.99"), NOT integer cents — matches CCBill's other money fields + classic FormatCentsDecimal, and is the
      SAFER guess (cents-vs-dollars mistake under-refunds/errors instead of a 100x over-refund). ONE-line
      encoding in refundForm; wire-pinned (999c → "9.99", 6000c → "60.00").
- [x] verify-not-decline: CCBill exposes only per-subscription refund COUNTERS (refundsIssued/voidsIssued), no
      per-transaction refund read. Verify reads viewSubscriptionStatus: counters KNOWN-ZERO ⇒ verified NOT
      executed (retryable); NONZERO or UNKNOWN ⇒ cannot attribute to this transaction ⇒ Ambiguous (operator
      confirms — never auto-decline nor auto-resend). Reclaimed lease (attempts>1) pre-send-verifies the same
      way. A producer-captured baseline would enable "counter incremented ⇒ succeeded" attribution (future).
- [x] -7 OVERLOADED-denial fix (2026-07-03 safe fail-probe: refund/void of a 12-yr-dead sub returned HTTP 200
      `<results>-7</results>` with VALID creds + subsystems enabled — so -7 is NOT auth-only, it is CCBill's
      generic "request denied" (auth OR operation-not-permitted / not-refundable). ErrDataLinkAuth doc +
      classifyResultsCode reframed to say so. Handlers no longer clean-retry -7 forever behind a misleading
      "auth rejected": bounded retry (ccbillDenialMaxAttempts=3, covers transient auth/IP) THEN OutcomeTerminal
      with an operator-facing reason — refund releases the reservation, cancel goes Terminal. Applied to BOTH
      the refund AND cancel handlers. The -7 op did NOT execute (safe). NO live money calls.
- [x] Breaker: refunds are NOT breaker-gated today (destructiveIntentTypes holds only cancel/delete, NOT
      TypeNMIRefund/TypeStripeRefund) — ccbill_refund stays CONSISTENT (not added). Refund rate-limiting is the
      separate #732.
- [x] Tests (build+vet+unit green, NO live calls): refund form wire-pin; response classification
      (success/auth/reject/ambiguous); readonly-zero-HTTP; counter parsing; handler classification
      (parked/auth-retryable/reject-ambiguous/transport-ambiguous/missing-txn-terminal); reclaim pre-send-verify;
      Verify (counters zero→retryable, nonzero/unknown→ambiguous, read-fail→ambiguous); RefundIntentFor ccbill
      routing; admin ccbillRefundTarget resolution + guards. Full-path success→finalize needs DB (integration,
      not run here).
- [ ] FIRST REAL REFUND MUST VERIFY (before trusting the wire / closing #696): (1) success + already-refunded +
      partial result codes for voidOrRefundTransaction; (2) the amount format — DECIMAL DOLLARS assumed vs cents
      (the single riskiest field: fix refundForm's one line + re-pin a golden if wrong); (3) that transactionId
      narrows the refund to THAT charge (vs CCBill refunding the latest); (4) whether refundsIssued/voidsIssued
      increment on success (the verify signal). Then add a production golden test like the read/cancel ones.

## Out of scope

- Refund rate-limiting / breaker gating (separate #732; refunds are un-gated today, ccbill matches).
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

# #733: merchant analytics API — three-stream metrics (subscriptions / one-time / usage-credits)

**Completed:** no
**Status:** planned (2026-07-03). Replaces the fixed-section admin metrics surface (#232/#528:
{summary, revenue, subscriptions, rails, churn}) with ONE composable, per-merchant analytics query
endpoint — a small in-house semantic layer over a fixed star schema. A merchant has up to three
revenue streams — subscriptions, one-time products, and the API platform (credit purchase + usage
draw-down) — and every dashboard tile any of them wants is a point in one cube: measures × dimensions
× time-grain. Instead of a route (or named section) per metric, the frontend composes its own queries
by picking from a fixed measure/dimension REGISTRY; the server owns all SQL.

## Metadata
- Category: product / analytics
- Status: planned
- Passes: false

## Design doctrine

- ONE composable endpoint, NOT one route per metric and NOT named sections. `GET /admin/metrics/query`
  takes measures + group-by dimensions + filters + date range + grain and returns rows.
- SEMANTIC LAYER, NOT a query language. The frontend speaks business nouns (measure/dimension NAMES);
  it never sends SQL, column names, or raw expressions. Safety is an allowlist lookup, not a parser:
  1. `measure` / `by` / `filter` keys are resolved through a fixed in-code REGISTRY (name → SQL
     expression + aggregation semantics). Any unknown name → 400. No client string ever becomes a
     column or SQL fragment.
  2. `WHERE merchant_id = ?` is injected server-side on EVERY query, non-overridable, non-visible to
     the client (merchant.Require). No cross-merchant reads — ever.
  3. Filter VALUES are parameterized; operators are allowlisted; `limit` clamped; grain is an enum.
  Result: the whole request compiles to ONE parameterized query.
- MEASURES CARRY THEIR AGGREGATION TYPE (this is why the registry must be code, not arbitrary columns):
  - `additive` — SUM/COUNT over the grain (net_revenue, new_subscriptions, …).
  - `ratio` — numerator/denominator computed AFTER grouping (churn_rate, approval_rate, arpu, …); the
    engine aggregates the two components, then divides per row. Never averages an average.
  - `snapshot` — point-in-time / last-value, NEVER summed across days (mrr, active_subscriptions,
    outstanding_credit_liability, …). An arbitrary-column API would silently `SUM(mrr)` over 30 days
    and report garbage; the registry prevents that structurally.
- TWO DATASETS, one per source of truth. A query targets ONE dataset; mixing measures across datasets
  in a single call → 400.
  - `events` (ClickHouse `daily_metrics` rollup) — all flow/series/count measures. Display-only and
    optional: a CH outage degrades this endpoint, never blocks openrails.
  - `balances` (Postgres ledger, point-in-time) — outstanding credit liability, arrears AR. NEVER
    derive a balance from the event stream (the existing "ClickHouse is never truth" boundary).
- Prepaid credits: cash-in ≠ revenue-earned. `credits_sold` (cash) and `usage_revenue` (consumed =
  recognized) are DISTINCT measures; deferred revenue = unconsumed lots, a `balances` snapshot.

## API shape (illustrative)

```
GET /admin/metrics/query
  ?dataset=events                       # events | balances (default events)
  &measure=net_revenue,new_subscriptions
  &by=day,rail                          # group-by dimensions (incl. time grain as `day/week/month/…`)
  &from=2026-06-01&to=2026-06-30
  &filter=rail:ccbill;currency:USD      # allowlisted dim:value, parameterized
  &order=net_revenue.desc&limit=100
```
Response: `{ columns: [...], rows: [...], grain, currency_mode, data_fresh_as_of }`. Multi-currency
stays explicit — `currency` is a normal dimension; a query spanning currencies without `by=currency`
or a `currency:` filter is either grouped or 400 (same ambiguity rule as #528, enforced in the engine).

## Registry v1 (the fixed vocabulary the endpoint ships with)

MEASURES —
- additive money: `gross_revenue`, `net_revenue`, `subscription_revenue`, `one_time_revenue`,
  `usage_revenue`, `credits_sold`, `refunds`, `chargebacks`, `breakage`.
- additive counts: `payment_count`, `payment_attempts`, `payment_failures`, `new_subscriptions`,
  `cancellations`, `reactivations`, `entitlement_grants`, `units_sold`, `chargeback_count`,
  `refund_count`.
- ratio: `arpu`, `churn_rate`, `approval_rate`, `chargeback_rate`, `refund_rate`, `recovery_rate`
  (dunning), `ltv`.
- snapshot (events): `mrr`, `arr`, `active_subscriptions`.
- snapshot (balances dataset): `outstanding_credit_liability`, `outstanding_owed`.

DIMENSIONS — `time`(grain day/week/month/quarter/year), `currency`, `rail`, `stream`
(subscription/one_time/usage), `product_id`, `price_id`, `plan`, `billing_cycle`, `cancel_type`
(voluntary/involuntary/expired/chargeback), `status`, `event_type`, `payer`, `sku`/`rate_card`.

Every dashboard view is then a query, no bespoke endpoint:
- KPI row → measures `mrr,arr,net_revenue,churn_rate` with prior-period compare.
- Revenue-over-time stacked by stream → `net_revenue` by `week,stream`.
- Dunning/recovery → `payment_failures,recovery_rate` by `week`, filter subscription stream.
- Payment health → `approval_rate,chargeback_rate,refund_rate` by `rail`.
- Product mix → `net_revenue,units_sold` by `product_id`, order desc.
- Usage → `credits_sold,usage_revenue,breakage` by `week,sku`; top payers → `usage_revenue` by
  `payer` order desc limit 10.
- Deferred revenue / AR → dataset `balances`, `outstanding_credit_liability` by `currency`.

## Scope

Query engine (the new read surface):
- [ ] Measure/dimension REGISTRY (in-code): name → {dataset, aggregation type, SQL expr, allowed as
      dimension?}. Single source of truth; the endpoint's whole vocabulary.
- [ ] Query compiler: params → validated plan → ONE parameterized query per dataset. Enforces
      merchant_id injection, allowlist resolution, ratio post-aggregation, snapshot last-value,
      single-dataset rule, currency-ambiguity rule, clamped limit, enum grain.
- [ ] `GET /admin/metrics/query` handler on the existing per-merchant admin surface; column/row
      response shape; prior-period compare param.
- [ ] Registry-driven capability endpoint (`GET /admin/metrics/query/schema`) so the frontend can
      discover available measures/dimensions/grains instead of hardcoding — makes it truly
      self-composing.

Rollup + schema work (the data the registry reads; unchanged by the API redesign):
- [ ] Extend `daily_metrics` with the missing dimensions/measures: `stream`, `product_id`/`price_id`,
      `payer`, `sku`/`rate_card`; dunning columns (declines, in-retry, recovered-vs-lost — data
      already flows via `charge_failure`, `subscription_past_due`, cancel-type events); usage columns
      (`credits_sold`, `usage_revenue`/consumed, `breakage` — ACU events already flow); per-rail
      failure counts (for approval/chargeback/refund rates).
- [ ] `balances` dataset: Postgres ledger aggregates for `outstanding_credit_liability` (unconsumed
      grant lots = deferred revenue) and `outstanding_owed` (arrears AR + simple aging), point-in-time,
      merchant-scoped.
- [ ] Micros cleanup (do it in this same schema change, not a second migration): analytics events
      carry money as float64 (`PriceAmount`, `Amount`) and CH columns are named `_cents` — move to
      typed micros end-to-end and rename columns to match doctrine.

Tests:
- [ ] Engine unit-ish coverage on the compiler: allowlist rejection (unknown measure/dimension → 400),
      merchant_id always injected, ratio vs snapshot vs additive aggregation correctness, cross-dataset
      rejection, currency-ambiguity rule.
- [ ] Integration tests (testcontainers CH+PG) exercising real queries across the registry, including
      merchant-isolation (follow `admin_metrics_merchant_isolation_integration_test.go`): confirm one
      merchant's query can never read another's rows.
- [ ] `balances` verified against ledger truth (seed grants/usage/owed in PG, assert API numbers match
      the ledger, not the event stream).

Migration note:
- [ ] This SUPERSEDES the fixed `/admin/metrics` sections (#232/#528). Greenfield / pre-launch — hard
      cut to `/query`, no compatibility shim for the old section response (house style). The frontend
      dashboard moves to composed queries.

Explicitly out of scope:
- Arbitrary SQL / raw-expression passthrough — the registry allowlist is the hard boundary; if a true
  ad-hoc need appears later it's a per-merchant event EXPORT (CSV), never an open query language.
- Joins / arbitrary multi-dataset queries in one call — one dataset per query; the frontend stitches.
- Cohort retention curves + a dedicated LTV endpoint (LTV ≈ arpu ÷ churn_rate composed client-side);
  add a `cohorts` dataset when a merchant actually asks.
- Entity/list queries (subscriber search, failed-payment lists) — admin CRUD surface, not metrics.
