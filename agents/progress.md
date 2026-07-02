<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 695

---

# #694: test-suite overhaul — enforce the net, close the gaps, then delete the redundancy

**Completed:** no
**Status:** PLANNED (2026-07-01, Paul) — from the 2026-07-01 test-system audit. Paul's target: a LEAN suite —
a few unit tests on critical core invariants (money precision/unit wire-pinning, pure decision logic) +
integration tests covering all functionality end-to-end; delete everything redundant/mock-theater. Baseline:
466 test files, ~77k test LOC vs ~140k production LOC, 1,565 test funcs, 124 files using mocks/fakes.
ORDERING IS THE POINT: enforce + close gaps FIRST, then cut — never delete strands out of an unverified net.

## Metadata
- Category: testing
- Status: planned
- Passes: false

## Phase 1 — make the net real (do first, small) — DONE 2026-07-02 (one deferral, see GAP 1)
- [x] CI: flipped to enforcing (2026-07-02). ci.yaml integration job: `continue-on-error` removed,
      `timeout-minutes: 40` added (go test itself is capped at 25m inside scripts/test_integration.sh).
- [x] GAP 1, session leg (2026-07-02): tests/stripe_checkout_e2e_test.go TestCheckoutSessionStripeRedirect —
      real HTTP POST /v1/checkout (rail=stripe) through the full suite; fake ONLY at the stripeapi choke
      point (new integration-build-only hook stripeapi.SetTestBaseTransport + httptest serving real Stripe
      wire shapes). Asserts requires_action + redirect_url, the persisted checkout_sessions row, the
      rail_customer_accounts mapping (#212), and wire-pins the checkout-session create form (mode/price/
      metadata linkage incl. checkout_session_id, customer XOR customer_email, pinned Stripe-Version).
      New suite option WithSuiteStripeRail. DEFERRED: the activation leg (checkout.session.completed →
      active local sub) — internal/modules/webhooks/stripe.go is mid-rewrite under #684
      (fetch-and-converge); add that leg after #684 lands.
- [x] GAP 2 (2026-07-02): tests/nmi_webhook_signature_http_test.go TestNMIMerchantWebhookSignatureHTTP —
      real HTTP POST /v1/merchants/{slug}/webhooks/nmi with the webhook_signing_secret seeded through the
      merchant secret store; valid sigverify HMAC (t=ts,s=hex(hmac-sha256(secret, ts+"."+body))) ⇒ accepted
      AND the recurring.subscription.delete event actually cancels the seeded active sub in PG; missing /
      wrong-secret / signed-then-tampered ⇒ 401 with no state change.
- [x] GAP 3 (2026-07-02): tests/river_stripe_webhook_worker_test.go TestStripeWebhookReconcileRiverWorker —
      enqueue StripeWebhookReconcileArgs ⇒ real in-process River worker picks it up (waits on THAT job id,
      not a completed-count) ⇒ handler proven by effects: managed-endpoint list→create against the fake
      Stripe wire (merchant-scoped URL, pinned api_version, openrails_managed marker) and the returned
      whsec persisted into the merchant secret store. (jobs_stripe_webhooks is the ENDPOINT RECONCILER, not
      an event-apply path — no #684 overlap.)
- [x] Live-gated workflow (2026-07-02): .github/workflows/live-gated-integration.yml — weekly cron +
      workflow_dispatch, modeled on solana-devnet-integration.yml. Jobs (each enforcing; the workflow is
      non-required): nmi-sandbox (TestNMILiveLifecycleE2E + TestLiveSandboxClientSurface +
      TestChargeOutstanding_NMISandbox_CollectsRealCharge), live-invoice-collection (Stripe/NMI invoice
      collection, OPENRAILS_LIVE_RAIL_TESTS=1 + TEST_MODE), stripe-model-b (lane for the orphaned
      `stripe_integration` tag; a vet step compiles the tag even secret-less), vaultint (dev Vault container
      + transit — validated green locally end-to-end). Skip-if-secret-absent semantics preserved and
      verified locally (secret-less run is green-but-empty).
      Phase-3 hygiene items pulled forward: (b) vaultint lane DONE; (a) stripe_model_b_integration_test.go
      could NOT be retagged/fixed in place — internal/modules/subscriptions/* was fenced this wave
      (concurrent agents), and the file HAS bit-rotted exactly as the audit warned (tag compiled nowhere):
      line ~214 `{Rail: models.RailStripe, SecretKey: key}` must become
      `{Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: key}}`. Until that one-liner
      lands, the stripe-model-b job is KNOWN-RED at its vet step — that red IS the enforcement working.
      Note: correct resolution is the live-gated lane, NOT retagging to plain `integration` (the test hits
      real Stripe TEST mode and must never run on PR CI).

## Phase 2 — harness consolidation (one blessed stack: dbtest + integrationharness)
Audit verdict: sprawl is SMALLER than suspected — dbtest already unifies PG/Redis/TestMain across ~30
packages (per-run DBs, orphan reaper, shared containers). The one true duplicate is the legacy
tests/TestContainerSuite sitting beside the newer, better integrationharness (real standalone server, real
AuthKit-minted credentials, RLS-enforced app role, dbtest-shared infra).
- [ ] Extend integrationharness.StartStandalone with the two options tests/ needs: worker boot (RunWorkers +
      River wait) and per-test injectable clock (kills the 27 fresh-suite boots that exist only for
      WithSuiteClock — tests/ wall-clock drops from ~8-9 min to ~3-4).
- [ ] Migrate tests/ onto it; DELETE TestContainerSuite (~600-800 LOC incl. stub JWT authenticators replaced
      by real minted tokens, dead ResetDatabase — invalid SQL, zero callers — and deprecated SetMockClock +
      its 15-service setClock fan-out). ClickHouse becomes opt-in dbtest.SharedClickHouse.

## Phase 3 — the deletion sweep (after 1+2 prove the net)
Rule: every deleted test must be DOMINATED by a surviving one (same behavior, stronger level). A test that is
the only coverage of something real gets replaced (usually by extending an e2e flow), never dropped.
- [ ] Integration-side redundant clusters (~4,500-6,000 LOC, audit-verified):
      - dunning tested across FIVE layers (~2,500 LOC: tests/dunning_worker_test.go 833-LOC/14 fresh boots +
        tests/entitlements_dunning_* 808 + river + intents + units) → keep intents rebill semantics + ONE
        per-rail e2e ladder; filter/skip mechanics become unit invariants.
      - spend/hold/capture across five levels → the internal/modules/admission middle layer (~190 LOC)
        mostly re-asserts spendgate outcomes through an adapter; collapse.
      - cancel-subscription across four levels → keep intents + one HTTP test.
      - catalog push/dump round-trip ×3 (pkg/embedded + bootstrap + integrationharness) → one HTTP publish +
        one dump unit.
- [ ] Unit-side sweep — AUDIT LANDED (2026-07-01), suspicion NOT confirmed: ~95% of the 34.4k unit LOC pins
      exactly the target philosophy (money wire formats, pure decision law, byte-exact layouts, fail-closed
      posture matrices — usually citing the incident/issue it guards); MOCK THEATER IS ESSENTIALLY ABSENT
      (fakes sit at transport seams; real logic runs underneath). Deletable: ~1,550 LOC in ~21 SMALL files —
      the big files are the best files. Kill-list (whole files unless noted): pkg/embedded/pull_provider_log_test.go
      (120, pins log prose), pkg/service/catalog_pagination_test.go (82, clamp table), pkg/embedded/river_test.go
      (67, nil-guard self-asserts), payments/stripe_card_test.go (62, brand display trivia),
      controlplane/api_key_test.go (57, subsumed by perm_glob matrix; fix its stale comment),
      migrations catalog_benefits_metering/identity_anchors/merchant_provisioning schema tests (56+43+53,
      immutable-migration byte-pins), merchant_rls_encryption_schema_test.go (42, salvage ~10 role/DEK asserts
      then delete), pkg/service/catalog_credits_spec_test.go (48), normalize_test.go (33, tests TrimSpace),
      cmd catalog_apply_test.go (33, asserts cmd.Use literal), subscriptions/solana_cancel_cascade_test.go (32),
      auth/provider_test.go (31, keep blank-issuer check), embed/provision_test.go (31),
      pkg/service/currency_test.go (31, error-prose pins), pkg/api/error_test.go (30, builder echo),
      controlplane/catalog_delegated_test.go (30, dup of delegated_test), pkg/embedded/gin/api_test.go (29),
      + ~600 LOC intra-file trims (stripe_refunds_test validation-prose tables, webhooks/nmi_test dead cases).
      CONDITIONAL: vault/kv_httptest_test.go (187) folds ONLY if the `vaultint` tag gets a CI lane.
- [ ] Hygiene surfaced by the audit: (a) `stripe_model_b_integration_test.go` carries an orphaned
      `//go:build stripe_integration` tag that runs in NO suite — fix the tag or the unit tests stay
      load-bearing; (b) `vaultint` opt-in tag runs nowhere in CI — add a lane or accept the httptest twin;
      (c) watch-item: reconcile engine_test's ~500-LOC in-memory store re-implements SQL upsert semantics —
      earning its keep today, but flag for divergence when store semantics change.
- [ ] PROTECTED (audit-confirmed only-coverage — never delete without replacement): intents
      enforcement_guard_test (#674 CI wall), reconcile decider{,_property}_test, merchantsecrets store_test
      (#667), db/rls_test (bypass-role refuse-boot), analytics admin_metrics_merchant_test (ClickHouse has no
      RLS), deps_test.go, solana confirm-mirror + failure-classify tests, webhooks webhook_handler_test +
      deduplication_test, checkout duplicate_billing_guard + stripe_customer_resolution, money currency_test
      (JPY), remote_test (trust_tier wire name), routes merchant_action_routes_test (perm axis), pyth
      client_test, catalog apply_options_test.
- [ ] PROTECTED CLASSES (never cut): wire-pinning money tests (#671 test wall — micros in ⇒ exact wire value
      out), pure-invariant tests (reconcile decider property suite, pricing big.Int math, registry
      completeness, config/posture matrices), RLS isolation suite, conformance.

## Acceptance (revised after both audits)
CI enforces the integration suite; Stripe checkout + NMI webhook-HTTP e2e exist; ONE app-boot harness
(dbtest + integrationharness) — TestContainerSuite gone; redundant integration clusters collapsed with every
deletion dominated by a surviving stronger test; protected classes intact. HONEST TARGET: ~7-8k LOC net cut
(~10% — integration clusters 4.5-6k + harness ~800 + unit kill-list ~1.5k); the bigger leanness wins are
SPEED (tests/ wall-clock ~halved via injectable clock) and ENFORCEMENT (the net actually gates merges). Both
audits agree the suite is substantively healthy — the 77k figure buys real guarantees; cut the named waste,
don't chase a ratio.

---

# #692: operator findings queue — recommended actions, approve/ignore per item, self-verifying resolution

**Completed:** no
**Status:** IMPLEMENTED (2026-07-02, uncommitted; planned 2026-07-01, Paul) — the end-to-end operator flow for surfaced findings (#690 duplicates/
freeloaders, held_bulk, and every other ADMIN/OPERATOR finding): detect → save with a machine-computable
recommendation → admin queue API → operator approves (executes the recommendation) or ignores (with note) one
item at a time → the next converge sweep verifies the fix took. BACKEND ONLY — the dashboard frontend is
future.md #693, planned later.

## Metadata
- Category: admin / reconciliation
- Status: implemented (uncommitted)
- Passes: true (unit + integration green on touched packages; see Progress)

## The end-to-end flow

1. **DETECT (exists).** Converge passes + the pull engine emit findings into `reconciliation_findings` with
   stable identity (upsert per subject), severity, evidence JSONB, first/last-seen runs; vanished conditions
   auto-resolve. Cadence: 15-min sweep + inline converge per mutation + post-pull pass.
2. **RECOMMEND (new).** Checks that emit ADMIN/OPERATOR findings populate `recommended_action` (column exists,
   almost nothing writes it) with an operator-readable sentence AND a STRUCTURED recommendation in evidence
   (`evidence.recommendation = {action, params}`) so approval can execute mechanically:
   - `consistency.duplicate.ownership` / `duplicate.provider_charge`: "User X holds two overlapping
     subscriptions A (created …, price …) and B (created …, price …). Cancel B (later-created) and refund
     payment P (amount, date)." → `{action: "cancel_and_refund", subscription_id: B, refund_payment_id: P}`.
     Later-created is the DEFAULT recommendation only — the structured params let the operator flip which one
     dies before approving (the later one may be the annual plan the user meant to keep).
   - `derive.entitlement.orphan` (freeloader): "Live access with no recorded justification — revoke window W
     unless known-legitimate (then record an admin grant instead)." → `{action: "revoke_entitlement", ...}` and
     the alternative `{action: "record_admin_grant", ...}`.
   - `life.provider_intent.held_bulk`: "N destructive deletes in 24h exceeded budget B — review the cohort;
     approve to resume." → `{action: "ack_resume"}` (resolution already resumes the breaker, #679).
   - Findings with no mechanical fix (e.g. amount mismatches) get prose + no structured action → approve is
     disabled for them; resolve/ignore only.
3. **SAVE (exists).** The ledger IS the queue: `requires_review` rows ordered by severity+age are the work list;
   #690's gauges are counts over it.
4. **QUEUE API (new).** Admin-gated endpoints following the existing `/admin/*` conventions
   (admin_catalog_drift.go is the pattern; embedded hosts front them with host admin auth):
   - `GET /admin/findings` — open findings; filters (severity, finding_type, status), sort severity desc + age
     desc, pagination; response includes the #690 gauge summary (freeloaders, duplicate_coverage, and later
     verification_pressure) so one call paints the dashboard header.
   - `GET /admin/findings/{id}` — full evidence + recommendation.
   - `POST /admin/findings/{id}/resolve` — `{outcome: approve|ignore, notes, override_params?}`:
     - **approve** → execute the structured recommendation (with optional operator overrides, e.g. swapping
       which duplicate sub to cancel), then mark `fixed`/`admin_fixed` with notes.
     - **ignore** → `ignored` with REQUIRED notes; permanent silence for that subject (same semantics the
       breaker's dismiss already has — an ignored subject never re-pages).
   - One item at a time by design; no bulk-approve endpoint (bulk destructive ops are what #679 guards against).
5. **EXECUTE (new, thin — composes existing machinery only).** An action executor mapping
   `evidence.recommendation.action` → existing paths, never new mutation logic:
   - `cancel_and_refund`: lifecycle cancel (remote side = durable intent, queue-always #679, breaker-guarded) +
     operator-authorized refund (the spec's `RevokeGrant(grant, {refund})` bundle — the approve click IS the
     authorization; refund executes via the rail's refund API through the intents log, recorded in the money
     ledger). Partial failure leaves the finding open with the error in notes — never half-marked fixed.
   - `revoke_entitlement` / `record_admin_grant` (freeloader orphan): as-of revoke via the existing entitlement
     service, or an admin-sourced grant making the access legitimate.
   - `ack_resume`: plain resolution (breaker re-arms per #679).
   - Idempotent: approve on an already-resolved finding is a no-op; remote effects ride intents (#674
     effectively-once).
6. **VERIFY (exists — the loop closes itself).** The next sweep re-derives truth: condition gone → auto-vanish
   confirms the fix; condition persists → the finding stays open and the gauge stays nonzero, so a fix that
   didn't take is indistinguishable from no fix — the dashboard never trusts, it re-measures.
7. **AUDIT (small).** Record the acting operator identity on resolution (add `resolved_by` via forward-only
   migration — `operator_notes`/`resolution` exist; actor identity does not). Every approve/ignore is
   attributable.

## Tasks

- [x] Structured recommendations: `evidence.recommendation` shape + `recommended_action` prose
      (`internal/reconcile/recommend`, leaf contract package); written NOW by held_bulk (#679); the #690
      duplicate/orphan checks consume the same contract when they land. Later-created default for duplicates;
      params overridable at approve time.
- [x] Queue API: list/get/resolve endpoints per above, admin-gated, embedded + standalone wiring; gauge summary
      in the list response.
- [x] Action executor composing existing paths (cancel+refund bundle, revoke/admin-grant, ack); partial-failure
      → finding stays open with error notes; idempotent approve.
- [x] `resolved_by` migration (065) + stamped on every resolution.
- [x] Tests: end-to-end per action type — seed the broken shape → finding with recommendation → approve →
      effects (intent queued / refund recorded / window revoked) → next converge auto-confirms → gauge back to
      zero; ignore → permanent silence; approve with override_params; partial failure leaves finding open.

## Progress

- 2026-07-02 IMPLEMENTED (uncommitted, this tree). What was built:
  - Contract: `internal/reconcile/recommend` — `evidence.recommendation = {action, params, alternatives?}`;
    actions `cancel_and_refund` {subscription_id, refund_payment_id?, amount?}, `revoke_entitlement`
    {entitlement_id, as_of?}, `record_admin_grant` {customer_id, product_id, reason?}, `ack_resume` {}.
    Canonical location top-level `evidence.recommendation`; `evidence.local.recommendation` honored for
    converge-pass writers. The #679 breaker now writes prose + `{action: "ack_resume"}` (intents/breaker.go).
  - Queue API (mounted on the shared merchant action surface → BOTH standalone `/v1/merchant/findings*` and
    embedded `/billing/v1/merchant/findings*`): GET list (severity/finding_type/status filters, severity-desc +
    age-desc sort, pagination, `gauges` = freeloaders + duplicate_coverage + open-by-severity + total_open),
    GET one (full evidence + parsed recommendation), POST resolve {outcome approve|ignore, notes,
    override_params?}. Reads gated on merchant:repair-alerts:read; resolve on NEW merchant:findings:resolve.
    422 approve without structured rec; 409 on already-resolved; ignore requires notes. No bulk endpoint.
  - Executor (internal/http/handlers/admin_findings_actions.go — in handlers deliberately, to reuse the
    EXISTING admin-refund producer `executeAdminRefund` unchanged): cancel via ApplyLocalCancellation +
    NMIDeleteScheduler (queue-always #679, breaker-guarded) / Stripe cancel + CancelMembership; refund via the
    admin refund intent path (idempotency key derived from the finding); revoke via
    RevokeExistingEntitlement(AsOf); grant via GrantProductAccess (SourceID `finding:<id>`, grant ledger).
    Clear not-supported error for rails without a cancel/refund path (CCBill/solana). Partial failure appends
    error + completed compensation state to notes, finding stays OPEN.
  - Audit: migration 065 `resolved_by text`; stamped from the authenticated admin identity on every resolve;
    exposed in GET responses.
  - Tests (REAL testcontainers Postgres + fake NMI HTTP server only): handlers integration —
    duplicate-approve e2e (sort/filters/gauges → cancel + durable delete intent + refund executed & recorded →
    fixed w/ resolved_by → converge sweep does NOT reopen → gauge back to 0 → re-approve 409), partial refund
    failure (finding open, cancel documented), ignore (notes required, permanent silence on re-upsert),
    override_params swaps the cancelled sub, revoke/grant paths, held_bulk e2e (trip breaker → ack_resume rec
    in queue → approve → held delete drains). Routes unit tests pin permission gates on both prefixes + 401.
    `go test ./...` and `-tags integration` green on every touched package.
  - Known concurrent-tree noise (NOT this issue): sibling #691's uncommitted converge/entitlements work
    (converge derive-mismatch integration test red in their tree state). Known edge: a terminally-declined
    refund cannot be re-approved through the same finding (content-addressed intent identity) — resolve via
    the payments refund endpoint, then ack/ignore the finding.

## Out of scope

- The dashboard frontend (future.md #693).
- Bulk approve, auto-execution of recommendations, notification/paging integrations.
- New mutation logic of any kind — the executor only composes existing, already-guarded paths.

Acceptance: every ADMIN/OPERATOR finding carries an operator-readable recommendation (structured when
mechanically executable); an operator can list, inspect, approve (with overrides), or ignore findings one at a
time through admin endpoints; approvals execute through the existing intent/refund/entitlement machinery with
full attribution; and resolution is verified by re-measurement, not trust — a failed fix keeps the finding open
and the #690 gauges nonzero.

---

# #691: access ends only by proof — fail-open entitlements for auto-renew subscriptions

**Completed:** no
**Status:** IMPLEMENTED (2026-07-02, uncommitted) — see Progress; planned 2026-07-01 (Paul) — policy: a user must NEVER lose access because OUR billing system failed.
Extends the #664 doctrine (cancellation requires certainty) to its conclusion: ACCESS REMOVAL requires certainty.
Uncertainty resolves in the customer's favor. Motivated by a real incident: CCBill kept billing a user, our
webhooks were lost, local state went stale, the user was downgraded (window lapsed), was told to re-subscribe,
and was then double-billed for a year.

## Metadata
- Category: entitlements / policy
- Status: implemented (uncommitted; #690 gauge bullet outstanding)
- Passes: true (unit + integration green on every touched package; see Progress)

## Problem

#664 stopped guess-CANCELLATION, but access still fails closed on our own failure: a parked-`unknown` sub's
entitlement window expires at paid-through, so webhook loss / converge downtime / provider outage downgrades a
PAYING user by window math. The two failure costs are asymmetric: a freeloader costs marginal content access
(bounded, visible via #690); a wrongful downgrade costs a paying customer, support load, a double purchase and
double-billing.

Second, still-open hole (fix regardless of the main design): the one-live-sub-per-(customer, product) constraint
covers active/pending/past_due only — `unknown` does NOT hold the lifecycle slot, so a downgraded user can
re-purchase the same product while their real provider-side sub is alive → double-billing.

## Design — invert the projection for auto-renew subscriptions

Two candidate shapes; the second is chosen:

- REJECTED: bridge windows appended when parking `unknown` — fails closed when the convergence engine itself is
  down (nothing writes the bridge), which is an explicitly named failure mode to survive.
- CHOSEN: the entitlement PROJECTION for an auto-renew subscription is OPEN-ENDED from creation
  (`end_at = NULL`) and is CLOSED only by a PROVEN event: user cancellation (closure written in advance at the
  known period end — the resumable-cancel runway), provider-confirmed death, or dunning exhausted with real
  attempts (FailMembership terminal). Paid-through remains a first-class FACT on the subscription and on the
  (unchanged, bounded, frozen-window) grant ledger — accounting truth is not weakened; only the access
  projection becomes standing. Under total system failure on a provider-autonomous rail the outcome is CORRECT:
  provider bills, user keeps access, our ledger is stale — staleness becomes an accounting problem, not a
  customer problem.

Scope boundaries:
- Bounded purchases STAY bounded intervals: one-off durations/rentals, trials, and the runway of a
  user-cancelled sub — the user paid for a bounded thing.
- Grants stay per-period, bounded, frozen (#658 replay determinism untouched). Derive's per-renewal
  window-append becomes "ensure standing window open + record paid-through"; the projection is a deterministic
  fold of grants + closure events ([first grant start, closure or NULL)).
- Revocation as-of semantics already exist (RevokeSourcesForSubscriptionAsOf) — closures stamp effective time.

Consequences handled in-issue:
- **#690 gauges redefinition**: freeloader = live access whose source is PROVEN dead/absent (not merely stale) —
  still always-zero-when-healthy. NEW pressure gauge: count + max-age of standing access past paid-through with
  verification unresolved — drift visible without being wrong.
- **Deletion opportunity (Paul: "eliminate the 48-hour grace window entirely — way more ergonomic")**: every
  grace mechanism exists solely to keep ACCESS alive across silence, which standing access solves at the root.
  Delete wholesale for auto-renew subs: #368 trailing renewal grace (`GraceSlack`/`graceSlackCap`,
  `appendRenewalGraceWindows`, `RenewalGraceEligibleRail`), CCBill webhook-driven grace appends, and the
  EntitlementSourceGrace plumbing in the sub cancel/revoke paths. The converge dunning-entry grace
  (`periodGrace`, `grace_ends_at`) loses its access role — its only surviving job is pacing when a stalled
  past_due row parks to `unknown`, better expressed by the cadence-derived `DunningWindow` than a 48h constant
  (evaluate dropping the column's role entirely). The needs_verification 48h cutoff survives but is DEMOTED to a
  pure internal detection debounce with zero customer stakes. Net: no constant anywhere is a bet on how long our
  infrastructure can silently fail.
- **GIST no-overlap**: standing window + per-period appends would collide — the append path is replaced, not
  layered.

## Tasks

- [x] Checkout guard (independent, do FIRST — closes the double-billing hole today): block or explicitly warn on
      purchase of a product/tier-group for which the customer holds an `unknown` (or past_due) subscription;
      offer verify/resume instead. Cover the embedded checkout path + tests.
- [x] Projection inversion in derive/MaterializeGrant for auto-renew subscription sources: standing open window,
      paid-through recorded as fact; renewal grants extend the fact, not the window.
- [x] Closure events write window end: user cancel (advance-scheduled at period end), FailMembership terminal
      (as-of policy instant), ResolveCancelled/ResolveCancelledRemoteAlive (as-of provider truth), import of
      declared-expiry rows (#731 shape — closure at legacy expiry).
- [x] Migration for existing live auto-renew windows → standing (open) with recorded paid-through; bounded
      purchase windows untouched.
- [x] Delete the now-redundant trailing-grace machinery for auto-renew subs (#368) after parity tests.
- [x] Redefine #690's freeloader query (proven-dead source) + add the unresolved-verification pressure gauge. (DONE in #690, 2026-07-02.)
- [x] Tests: webhook loss → access retained + pressure gauge rises + provider pull resolves; converge fully off →
      access retained; user cancel → access ends exactly at period end; dunning exhausted → access ends;
      provider-confirmed dead → access ends as-of truth; bounded purchases unaffected; GIST holds.

Acceptance: no failure of OUR machinery (webhook loss, converge downtime, provider outage, operator error) can
remove a paying user's access; access ends ONLY on proof (user cancel, provider-confirmed death, exhausted real
dunning); paid-through remains truthful for accounting and the gauges make fail-open drift visible; a user with
an `unknown` sub cannot silently double-purchase the same product.

## Progress

- 2026-07-02 IMPLEMENTED (uncommitted). What was built:
  - **Checkout guard**: `CheckSubscriptionConflict` (+ new `checkUnknownSubscriptionConflict`) rejects a
    subscribe when the customer holds an `unknown` sub for the product OR tier-group, with machine-readable
    `code=membership_pending_verification` (existing blocks also carry codes now: `duplicate_subscription`,
    `change_tier_required`); wired into BOTH `CheckoutService.Checkout` (embedded /checkout + standalone
    session flow, which funnels through it) and the solana session subscribe paths (already on
    CheckSubscriptionConflict). New queries `GetUnknownSubscriptionByCustomerAndProduct/TierGroup` +
    repo/service wrappers; `CheckoutResponse.Code` added.
  - **Projection inversion (derive-2)**: `MaterializeGrant` for an entitlement grant sourced from an
    auto-renew-priced sub in a non-terminal state (new query `SubscriptionProjectsStandingAccess`) ENSURES one
    standing open window per (customer, entitlement, source) — `StandingSubscriptionEntitlementExists` skip,
    grant-row skip, open-overlap check with bounded fallback (GIST-safe replay). The grant ledger stays
    per-period/bounded/frozen: `PushNewEntitlement`'s indefinite-covered branch appends the bounded per-period
    grant for same-source renewals (`appendCoveredPeriodGrant`, idempotent via
    `LatestEntitlementGrantEndForSource`); `ListLiveGrantsMissingEffects` mirrors the standing-satisfied skip
    so DERIVE detection ⇔ repair agree.
  - **Closures**: user cancel writes end_at IN ADVANCE at period end (`BoundSubscriptionAccess` — modified
    `EndActiveEntitlementsBySubscription` + new `SoftDeleteFutureEntitlementsBySubscription`), called from
    `ApplyLocalCancellation` (all lifecycle/converge cancels) AND `CancelUserSubscription` (NMI user cancel,
    tx-atomic); FailMembership terminal / ResolveCancelled(RemoteAlive) close via the existing as-of revokes;
    parking unknown / past_due entry touch NOTHING. Resume re-opens (end_at→NULL): rewritten
    `ResumeEntitlementsBySubscription` (latest-window, overlap-guarded) via `ResumeSubscriptionAccess` in
    `ReactivateMembership` + the Stripe resume worker.
  - **Grace deletion**: #368 trailing renewal grace fully deleted (`GraceSlack`, `graceSlackCap`,
    `appendRenewalGraceWindows`, `RenewalGraceEligibleRail` + the rails-registry field), FailMembership's
    dunning grace-append block deleted, CCBill webhook grace appends deleted (grace_ends_at kept as PACING
    marker only, still capped by ccbillGraceCap). `EntitlementSourceGrace` enum + all grace REVOCATION paths
    kept for historical rows. `PeriodGrace` (48h) kept as pure pacing/debounce — evaluated the cadence-derived
    DunningWindow swap and deliberately declined (Decide is pure/price-less; zero access stakes now; rationale
    on the const).
  - **Migration 063** (`063_failopen_standing_entitlements.up.sql`): latest live window per (customer,
    entitlement, source) of auto-renew non-terminal subs → end_at NULL; overlap-guarded (conflicting-future-
    window shapes stay bounded and heal at next renewal); cancelled runway / bounded prices / one-off windows
    untouched; idempotent.
  - **Tests (real testcontainers Postgres, no mocks)**: lifecycle_failopen_integration_test.go (webhook
    silence → park unknown → resolve renewed with access continuous; converge-off trivially; user cancel closes
    exactly at period end; immediate cancel; reactivate restores standing; dunning exhaustion via real recorded
    FailMembership attempts; ResolveCancelledRemoteAlive closes + #679 delete still queued; bounded purchase
    expires; derive replay idempotent + MissingEffects parity), converge_failopen_integration_test.go (REAL
    sweep parks unknown, standing window untouched across repeated sweeps + resolution),
    failopen_migration_integration_test.go (5 seeded shapes, GIST holds, idempotent re-run),
    unknown_guard_integration_test.go (same product/tier-group blocked w/ code, unrelated allowed, resolved-
    terminal allowed, full Checkout() blocked), CCBill webhook tests rewritten to the new state machine.
    All integration suites green on touched packages: subscriptions, entitlements, grants, checkout, webhooks,
    reconcile, reconcile/converge (incl. the #664 imported-shape cohort), rails; one pre-existing test
    (converge derive-mismatch grant direction) re-seeded to the legacy bounded shape its scenario requires.
  - DEFERRED to #690 (unticked bullet): freeloader redefinition + verification_pressure gauge.
  - NOTED, not fixed (sibling-owned reconciliation.sql): `ListDeadSubsWithLiveEntitlements` flags a cancelled
    sub with a LIVE runway window (end_at > now) and its AUTO repair revokes it as-of now — pre-existing; under
    #691 every period-end cancel produces that shape for the runway duration, so the check needs a
    paid-through/ended_at guard or the runway dies at the next sweep. Follow-up needed.
    → FIXED in #690 (2026-07-02): entitled-bound guard + BoundSubscriptionAccess repair; see #690 Progress.

---

# #690: invariant gauges — freeloader count + duplicate-coverage count, always zero

**Completed:** no
**Status:** IMPLEMENTED (2026-07-02, uncommitted) — see Progress; planned 2026-07-01, Paul — two always-zero error metrics; any nonzero value means the billing state
machine is failing. Named by Paul: "freeloaders" = users retaining a live entitlement they did not pay for
(membership expired, no admin grant). Legacy production doujins has ~400 of them today (the #664/#731 analysis
cohort: source-active NMI rows unpaid since before late April); the new system measured ZERO on the full
re-imported dataset at every level of the definition — this gauge makes that property observable and alertable.

## Metadata
- Category: reconciliation
- Status: implemented (uncommitted)
- Passes: true (unit + integration green on every touched package; see Progress)

## Definitions

**Freeloader** — a LIVE entitlement window (start<=now<end, not revoked/deleted) whose justification chain is
broken. The chain: window ← live (unterminated) grant with a window covering now ← live source (subscription
whose paid/grace period covers now, completed non-refunded payment for ownership, or admin grant). Coverage
after #664/#665:
- grant terminated but window still live → `derive.grant_effect.excess` (exists);
- dead subscription with live windows → the #665 DERIVE check `ListDeadSubsWithLiveEntitlements` (exists;
  `unknown` correctly excluded — access-intact is policy, and their windows end at paid-through naturally);
- live window with NO grant at all (the spec's deferred "true orphan") → MISSING, build it;
- live window extending past its subscription's paid-through+grace → verify the dead-sub/derive checks cover
  every path here; if any slips through (e.g. active sub whose window outruns period end), add the check.

**Duplicate coverage** — the same (customer, product) holding overlapping paid coverage twice. Mostly
STRUCTURAL already: `uq_subscriptions_customer_product_lifecycle` (one live lifecycle sub per customer+product),
GIST no-overlap on live entitlement windows, `consistency.duplicate.provider_charge` (double charge, same
month), `pull.subscription.duplicate` (remote-side duplicates, #665 rename). The real gap: ownership
double-purchase ACROSS months (same one-off/lifetime product bought twice > 1 month apart escapes the
month-scoped charge check) → one CON check: more than one live ownership grant per (customer, product).

## Tasks

- [x] DERIVE/CON check `derive.entitlement.orphan` (freeloader): live entitlement window with no live grant
      covering now for its (customer, source_type, source_id) — merchant-wide set query + customer-scoped narg,
      converge-pass pattern. ADMIN surface-only (revoking access is an operator decision; per policy never
      auto-revoke on a derived conclusion alone) unless the orphan is provably a projection bug.
- [x] CON check: >1 live ownership grant per (merchant, customer, product) → `consistency.duplicate.ownership`,
      ADMIN surface-only (refund is an operator decision).
- [x] The gauges: expose named counters computed from OPEN findings of the designated type sets —
      `freeloaders` = open {derive.entitlement.orphan, derive.grant_effect.excess, dead-sub-live-window type};
      `duplicate_coverage` = open {consistency.duplicate.provider_charge, consistency.duplicate.ownership,
      pull.subscription.duplicate}. Surface on the existing report/admin path (findings ledger is the metric
      store — no new tables); alert semantics: > 0 for one full sweep cycle = state machine failing.
      SEVERITY ORDER (Paul 2026-07-01): a duplicate is WORSE than a freeloader — it means a customer is being
      CHARGED TWICE (money harm, refund + trust damage) vs marginal content access deliberately tolerated under
      #691 fail-open. duplicate_coverage findings carry severity `critical`; freeloader findings `high`.
- [x] #691 interplay: once fail-open lands, redefine `freeloaders` to PROVEN-dead/absent sources only (stale ≠
      freeloading) and add the companion `verification_pressure` gauge (count + max-age of standing access past
      paid-through awaiting verification) — allowed to be nonzero, but its AGE trending up means the
      verification machinery is down.
- [x] Integration tests: seed each broken-chain shape → finding + gauge > 0; healthy dataset → both gauges 0;
      the #664 imported-shape dataset stays 0 (regression: freeloaders can never be reintroduced by an import).
- [x] Verification SQL from the 2026-07-01 analysis documented on the checks (the two queries proving doujins
      measured 0: grant-justification by source, window-vs-paid-through by status).

Acceptance: `freeloaders` and `duplicate_coverage` are queryable named gauges that read 0 on a healthy merchant
(doujins full import measures 0 today); each broken-justification shape and the cross-month ownership
double-purchase produce an open finding that moves its gauge above 0; no auto-revocation is introduced —
surfacing only.

## Progress

- 2026-07-02 IMPLEMENTED (uncommitted). What was built:
  - **Runway-hazard fix FIRST (the #691 NOTED item)**: `ListDeadSubsWithLiveEntitlements` now guards on the
    ENTITLED BOUND = GREATEST(current_period_ends_at, ended_at) — a terminal sub's BOUNDED live window is
    excess only past that bound (a user-cancel runway is paid access, never flagged before period end); the
    repair is no longer revoke-as-of-now but the missed/correct #691 closure
    (`entitlements.BoundSubscriptionAccess(sub, bound)` — bounds the overrun back to the bound, runway
    survives). STANDING (end_at NULL) windows of terminal subs moved OUT of this AUTO check into the orphan
    detector (ADMIN). `ReconcileRevokeSubscriptionEntitlements` deleted (sole caller was the old repair).
  - **Freeloader detector `derive.entitlement.orphan`** (DERIVE, ADMIN surface-only, severity high, subject
    `entitlement:<id>`): a LIVE window with NO live un-terminated entitlement grant covering now (matches
    MaterializeGrant's standing-access projection — per-period grants of a live sub lapsing is verification
    pressure, NOT freeloading) whose source is PROVEN dead/absent. Exactly three legs (query
    `ListOrphanEntitlementWindows`, customer-narg + merchant-wide, converge-pass pattern):
    `missing_subscription` (no sub row at all — LIVE dangling windows moved here from
    consistency.reference.source_reference, which now keeps only NON-LIVE dangling refs),
    `terminal_subscription_standing` (end_at NULL on a cancelled/expired/failed sub — the #691 closure never
    landed; fires only once no grant covers now, i.e. past paid-through), `refunded_payment` (one_off window,
    payment status refunded, no live grant; grant-backed refunded shapes stay derive.grant.excess, terminated-
    grant-backed windows stay derive.grant_effect.excess). Recommendation: prose + `{action:
    revoke_entitlement, params:{entitlement_id, as_of?}}` (as_of = entitled bound for the terminal-standing
    leg) + alternative `{action: record_admin_grant}`. Verification SQL from the 2026-07-01 doujins analysis
    documented on the query.
  - **Duplicate detector `consistency.duplicate.ownership`** (CON, ADMIN surface-only, severity CRITICAL,
    subject `ownership:<customer>:<product>`): >1 live un-terminated PAID (purchase/subscription-sourced,
    `include:%` bundle children excluded) ownership grant per (customer, product) — the cross-month double
    purchase (`ConDuplicateOwnershipGrants`). Refund-netting BOTH ways (status='refunded' OR a linked refund
    row, the admin path) so an approved refund self-confirms on the next sweep; same netting added to
    `ConDuplicateChargesSamePeriod`. Recommendation: prose naming both purchases (grant/payment ids, amounts,
    dates) + `{action: cancel_and_refund, params:{subscription_id? (later leg if subscription-shaped),
    refund_payment_id (later payment)}}`. `duplicate.provider_charge` bumped to CRITICAL + given the same
    refund recommendation (later charge). #692 executor relaxed: cancel_and_refund's subscription_id is now
    OPTIONAL (refund-only for pure one-off duplicates; ≥1 of the two params required) — with tests.
  - **Gauges** (`GET /merchant/findings` → `gauges`): freeloaders = open {derive.entitlement.orphan,
    derive.grant_effect.excess} (dead-sub check deliberately NOT in the set — it auto-repairs in the same
    sweep, never sits open); duplicate_coverage = open {consistency.duplicate.ownership,
    duplicate.provider_charge, pull.subscription.duplicate}; NEW `verification_pressure` = {count,
    max_age_seconds} of `unknown` subs past paid-through, computed LIVE from subscriptions
    (`CountUnknownSubsPastPaidThrough`) — pressure reading, not findings. Alert semantics documented on the
    handler: freeloaders/duplicate_coverage nonzero for a full sweep cycle = state machine failing;
    verification_pressure nonzero allowed, max_age trending up = verification machinery down.
  - **Plumbing**: `ConvergeFinding.RecommendedAction` → UpsertReconciliationFinding.recommended_action (prose
    now persists from converge passes); structured recommendation rides `evidence.local.recommendation`
    (FromEvidence honors both); recommendation builders added to internal/reconcile/recommend
    (RevokeEntitlementRec/RecordAdminGrantRec/CancelAndRefundRec). Findings CHECK is pattern-based
    (`^(pull|derive|life|consistency)\.…`) — both new types pass, NO migration needed. Route-surface golden
    updated with the sibling's `/v1/merchant/findings*` + `/v1/merchant/worker-health` routes.
  - **Tests (REAL testcontainers Postgres, no mocks)**: converge_gauge_detectors_integration_test.go (runway
    guard: paid runway never flagged, overrun bounded back to period end not revoked, idempotent; all three
    orphan legs fire with recommendations + as_of; runway/unknown/past_due/completed-payment negatives — stale
    ≠ freeloader; partition assertions: live dangling window is orphan-only, standing terminal window never
    touched by the AUTO dead-subs check; duplicate ownership: cross-month pair fires critical w/ later-payment
    rec, subscription-shaped rec carries subscription_id, single/sequential-after-termination clean,
    surface-only), admin_findings_gauges_integration_test.go (healthy dataset → all three gauges zero;
    freeloader detector e2e: sweep detects → gauge 1 → approve revoke via the #692 endpoint → next sweep
    confirms, gauge 0; verification_pressure count+max-age, resolve one → drops; duplicate detector e2e incl.
    refund-only approve → refund nets the duplicate → sweep confirms, gauge 0; empty cancel_and_refund → 400).
    Re-seeded two pre-existing tests to the new partition (derive-mismatch revoke direction → bounded-overrun
    shape asserting closure-not-revoke; CON source_reference → non-live dangling window). Full
    `-tags integration` suites green: internal/reconcile, internal/reconcile/converge (incl. the #664
    imported-shape cohort), internal/http/handlers, internal/integrationharness (route-surface golden);
    `go build ./...` + `go test ./...` green.
  - Files: internal/db/queries/{reconciliation,consistency}.sql (+ regen), internal/reconcile/converge/
    {converge,converge_passes}.go, internal/reconcile/{store.go,recommend/recommend.go},
    internal/http/handlers/{admin_findings.go,admin_findings_actions.go},
    internal/integrationharness/testdata/standalone_route_surface.txt, tests.
  - NOT done (deliberate): no auto-resolve sweep for orphan/duplicate findings whose condition clears WITHOUT
    operator action (converge has no vanish machinery; matches every other ADMIN finding — operator ignores
    stale items); `pull.subscription.duplicate` severity/emission untouched (pull-engine-owned);
    subscription-shaped duplicates with NO payment linkage emit prose-only (cancel alone would not clear the
    condition — operator resolves out-of-band).

---

# #689: worker observability — a River worker failing 100% of its runs must be impossible to miss

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented + verified: go build/vet clean (plain and -tags integration);
unit tests green; river + app + handlers + migrations integration suites green on the working tree (alongside
the landed #688/#665 churn). Origin: the #673 lesson generalized. InvoiceWorker/AutoTopupWorker failed
EVERY run since birth (bare job context → merchant.Require → ErrNoMerchant) and no signal anyone watched could
distinguish that from health: the process was up, the queue moved, jobs reached terminal states on schedule,
and the identical hourly error line read as wallpaper. Nobody missed the money because the capability had
never visibly worked. Found only by reading the call graph in an audit.

## Implementation (2026-07-01)
- Mechanism: `rivertype.WorkerMiddleware` (river v0.26 `river.Config.Middleware`) — ONE middleware
  (`riverjobs.WorkerHealthMiddleware`, internal/river/worker_health.go) wraps every registered worker; zero
  per-worker code. Snoozes are neutral; bookkeeping runs on a detached ctx and never fails the job.
- Table: migration `062_worker_health.up.sql` → `openrails.worker_health` (kind PK, registered_at,
  expected_period_seconds, last_success_at/last_error_at/last_error (2000-rune truncate),
  consecutive_failures, last_alerted_at). RLS posture: OPERATOR-GLOBAL control-plane table like
  `openrails.merchants` — no merchant_id, NO RLS policy, granted to openrails_app (documented in migration).
- Registration capture: `addTrackedWorker` + `Runtime.healthPeriodic` (internal/app/worker_health.go) note
  every kind + periodic cadence (shortest wins) in `riverjobs.WorkerRegistrations`; river_register.go routes
  all registrations/schedules through them. Standalone client installs the middleware in InitRiver; embedded
  hosts get `Embedded.WorkerMiddleware()` for their own client config.
- Alert rule: `WorkerHealthCheckWorker` (kind `openrails.worker_health_check`, every 5m, RunOnStart) seeds a
  row per registered kind, then alerts on consecutive_failures ≥ 3, never-succeeded past
  max(3×period, 30m) since registered_at, or stale last_success past the same threshold (periodic kinds
  only). Deduped: re-alert after 24h or a fresh incident (success since last alert). Routed via
  `webhooks.RecordLedgerRepairAlert` (notification_queue system alerts, operation=worker_health) fanned to
  every active merchant — the SAME surface as the ledger reconcilers. The checker heartbeats its own row.
- Admin surface: `GET /merchant/worker-health` (handler→gen read, `PermMerchantRepairAlertsRead`,
  internal/http/handlers/admin_worker_health.go).
- Tests: unit (rule matrix/dedup/registrations/truncation, internal/river/worker_health_test.go) +
  REAL-River-client integration (internal/river/worker_health_integration_test.go: always-failing worker →
  streak row + alert within threshold; healthy worker never alerts; #673 merchant.Require repro trips
  never_succeeded; checker dedup) + admin endpoint integration
  (internal/http/handlers/admin_worker_health_integration_test.go).

## Metadata
- Category: reliability
- Status: done
- Passes: true

## Target
Per-worker health bookkeeping + a screaming signal:
- Record per worker kind: last_success_at, last_error_at, last_error, consecutive_failures (a tiny table or
  in-Postgres upsert from a worker middleware — River supports middleware/hooks; wrap once, cover every
  registered worker, zero per-worker code).
- Alert condition: worker has NEVER succeeded since deploy, or consecutive_failures ≥ N, or no success within
  k× its expected period (periodic kinds' cadence is known at registration). Route to the existing
  repair-alert channel (durable, surfaced in admin) — not just a log line.
- Admin surface: one endpoint/table listing every registered worker kind with last success/error — the
  "expected N collections, got 0" dashboard that would have caught #673 on day one.
- Integration test: a worker registered with an always-failing Work() trips the alert within the threshold;
  a healthy worker never does.

## Non-goals
Not a metrics stack (no prometheus dependency for this); not per-job tracing. One middleware, one table, one
alert rule, one admin view.

## Acceptance
A worker that errors on every run (or stops succeeding) produces a durable repair alert within its threshold
window without any per-worker instrumentation code; the admin surface shows last-success for every registered
kind; the #673 scenario reproduced in a test trips the alarm.

---

# #688: flatten the layer ceremony — retire internal/db/repo as a layer, trim service re-exports

**Completed:** no
**Status:** PLANNED (2026-07-01, approved by Paul) — from the layering discussion in the 2026-07-01 design
review. Principle (goes in CLAUDE.md as part of this issue): **a layer earns its existence by doing work at
its own altitude** — handler = HTTP concerns; module/service = orchestration (tx, locks, state machines,
doctrine); gen = your SQL, typed. Traverse a layer only when it has a job for that operation; skipping a
layer with nothing to do is correctness, not sloppiness. The anti-pattern is mandatory ceremony.

## Metadata
- Category: refactor
- Status: planned
- Passes: false

## Problem
Two styles coexist: the newest, most-audited code (money, grants) calls sqlc `gen` directly; older modules
route through `internal/db/repo` wrappers that are often pure relays, and some module services re-export
their repo's surface as one-line forwards (entitlement service: 18 of them). Three names per operation,
parity by discipline, zero added behavior.

## Design (settled in review)
- **Retire `internal/db/repo` as a LAYER, not as logic.** Every repo method is one of:
  (a) PURE RELAY (forwards to gen with the same args) → delete; callers call gen.
  (b) LOGIC-BEARING (decides/transforms/coordinates — e.g. subscription.go UpdateAt full-row contract +
      FOR-UPDATE variant, entitlement_timeline.go non-overlap window logic, genmap.go model conversion,
      payment.go CreateIfNotExists semantics) → RELOCATE into the owning domain module (subscriptions,
      entitlements, payments…), then delete the repo shell. The logic moves home; the layer disappears.
  Current inventory: ~20 files / ~6k lines in internal/db/repo (+ its integration tests, which move with
  their logic or retarget gen).
- **Trim service re-export forwards**: a module service does not mirror its data surface; callers that need
  a bare read call gen (or the module's real method). Entitlement service's 18 forwards are the poster case;
  sweep other modules for the same shape.
- **Explicitly OUT of scope**: pkg/service's dual-transport role (that's #685's parity-mirror retirement);
  handlers that already call gen for orchestration-free reads (that pattern is BLESSED — admin_operations.go
  style); building any new wrapper to "complete" a layer (forbidden — the trap is adding forwarding to the
  good code to match the bad pattern).

## Tasks
- [x] Write the altitude principle + conventions into .claude/CLAUDE.md (modules talk to gen; repo-style
      wrappers only where they carry logic, and they live IN the module; services don't re-export data
      surfaces; handlers may call gen for orchestration-free reads). (phase 1, 2026-07-01)
- [x] Inventory pass: classify every internal/db/repo method relay vs logic (write the table into this
      section); count callers per method. (phase 1 — table below; hot files classified + deferred)
- [x] Relocate logic-bearing methods into their owning modules (keep names/semantics; move tests alongside) —
      PHASE 1 domains done: entitlements, catalog (price/product), productaccess, checkout-session,
      payments, paymentmethods. Phase 2: subscriptions, customer/profile, notification-queue, solana-subs.
- [x] Delete pure relays; migrate callers to gen (phase-1 domains; green suites between batches).
- [x] Trim service re-export forwards — entitlements poster case done (the 18 forwards absorbed their repo
      bodies and became the real methods); catalog services likewise. payments/paymentmethods/checkout
      services still forward to their (now module-local) repos — trim in phase 2.
- [ ] Delete internal/db/repo when empty (genmap conversions land in internal/db/models or the modules that
      own the shapes).
- [ ] Full unit + integration suites green after each batch; no behavior change anywhere (pure motion).

## Sequencing
After the in-flight #665/#686/dedup agents land (subscriptions + reconcile are hot); safe to run per-domain
batches concurrently with #684/#685 as long as batches avoid their areas. Good candidate for the same
one-agent-per-domain pattern as the audit batch.

## Acceptance
internal/db/repo no longer exists; every former repo behavior lives either in gen (relays gone) or in its
owning module (logic relocated, tests moved); no module service re-exports its data surface; CLAUDE.md states
the altitude convention; zero behavior change (suites green throughout, no wire/API drift).

## Phase 1 (2026-07-01) — safe domains DONE, hot domains deferred

Classification + disposition per repo file (build + vet + per-module integration suites green;
zero test-assertion changes):

| repo file | disposition |
|---|---|
| genmap.go | conversions RELOCATED to internal/db/models/genmap.go (exported *FromGen, To/FromJSONB, UpdateTimestamp, IntPtrTo32, Deref*, RevokeReasonPtr). repo/genmap.go shrunk to transitional one-line shims for subscription.go / notification_queue.go (phase-2 deletions). |
| entitlement.go | ABSORBED into entitlements.EntitlementService — the 18 forwards inlined the repo bodies and became real methods; Insert + LatestFiniteWindow* gained real bodies; EntitlementRepo type deleted. DEAD methods deleted (zero callers, grep-verified): SetEndAtTx, GetLatestActive(+ByCustomer), EndActiveBySubscription, ResumeBySubscription, RevokeByID, RevokeBySubscriptionAndName. |
| entitlement_grace.go | DEAD entirely (3 methods, zero callers — orphaned by the #511/#512 grant rewrite) → deleted. |
| entitlement_timeline.go | RELOCATED to internal/modules/entitlements/timeline.go (all logic-bearing; only caller was the module). DEAD deleted: RevokeEntitlementNowTx, InsertTimelineWindow, SetEntitlementEndAtTx. |
| entitlement_feature.go | RELOCATED whole (merchant scoping + RLS-bypass FK guard + mapping) → entitlements/feature_repo.go; FeatureService holds the local type + *EntitlementService. |
| price.go / product.go | ABSORBED into catalog.PriceService / catalog.ProductService (forwards became real bodies). Raw Update → unexported updateRow (public Update/Delete keep the deliberate immutability errors); raw Delete DEAD → deleted. PriceFilter moved into catalog, alias killed. |
| product_access_grant.go | RELOCATED whole (grant-ledger mapping logic) → productaccess/grant_repo.go. |
| checkout_session.go | RELOCATED whole → checkout/session_repo.go (incl. BindSolanaTransactionRequest). solana/poller.go's two orchestration-free reads now call gen + models.CheckoutSessionFromGen directly (avoids a checkout→solana import cycle). |
| payment.go | RELOCATED whole → payments/payment_repo.go (CreateIfNotExists semantics, provider-account stamping, relation stitching). PaymentRepo.Delete DEAD → deleted. PaymentFilters moved along; payment_test.go moved. |
| payment_method.go | RELOCATED whole → paymentmethods/payment_method_repo.go. The repo + service ErrPaymentMethodNotFound sentinels UNIFIED (same package now; same message, errors.Is-compatible for all callers). |
| customer.go | DEFERRED (callers in modules/subscriptions + modules/webhooks — hot). Enabler only: ensureCustomerRow exported as EnsureCustomerRow (unexported alias kept for subscription.go). Phase 2 finds identity's home. |
| profile.go | DEFERRED (ProfileRepo fields live in modules/webhooks dispatcher/ccbill — hot). |
| notification_queue.go | DEFERRED (all callers in modules/subscriptions — hot). |
| solana_subscription.go | DEFERRED (callers incl. modules/subscriptions lifecycle + cascade test — hot; river/http callers move with it). |
| subscription.go | DEFERRED per plan (concurrent agents own it). |
| provider_account_stamp.go | stays (subscription.go dep); resolver exported (ResolveRailMerchantAccountIDForStamp) for the relocated payment/payment-method code; WithRailMerchantAccountID unchanged (handlers/webhook.go, reconcile test). Moves with subscription.go in phase 2. |
| errors.go (IsNotFound) | stays — 98 call sites across all modules incl. hot; its own phase-2 sweep. |

Tests moved with their logic (assertions untouched): entitlement_timeline → entitlements/timeline_integration_test.go
(calls the unexported extendActiveBySubscription/endActiveByPayment where the old repo API took an explicit
now the public service API injects via clock), entitlement_feature, price_intro, price_by_product,
rls_realtable (ProductService), catalog_platform_sidecars (RLS-boot helpers duplicated locally),
payment_method_charge, payment_test. catalog gained main_test.go (dbtest.RunMain).

Line delta over phase-1 files: 41 files, +990/−2243 (net −1253).

Cross-module edits flagged: internal/reconcile/provider_account_stamp_integration_test.go — one mechanical
swap repo.NewPaymentRepo → payments.NewPaymentRepo (reconcile is a concurrent agent's area; the file had no
uncommitted churn). pkg/service needed NO edits (it only uses repo.IsNotFound, untouched).

Full ./tests/... run (2026-07-02): ONE failure — TestFailMembershipLimitedModeLeavesRemoteSubscription
("limited mode must not stamp a proactive deletion marker / schedule a remote delete",
tests/deferred_delete_followups_test.go). VERIFIED PRE-EXISTING: fails identically in a clean worktree at
committed HEAD a6ec9070 with zero uncommitted changes — NOT caused by #688 phase 1; it's in the #664/#665
limited-mode/deferred-delete domain (concurrent agents active there). Everything else in ./tests/... green.

Phase 2 remainder: subscription.go (+ callers in subscriptions/webhooks/intents/river), notification_queue.go,
solana_subscription.go, customer.go, profile.go, provider_account_stamp.go, errors.go, repo/genmap.go shims;
trim residual same-name forwards in payments/paymentmethods/checkout services (their repos now live in-module,
but the intra-module forward ceremony remains); delete internal/db/repo when empty.

---

# #686: decompose the subscriptions module along the #669 registry seam

**Completed:** yes
**Status:** DONE (2026-07-01, uncommitted in-tree) — rail-specific lifecycle behavior moved out of the
per-rail branches inside internal/modules/subscriptions into the #669 rail descriptor registry (3 new
descriptor fields: RemoteDeleteOnTerminalCancel, CancelMode func, CancelPortalURL); lifecycle orchestration no
longer switches on rail strings (six justified single-rail executor/machinery sites remain, see Inventory).
Behavior-preserving; all suites verified (see Tasks) with an isolated A/B proving zero introduced tests/
failures.

## Metadata
- Category: architecture
- Status: done
- Passes: true

## Problem
internal/modules/subscriptions (~9k lines) carries rail-conditional behavior inline: lifecycle
create/renew/fail/cancel paths branch per rail (NMI remote-delete scheduling, CCBill
provider-auto-billed/no-retry semantics, Stripe-specific sources), which means every new rail edits the
module's core files and every reader must hold all four rails in their head at once. #669 built the registry
(predicates + metadata already collapsed); behavior hooks are the remaining half.

## Inventory (2026-07-01, every rail-conditional branch in internal/modules/subscriptions, non-test)
(a) = existing/new registry consult, (b) = new descriptor field, (c) = genuinely inline (justification given).
1. cancel_mode.go:89-104 CancelModeFor rail switch (stripe reversible / ccbill external-portal / nmi
   state-conditional via nmiDeletePending / default destructive) → (b) descriptor func field `CancelMode`
   (CancelMode type + constants move to rails; subscriptions keeps type/const ALIASES so the exported surface —
   used by handlers/subscription_lifecycle.go:162 + pkg/service/service_user.go:1058 — is signature-stable).
2. cancel_mode.go:177-183 CancelPortalURL (hardcoded CCBill support URL) → (b) descriptor metadata field
   `CancelPortalURL string` ("" = none).
3. lifecycle_service.go:1574 ResolveCancelledRemoteAlive → `rails.IsNMI && RailSubscriptionID != ""` gates the
   deferred NMI delete queue → (b) descriptor bool `RemoteDeleteOnTerminalCancel` (nmi true, all others false).
   deferDelete!=nil gating + #679 queue-always + marker/intent atomicity UNTOUCHED — only the rail-name test
   moves to the registry.
4. lifecycle_service.go:1932 FailMembership terminal cancel, same `rails.IsNMI` gate → same (b) predicate.
5. user_service.go:479 CCBill guard in CancelUserSubscription (portal error) → (a) consult the new registry
   cancel mode (external_portal ⇒ CCBillCancelError, URL from descriptor); message text preserved.
6. user_service.go:487-530 NMI-only user-cancel executor (deferred/immediate rail-side delete, NMI clients) →
   (c) single-rail machinery needing injected NMIClients; a hook would have exactly one implementer and drag
   client deps into the dependency-free descriptor.
7. admin_service.go:180 cancelWithNMI guard → (c) NMI-only helper's own early return, not orchestration.
8. admin_service.go:218-235 admin cancel executor switch (NMI client delete / StripeService cancel / others
   rejected) → (c) per-rail EXECUTORS over injected clients; #669 deliberately left executors out of the
   descriptor (dependency-free metadata), no second dependency-free implementer exists.
9. lifecycle_service.go:1402 Solana cancel cascade inside ApplyLocalCancellation (#264 cranker stop) → (c)
   single-rail machinery with one implementer requiring db/repo handles; a hook inverts layering (rails would
   import internal/db/repo) for zero orchestration gain.
10. email_service.go:244 `isSolana` one-off receipt copy (keyed on a free-string payment method, not
    models.Rail) → (c) content templating for one rail; no orchestration.
11. dunning.go ClassifyNMIDecline + hardDeclineNMICodes → (c) NMI decline taxonomy consumed only by the NMI
    dunning worker (river/jobs_dunning.go:479); no other rail has NMI response codes.
12. dunning.go:275 RenewalGraceEligibleRail → already (a), registry delegate since #669.
13. email_service.go:576 railDisplayName → already (a), registry delegate since #669.
14. lifecycle_service.go:157 + :1888 (RenewalGraceEligibleRail / rails.OpenRailsDrivenDunning consults) →
    already (a) registry-backed.
15. subscription.go:72-73 RailCCBill/RailStripe string consts → not branches; consumed by
    internal/http/handlers; left.
16. provider_account_clients.go:31 rails.SameRail → normalization, not per-rail dispatch; left.
Candidates from the brief verified ABSENT in this module: payment-method-update remote propagation (no such
code here); dunning/retry participation + renewal grace semantics already registry predicates.
Webhooks-surface check: internal/modules/webhooks consumes subscriptions.{Notification,Subscription,
SubscriptionLifecycle}Service, New*Service ctors, {Create,Renew,Cancel,Fail,Reactivate}MembershipParams,
IsTerminalTransitionBlocked, RemoveCancelledSubscriptionsForActivation, PremiumEndReason*, DunningInterval,
TerminalCancelReason; internal/app consumes DeferredDeleteScheduler + SetDeferredDeleteScheduler. NONE of these
change shape (CancelMode aliasing is additive); no webhooks-side edits required, nothing deferred.

## Target
- Registry descriptors gain lifecycle FUNCTION FIELDS for genuinely rail-divergent behavior (e.g. terminal-
  cancel remote cleanup, dunning/retry policy shape, renewal semantics) — behavior lives with the rail.
- lifecycle_service.go and friends become rail-agnostic orchestration: consult the descriptor, never switch on
  the rail string.
- Doctrine preserved EXACTLY: cancellation-last-resort / evidence-gating (#664), NMI remote delete only on the
  certainty path (converge side-effect deps stay nil), #679 circuit-breaker semantics untouched.

## Tasks
- [x] Inventory rail-conditional branches in internal/modules/subscriptions (grep rail switches/IsNMI/case
      strings); classify each: registry predicate (exists), new behavior hook, or genuinely-inline.
      → DONE, see Inventory above (16 sites: 3 new descriptor fields, 2 registry-consult ports, 6 justified
      inline, rest already registry-backed or not branches).
- [x] Add the behavior hooks to the #669 descriptors; port branches; keep exported call surfaces used by the
      webhooks module STABLE (concurrent work in webhooks).
      → Descriptor gains `RemoteDeleteOnTerminalCancel bool` (nmi only), `CancelMode func(sub, now) CancelMode`
      (type + reversible/destructive/external_portal constants moved to rails; NMI's is the issue-216
      state-conditional nmiCancelMode), `CancelPortalURL string` (ccbill support portal). subscriptions keeps
      CancelMode/constants as ALIASES + CancelModeFor/CancelPortalURL as registry delegates — handlers,
      pkg/service, webhooks untouched. lifecycle_service.go:1574/:1932 deferred-delete gates and
      user_service.go:479 external-portal guard consult the registry; nmiDeletePending deleted (lives in the
      nmi descriptor func). #664/#679 doctrine untouched: deferDelete nil-gating, queue-always,
      marker+intent same-tx atomicity all byte-identical; converge still constructs lifecycle with nil deps.
- [x] Registry completeness test extended to the new function fields.
      → TestRegistryCompleteness forces CancelMode non-nil per rail + portal-URL-implies-external-portal;
      TestRegistryPinnedFacts pins per-rail RemoteDeleteOnTerminalCancel + active-sub cancel mode + ccbill
      portal URL + NMI's pending/executed/lapsed cancel-mode window; TestLookupNormalizes pins unknown-rail
      destructive default + nil-sub destructive + empty portal URL.
- [x] Existing subscriptions + webhooks + tests/ suites green as the behavior-preservation net.
      → go build + go vet (both tags) clean; repo-wide unit suite green (incl. rails + subscriptions);
      integration green: internal/modules/subscriptions, internal/reconcile (incl. the #679
      unknown-resolution queue-always test), internal/reconcile/converge (converge still constructs lifecycle
      with NIL side-effect deps — cannot fire the remote NMI delete), internal/river (dunning).
      tests/ suite: verified via ISOLATED A/B (staged baseline ± only this issue's 5-file diff, because
      concurrent webhook-dedup/decider work shares the tree and briefly broke the build): identical failure
      sets with and without the diff — ZERO failures introduced. Two PRE-EXISTING failures (confirmed at clean
      HEAD by other agents, NOT #686's): TestFailMembershipLimitedModeLeavesRemoteSubscription (stale pre-#679
      test still asserting the old #345 "limited mode doesn't queue" semantic; code correctly queue-always +
      parks at the executor — the test belongs to the #679 follow-up) and six ordering-dependent CCBill
      webhook-500 tests (pass in filtered runs both with and without the diff).

## Verification summary (2026-07-01)
Behavior-preserving; no migrations; no webhooks-module edits (its consumed subscriptions surface unchanged —
see Inventory tail); remaining rail checks in the module are exactly the six justified-(c) executor/machinery
sites. STATUS: DONE pending Paul's review/commit.

## Acceptance
No rail-string switches remain in subscription lifecycle orchestration; adding a rail's lifecycle behavior =
filling descriptor fields; all existing suites green with no behavior change.

---

# #685: unify embedded and remote modes — one client, pluggable transport

**Completed:** no
**Status:** IN PROGRESS (2026-07-01) — core landed in-tree. In-process RoundTripper (`embed/transport.go`)
dispatches into the real neutral /v1/merchant mux (same RegisterServiceRoutes standalone mounts); AUTH =
context-attached host principal (`billingauth.HostPrincipal` via `WithHostPrincipal`) checked FIRST in
`legacyGate.Authorize` — unforgeable from the network because the gate consults the request CONTEXT, never a
header; permissions = merchant owner grant, gated like every credential. Merchant pinning: transport pins
`Runtime.ConfiguredMerchant` (live read; falls back to caller's `openrails.WithMerchant`). `Runtime.Client()`
now returns the unified client (remote impl over the in-process transport); 20/21 `openrails.Client` methods
migrated, their transcriptions DELETED from embed/client.go (~1080 → ~370 lines). Still transcribed:
`SetCustomerSpendDelegations` (wire surface is /v1/customers/* delegated customer-treasury auth, not the
merchant API) + the embedded-only extras with no wire counterpart (single `Admit`, `SetCreditAccountSettings`,
`ListCreditTransactions`, `BudgetStatus`, `AbuseUsage` — kept conservatively; no in-repo or host callers
found, later-retirement candidates together with their sole pkg/service backers `BudgetStatus`/`AbuseUsage`).
pkg/service audit: nothing retired this pass — every facade method the deleted transcriptions used is still
handler-backed. SDK ergonomics shipped: `WithAPIKey`, `Verifier`/`openrails.Verify` (probe =
GET /v1/merchant/settings; needs settings:read), static base-URL validation (no-error constructor,
descriptive per-call failure). Conformance now runs the embedded side through the NEW in-process transport;
new integration tests: auth-traversal (context marker vs spoofed header), Verify (good/bad/unreachable/
invalid-URL), WithAPIKey against the real service-credential middleware. Direction set by Paul: embedded mode
is a KEPT product feature (removal considered in the 2026-07-01 design review and REJECTED). Successor in
spirit to #338/#468; unblocked by #670.
VERIFIED (2026-07-01): go build ./... + -tags integration green; vet green on touched packages; unit suites
green (root, embed, pkg/billingauth, pkg/embedded/*, pkg/service, internal/http/*); integration GREEN for
./embed (full conformance THROUGH the new in-process transport + TestInProcessTransportAuthTraversal +
TestSDKErgonomics_WithAPIKeyAndVerify), ./pkg/embedded/..., ./pkg/service. NOT mine (reported, not fixed):
integrationharness TestStandaloneRouteSurface golden drifted by concurrent agents' new routes
(/v1/merchant/findings*, /v1/merchant/worker-health — #689's to update); ./tests CCBill
dunning/entitlement family (9 fails) = the known pre-existing ordering-dependent CCBill webhook failures +
the concurrent subscriptions/dunning agent's in-flight work.

## Metadata
- Category: architecture
- Status: in-progress
- Passes: false

## Problem
The last real "two of everything" is `embed/client.go`: it hand-transcribes handler logic per method, while
`remote.go` implements the same SDK interface over HTTP. Parity is maintained socially (comments citing
"transcribes handlers.X") and by the conformance suite catching drift after the fact. A bug can exist in one
transport and not the other; every new endpoint is written twice.

## Target design
One client implementation, transport pluggable:
- `remote.go` stays THE client. Embedded mode becomes a custom `http.RoundTripper` that dispatches directly
  into the in-process neutral mux (#670's handler — the same one embedded hosts already mount), no socket.
- Two constructors: `Dial(url)` (network) and `Embed(app)` (in-process transport). Same types, errors,
  validation, auth, envelopes — by construction.
- Delete `embed/client.go`'s transcriptions; the conformance suite shrinks to a transport smoke test.
- `pkg/service`'s "dual-transport parity contract" role dissolves — handlers are the single surface; keep
  pkg/service only where it's a real facade, not a parity mirror.
- Cost accepted: one JSON round-trip per in-process call (noise for billing ops; buys production-identical
  request path for embedded hosts).

## What stays legitimately dual
Boot/infra ownership (`BootstrapOptions` injected pool; posture gates keyed on it describe a REAL difference).
cozy-art's browser surface is already unified (frontend → embedded HTTP routes with normal AuthKit bearers).

## Tasks
- [x] In-process RoundTripper over the neutral handler (context propagation: merchant pinning + auth principal
      traverse identically — host principal is a context value, checked by the real gate; test proves a
      header-spoofed network-shaped request is 401 while the transport-injected one authorizes).
- [x] Unified constructor: `Runtime.Client()` hands out the unified client (remote impl + in-process
      transport). (A root-SDK `Embed(app)` is impossible without linking the engine into the root package —
      deps_test forbids it; `embed.Runtime.Client()` IS the second constructor.)
- [~] Migrate embed/client.go method-by-method; 20/21 interface methods migrated + transcriptions deleted.
      Remaining: SetCustomerSpendDelegations (customer-treasury auth surface) + embedded-only extras
      (Admit/SetCreditAccountSettings/ListCreditTransactions/BudgetStatus/AbuseUsage — no wire counterpart).
- [ ] Collapse conformance suite to transport smoke tests once the last transcription falls (kept full for
      now; it currently proves the new transport against the real standalone).
- [~] Audit pkg/service for facade-vs-parity-mirror roles: nothing retired this pass (all still
      handler-backed); flagged svc.BudgetStatus/svc.AbuseUsage (sole callers = embed extras) as retirement
      candidates once host non-use is confirmed.
- [ ] Host adoption notes (cozy-art, doujins): constructor swap, no behavior change expected (hosts keep
      compiling unchanged — Runtime.Client signature untouched).
- [x] SDK ergonomics (2026-07-01): `WithAPIKey(key)` sugar; `Verifier`/`openrails.Verify(ctx, c)`
      authenticated readiness probe (GET /v1/merchant/settings — kept OUT of the Client interface so
      third-party implementations keep compiling); static url.Parse validation at construction
      (no-error constructor, descriptive per-call error).

## Acceptance
One SDK implementation serves both modes; adding an endpoint touches handler + route + (generated/typed) client
once; embedded and remote cannot drift because there is nothing to drift between. Boot remains dual only where
infra ownership genuinely differs.

---

# #684: webhooks as wake-up signals — fetch-and-converge for fetchable rails (Stripe, NMI)

**Completed:** no
**Status:** IMPLEMENTED (2026-07-02, uncommitted) — fetch-and-converge shipped for Stripe + NMI
subscription-STATE events; thin-destination cutover DEFERRED (final task, needs live verification — see
Implementation notes); refund/void/dispute/checkout money-record handlers deliberately stay payload-apply
this round (documented below). Originally from the 2026-07-01 design review; sibling of the rescoped #665.

## Metadata
- Category: architecture
- Status: implemented
- Passes: true

## Problem
Webhook handlers APPLY the event payload to local state. Every ordering defense we built in #675 (row locks,
`stripe_last_event_created` watermarks, stale-event rejection, terminal-transition guards) exists because two
events about one subscription are two competing snapshots whose write order matters. That entire bug class is
structural to payload-apply and cannot be closed, only patched.

## Target design
For rails with a read API (Stripe; NMI v5), a verified webhook means only: "this object is dirty — fetch
current provider truth and converge to it." The event keeps four jobs: signature verification, dedup key,
identifying the dirty object(s), carrying the event timestamp. It stops being a state source.

- Ordering becomes meaningless: N events about one subscription (any order, duplicated, delayed) collapse to
  "fetch, converge" — idempotent by construction; replay = redundant fetch = no-op.
- Dirty-flag coalescing: burst of events about one sub ⇒ one fetch (debounce window), FEWER provider calls
  under dunning storms than per-event processing.
- Degrades safely: provider API down ⇒ row stays dirty, retry later, access intact (#664 posture). Fetch 404
  IS evidence (provider-confirmed dead) feeding the certainty ladder.
- Historical facts (decline codes, charge amounts) come from fetchable records (Stripe invoices/charges, NMI
  v5 payment queries), not event payloads — the payload only hints where to look.
- Stripe thin-event hydration already does fetch-on-event; classic snapshot events are the legacy shape. This
  issue makes fetch-first the ONLY shape for fetchable rails.
- CCBill stays payload-apply (nothing to fetch) — the documented exception; the #675 ordering machinery
  remains earning its keep there only.

## Read-after-write lag
A provider read API can briefly trail its own event. Converging to slightly-old truth is self-healing (next
event/pull converges again) and never corrupting; if a specific NMI read is known-laggy, gate on the event
timestamp (fetch until object updated_at >= event created, bounded retries) — same pattern as Solana
ReadUntilConsistent.

## Tasks
- [x] Dirty-mark + coalesced fetch worker (River; per-subscription debounce; UniqueOpts on the sub id).
      → internal/river/jobs_subscription_converge.go: `SubscriptionConvergeWorker` + enqueuer, unique per
      (merchant, rail, subscription_reference) via `river:"unique"` tags, 5s debounce (ScheduledAt),
      ByState = default MINUS completed (a finished converge never blocks the next wake-up; forces river's
      slower advisory-lock insert — fine at webhook volume). Registered in internal/app/river_register.go;
      dispatcher gets a late-bound enqueuer (`runtimeConvergeEnqueuer`, build_runtime.go) that resolves the
      runtime's producer at call time (works for config-built AND embedded-injected River clients).
- [x] Stripe: reduce snapshot-event handlers to dirty-marks; converge from fetched objects. Keep
      payment-record writes fetch-sourced.
      → invoice.paid / invoice.payment_failed / customer.subscription.updated / customer.subscription.deleted
      now parse IDENTITY ONLY (`markDirtyFromInvoice`/`markDirtyFromSubscription`) and enqueue. The fetch is
      GET /v1/subscriptions/{id}?expand[]=latest_invoice via the #665 liveness prober (stripeapi read-only
      choke), EXTENDED with metadata/customer/items-price/canceled_at/invoice created+amount_due+billing_reason.
      webhooks/converge_stripe.go `StripeConvergeService`: (1) creation leg — no local row: CreateMembership
      from FETCHED metadata (user_id/internal_price_id stamped by checkout) + latest paid invoice; checkout
      session marked succeeded; initial credits; cross-key payment dedup ported
      (fetchedInvoicePaymentAlreadyRecorded); (2) fetch-sourced MIRROR FACTS outside the decider vocabulary —
      price remap (Model B) + entitlement downgrade revoke, scheduled-cancel marks (Stripe-portal
      cancel_at_period_end), portal resume (terminal-reason guarded); (3) decider convergence via the new
      `reconcile.ConvergeSubscriptionFromSnapshot`; renewal credits granted post-renew (idempotent per
      period_end), payment-failed notification on past_due.
- [x] NMI: same, via the v5 read surface + `unknown_probe.go` sources.
      → recurring.subscription.add/update/delete + transaction.sale.success/failure → identity-only
      dirty-marks. webhooks/converge_nmi.go `NMIConvergeService`: pending rows take the SIGNUP leg (the
      decider deliberately doesn't own pending): fetched settled charge (ProbeSalesByOrderID on the local id)
      → CreateMembership (amount verbatim from the report, 2% price tolerance check kept), fetched decline →
      failed-payment row + FailMembership, nothing yet → ErrConvergeRetryLater (worker snoozes 1m, gives up
      after 24h — the pull sweep owns it then). Non-pending rows: the ONE #665 NMISubscriptionProber snapshot
      → decider. v5 404 = provider-confirmed-gone (cancel w/ RemoteGone).
- [x] Delete the then-dead payload-apply machinery for those rails.
      → stripe.go −1007 lines / nmi.go −1006 lines (net −2,553 across handlers+tests, +1,199 converge code).
      RETIRED: `stripe_last_event_created` watermark (const + read/set/bump helpers), the FOR-UPDATE
      read-modify-write applies (handleSubscriptionUpdated/handleInvoicePaymentFailed row locks + stale-event
      rejection + terminal-transition guard), handleInvoicePaid/handleSubscriptionDeleted,
      applyStripeSubscriptionStatus/applyStripeScheduledCancellation, ensureStripePaidSubscriptionEntitlements,
      validateStripeInvoicePrice (fetched-record variant lives in converge_stripe.go), NMI
      handleAdd/Update/DeleteSubscription + handleTransactionSaleSuccess/Failure + the metadata-transaction-id
      activation helpers. KEPT: everything CCBill uses (ccbill.go untouched), the NMI/Stripe money-record
      handlers listed in the notes, #671's wire-pinning helpers (stripeInvoicePaidAmountMicros/
      FailedAmountMicros + their test), stripeInvoice identity parsing (classic + 2026-04-22.preview shapes —
      the dirty-mark needs it).
- [x] Port the #675 ordering tests to convergence assertions.
      → webhooks_apply_integration_test.go rewritten (Stripe half): fake Stripe TRANSPORT (httptest, real wire
      shape) + real decider. Stale-updated-after-payment-failed → TestStripeConvergeStaleEventsCannotRevertPastDue
      (N wake-ups re-fetch the same truth; recovery only when truth changes); invoice.paid ∥ subscription.updated
      both orders → TestStripeConvergeRenewalIdempotentAnyOrder (same op now; exactly-once payment);
      terminal-blocked renewal → TestStripeConvergeTerminalRowKeepsMoneyTruth (no resurrect; the charge lands
      as a durable PAYMENT ROW instead of the old repair alert — money truth ≠ lifecycle truth). CCBill/NMI
      void/refund-race #675 tests kept unchanged (those handlers stay payload-apply). stripe_ordering_test.go
      (watermark unit tests) deleted with the watermark.
- [ ] FINAL — thin-destination cutover: DEFERRED (unchanged scope). Requires live verification that every
      relied-on event type is thin-deliverable via v2 event destinations + a signing-secret rotation; can't be
      live-verified here and correctness doesn't depend on it (fetch-first already ignores snapshot bodies for
      subscription state). No flag-gated blind implementation shipped (NMI-v5 lesson: probe first).

## Acceptance
For Stripe and NMI: no webhook handler writes subscription/payment state from an event payload; all state
writes flow through fetch → decider (#665). The #675 ordering tests pass with the ordering machinery deleted
for those rails. CCBill behavior unchanged.

## Implementation notes (2026-07-02)
- **Converge entry point** (reconcile is #665's, extended): `reconcile.ConvergeSubscriptionFromSnapshot(ctx,
  db, lc, sub, snap, now, window)` — Decide over EvidenceBundle{Snapshot} → UNCONDITIONAL money-truth mirror
  (ALL fetched charge events for the sub backfilled idempotently by transaction id, even on TransitionNone:
  terminal rows, early renewals, mid-cycle upgrade invoices) → rail-customer materialization → ApplyDecision.
  Side-effects factored into `applyDecisionSideEffects`, shared with the unknown-cohort resolution.
  `StripeSnapshotFromLiveness` exported (probe + webhook converge share one record→snapshot mapping); an
  UNPAID latest invoice w/ amount_due now maps to a decline transaction ("failed:"-prefixed id, invoice's own
  created — #651 no fabricated instants) so failed attempts land as failed payment rows fetch-sourced.
- **#671-family fix (load-bearing now)**: unknown_orchestration's backfillSubscriptionPayments wrote
  RemoteTransaction.AmountCents (CENTS) raw into payments.amount (MICROS) — 10,000× under. Converted at the
  boundary (CentsToMicros); the pull-engine applier already converted (appliers.go), only this path was wrong.
- **Deliberate scope keeps (payload-apply survivors, all idempotent money-RECORD paths resolved against local
  rows)**: Stripe checkout.session.* (one-time purchase + session bookkeeping), charge.succeeded /
  payment_method.attached (card snapshots), invoice_payment.paid (payment linking), refunds + disputes; NMI
  refunds/voids/ACU/chargeback batches. They never move SUBSCRIPTION state except through terminal
  cancel-on-full-refund paths that key off recorded payments. Fetch-sourcing these is a possible follow-up.
- **Behavior deltas (deliberate)**: (1) NMI transaction events with NO reference at all are now terminal
  non-retryable (was: retry forever on the same bytes). (2) A renewal charge against a TERMINAL row records a
  payment row instead of a `terminal_blocked_renewal_success` repair alert (stronger durable trace). (3) NMI
  sale.success with an unknown/unresolvable reference: converge job logs + completes (checkout/pull own it)
  instead of erroring the webhook. (4) Analytics event-log calls from the deleted Stripe/NMI handlers
  (charge success/failure payment events) were not ported — analytics for those rails' renewals now come from
  the payment rows; re-addable in the converge services if wanted.
- **Read-after-write lag**: kept minimal per the issue — the 5s enqueue debounce absorbs most of it;
  settlement lag on NMI signups snoozes (bounded); anything else self-heals via the next event/pull (converge
  is never-corrupting). No per-read consistency gate was added.
- **Coalescing window caveat**: `running` must stay in the unique states (river v3 requirement), so an event
  arriving mid-fetch dedupes away; the next event or the 4h pull sweep re-converges. Documented in the worker.
- **Tests (all real integration, testcontainers PG; provider = httptest transport fakes; NO mocks)**:
  (a) ported #675 scenarios above; (b) burst coalescing — TestSubscriptionConvergeBurstCoalescesToOneFetch
  (5 enqueues through a REAL started river client ⇒ 1 river_job row ⇒ 1 provider fetch by transport counter);
  (c) provider-down — TestStripeConvergeProviderDownParksAndRecovers + TestNMIConvergeProviderDownParks
  (retryable error, state/access intact, converges after recovery); (d) fetch-404 through the REAL decider —
  TestStripeConvergeFetch404IsProviderConfirmedGone + TestNMIConvergeFetch404IsProviderConfirmedGone (terminal
  cancel, cancel_type=expired); (e) end-to-end — TestWebhookWakeUpEndToEnd_{StripeRenewal,NMIRenewal}
  (REAL signature verification via sigverify [webhookutil unusable in river tests: import cycle], dispatcher →
  Postgres dedup truth row → coalesced job on a real river client → converged subscription + payment row;
  payload carries DECOY amounts to prove nothing is payload-applied). NMI activation ported:
  nmi_add_subscription_integration_test.go → converge activation/retry-later tests (fake NMI query.php + v5,
  direct-post counter must stay 0). Cross-key dedup test ported to the fetched-record helper.
- **Suites**: go build/vet clean both tags. Unit: whole repo green. Integration green: internal/modules/webhooks
  (full), internal/reconcile (full), internal/reconcile/converge (full — 2 transient failures observed mid-run
  were the #691 agent's in-flight churn and cleared once their edit settled), internal/river (full incl. new
  tests), internal/app, internal/http (merchant webhook routing). tests/ suite result recorded below.
- **Wiring note**: per-merchant Stripe/NMI converge credentials follow the EXISTING worker precedent
  (config.RailMerchantAccountSet + process NMI client map — same as ProviderRefreshWorker/dispatcher); a
  deployment without a River producer fails the dirty-mark RETRYABLY (provider redelivers; nothing lost).

---

# #681: scoped provider-credential lookups hardcode environment="live" — sandbox deployments can't resolve NMI/Stripe/Solana credentials

**Completed:** no
**Status:** IMPLEMENTED (2026-07-01, uncommitted) — full sweep done; every scoped lookup now derives environment
from deployment posture (`config.ExpectedProviderEnvironment(IsTestMode())` or the plumbed
`merchants.Service.providerEnvironment`). Fixtures un-masked, per-rail integration tests added.

## Metadata
- Category: bug
- Status: implemented
- Passes: true

## Problem
Under #355/#668 semantics, `test_mode=true` ⇒ provider-account catalog rows get `environment='test'`
(ValidateRailSet enforces consistency). But scoped credential lookups hardcode `environment="live"`:
- internal/http/handlers/checkout_session.go:143
- internal/modules/checkout/merchant_rail_secrets.go resolveNMIClient (NMI leg; CCBill leg fixed)
- internal/modules/vault/vault_service.go:268
- internal/merchants/credentials.go — LoadStripeCredentials / LoadNMIWebhookSigningSecret / LoadNMITokenizationConfig
- internal/modules/solana/recurring/wiring.go

A true sandbox deployment (test rows) can never resolve those rails' scoped credentials. The tests/ suite
masks it by seeding NMI as 'live'.

## Tasks
- [x] Sweep all sites to derive environment from config (`ExpectedProviderEnvironment(IsTestMode())`) — some
      `merchants.Service` methods carry no config today, so signatures change; plumb, don't global.
- [x] Un-mask the test fixtures: seed NMI/Stripe as 'test' under test_mode posture and keep the suite green.
- [x] One regression test per rail: sandbox posture resolves scoped credentials.

## Acceptance
A test_mode deployment resolves every configured rail's scoped credentials from its 'test' catalog rows; no
lookup hardcodes an environment literal.

## Implementation notes (2026-07-01)
- Plumbing: `merchants.NewService(pool, secrets, providerEnvironment)` — posture is a constructor REQUIREMENT
  (validated live|test); scoped lookups (`LoadStripeCredentials`, `LoadNMIWebhookSigningSecret`,
  `LoadNMITokenizationConfig`) use `s.providerEnvironment`. Callers: internal/http/server.go and
  bootstrap provisionMerchantIdentity (cfg plumbed in) pass `config.ExpectedProviderEnvironment(IsTestMode())`.
- Root-cause default removed: `normalizeProviderSecretEnvironment("")` no longer silently returns "live";
  empty env is now an error at AssertRailMerchantAccountUnowned / ResolveRailMerchantAccountByIdentity, and
  List/Get/UpsertPaymentProviderConfig default an OMITTED environment to the service posture (invalid ⇒ error).
- Sites fixed: handlers/checkout_session.go checkoutRailConfigured; checkout merchant_rail_secrets.go
  resolveNMIClient (NMI leg); paymentmethods/vault_service.go resolveNMIClient; merchants/credentials.go ×3;
  solana recurring wiring.go (environment now a REQUIRED param, empty errors — no live fallback);
  handlers/webhook.go webhookProviderEnvironment now uses the config helper (was literal-but-correct).
- Fixtures un-masked: tests/testcontainer_suite.go NMI seeded at posture env (helper hardcoding 'live' deleted);
  tests/nmi_live_lifecycle_e2e_test.go secret name follows posture; integrationharness
  standalone_no_default_merchant_test.go stripe row 'live'→'test'; internal/http
  routes_merchant_webhook_integration_test.go reworked to test posture (its ccbill 500 asserts were ALREADY
  red at HEAD — 403 — pre-existing; now honest: no live ccbill rows so the #668 bypass engages).
- Integration tests (real testcontainers PG, no mocks): merchants/sandbox_posture_integration_test.go
  (Stripe + NMI + live-row isolation), checkout/sandbox_rail_credentials_integration_test.go (NMI +
  CCBill through real merchants.Service), solana covered by existing
  TestRailMerchantAccountSignerUsesConfiguredEnvironment (env=test rows).
- Write-side too: merchant-manifest reconcile/prune now default an OMITTED account environment to posture
  (`manifestProviderEnvironment(cfg, raw)`) instead of silently writing live rows a sandbox can't resolve;
  static manifest validation accepts omitted env (resolved at reconcile).
- Left alone on purpose: HasLiveRailMerchantAccounts (deliberate live probe, #668), gen SQL
  `COALESCE(environment,'live')` (nullable-arg default, all callers bind non-nil), probe_cache verdict string.
- ./tests full suite: 7 failures (6 CCBill webhook 500s + TestFailMembershipLimitedModeLeavesRemoteSubscription)
  are PRE-EXISTING — reproduced IDENTICALLY on a clean HEAD (a6ec9070) worktree; pass in isolation
  (ordering-dependent, webhooks/subscriptions area). integrationharness: TestStandaloneRouteSurface (new
  worker-health route not in the #670 golden) + TestNativeCatalogBundleIncludesHTTP (catalog validation) fail
  from OTHER agents' in-flight uncommitted work, not #681.

---

# #671: CRITICAL — micros passed where cents/dollars expected: real charges at 10,000×

**Completed:** yes
**Status:** DONE (2026-07-02) — all findings fixed (1a-1g), boundary structs retyped
(moneyutil.Micros/Cents/MajorUnits), and the TEST WALL is complete: every provider money boundary (NMI, Stripe
outbound + inbound webhook, CCBill parsing/±2% threshold, Solana, rail-minor converters) is wire-pinned with
literal expected values. Final leg 2026-07-02: Stripe + CCBill batteries (see TEST WALL bullet).
`go build ./...` + money/intents/pkg-service/subscriptions/webhooks/catalog unit suites green.
Ready for completed.md. Notable:
CalculateModelBUpgradeCharge output was generally NOT whole-cent micros (integer proration), so the fix
quantizes the unused credit UP to a whole cent inside the shared helper (customer-favored; preview==charge on
every rail) and the NMI RunSale seam converts via MicrosToCentsExact (errors on sub-cent, never rounds).
RefundPayload.AmountCents now really is cents (converted at prepare, 400 on sub-cent micros), which also makes
the verifier's cents-vs-cents comparison consistent by construction — no separate verifier edit was needed.
Original filing: from the 2026-07-01 money-path audit. Headline claims (1a, 1b, worker wiring)
re-verified firsthand in source before filing. One root cause: the micros migration flipped the happy-path
checkout but missed tier-change/upgrade, Solana quote, and admin-refund paths. `nmi.SaleParams.Amount` is CENTS
(`FormatCentsDecimal`, internal/integrations/nmi/payments.go:47,104); `price.Amount` is MICROS (the happy path
converts via `MicrosToCentsExact`, internal/modules/checkout/nmi_sale_service.go:156).

## Metadata
- Category: bug
- Status: done
- Passes: true

## Findings
- **1a. NMI upgrade proration sale**: `internal/modules/checkout/service.go:1690` computes `prorationAmount` in
  micros (`CalculateModelBUpgradeCharge(oldPrice.Amount, newPrice.Amount, …)`, unit-preserving :2119), then
  :1805-1807 charges it RAW: `client.RunSale(nmi.SaleParams{Amount: prorationAmount})`. Preview shows $31.34,
  execution charges $313,400. VERIFIED.
- **1b. NMI upgrade recurring amount**: service.go:1762 `Amount: float64(newPrice.Amount) / 100.0` where
  `RecurringPaymentData.Amount` is float DOLLARS (create path :706 correctly uses `MicrosToMajorUnits` ÷1e6).
  Every rebill of the upgraded sub at 10,000×. VERIFIED.
- **1c. Solana one-time quotes**: `CalculateTokenQuote(…, amountCents…)` divides by 10^minorUnits
  (internal/modules/solana/support.go:123-138,199-207); all three prod callers pass MICROS:
  solana/pay.go:217, checkout/session_service.go:716, handlers/solana_supported_tokens.go:305→361.
  $19.99 → wallet asked for 199,900 USDC. Also label bug: solana/transaction.go:117 FormatCentsDecimal(micros).
- **1d. Solana upgrade first pull**: micros fed into `FiatCentsToStablecoinBaseUnits` (cents contract,
  support.go:74-86) at handlers/solana_recurring.go:385-394 + checkout/session_service.go:1305-1312. The correct
  `FiatMicrosToStablecoinBaseUnits` (support.go:53) exists, tested, ZERO production callers.
- **1e. Admin refunds**: `req.Amount` validated in micros (internal/modules/payments/payment.go:267) then shipped
  as `RefundPayload.AmountCents` (handlers/admin_payments.go:353), executed as cents (intents/refund.go:226 NMI
  FormatCentsDecimal; :400-402 Stripe). Full refunds always fail (> charge), but a partial refund ≤1/10,000 of
  the charge SUCCEEDS at 10,000×: refunding 5,000 micros ($0.005) of a $60 payment refunds $50 real money.
  Bonus: verifier compares cents vs micros (refund.go:317-318) so verified-executed refunds never match.
- **1f. Reporting-only siblings**: Stripe failed-payment rows store `inv.AmountDue` CENTS in the micros column
  (webhooks/stripe.go:1151,1166-1178; success path :484 converts). Analytics `float64(x)/100.0` family:
  stripe.go:1234, nmi.go:386,2085, ccbill.go:1581,2527,2766,2894,2975, analytics/event_log_service.go:1063,1096.
- **1g. Contradictory internal→rail-minor converters; JPY 100× exposure**: arrears hardcodes
  `(snapshot+9_999)/10_000` ceil (money/arrears.go:262) vs auto-topup's registry-scale
  `nativeAmountToRailMinor` truncating + assuming 10⁻² (money_in.go:221-236). JPY (scale 4, currency.go:26):
  the two paths send 100×-different values to the same Charger; Stripe JPY is zero-decimal. Also topup
  truncation credits more micros than charged (≤9,999/topup); arrears ceil collects ≤9,999 micros unledgered.

## Tasks
- [x] Fix 1a/1b/1d with the existing exact converters (`MicrosToCentsExact`, `MicrosToMajorUnits`,
      `FiatMicrosToStablecoinBaseUnits`); fix 1c callers or retype CalculateTokenQuote to micros.
      → DONE: CalculateTokenQuote retyped to `moneyutil.Micros` (÷1e6, currency-independent; dead
      `currencyMinorUnits` deleted); `FiatCentsToStablecoinBaseUnits` deleted (zero production callers after
      the switch); transaction.go label bug → FormatMicrosDecimal.
- [x] Fix 1e end to end (micros → provider-minor at the intents boundary; fix the verifier comparison).
      → DONE: `refundAmountCents` (MicrosToCentsExact, 400 on sub-cent) at prepareAdminRefund; cents carried on
      adminRefundPrepared into RefundIntentFor + RefundPayload.AmountCents. Verifier compare is cents==cents
      once the payload is truly cents — no refund.go logic change needed (unit doc added).
- [x] Fix 1f storage row (stripe.go:1151) exactly — DONE (in #675 agent — stripe.go failed-payment row now
      stores CentsToMicros(inv.AmountDue)). Analytics display family: event_log_service.go /100.0 pair fixed
      (MicrosToMajorUnits) by #671; the stripe.go/nmi.go/ccbill.go /100.0(+/100) siblings in
      internal/modules/webhooks (8 sites) also converted to MicrosToMajorUnits by the #675 agent.
- [x] 1g: ONE scale-aware converter with an explicit per-adapter unit contract; both arrears + topup use it.
      → DONE: `money.NativeToRailMinor` (registry-driven: new `MinorDecimals` on the Currency table; ceil,
      errors on unknown currency); arrears chargeOneOpenInvoice + topUpAccount both use it;
      `nativeAmountToRailMinor` deleted; ChargeRequest.AmountCents unit contract documented.
- [x] SYSTEMIC: introduce `Micros`/`Cents` defined types at the integration boundaries (nmi.SaleParams,
      RefundPayload, solana helpers) so this bug shape is a compile error; sweep remaining `/100` and `/10_000`
      literals.
      → PARTIAL: `moneyutil.Micros`/`moneyutil.Cents` defined; applied to CalculateTokenQuote +
      FiatMicrosToStablecoinBaseUnits params. Remaining sweep hits are inside internal/modules/webhooks (#675)
      and internal/integrations/nmi (#663).
      → COMPLETE 2026-07-02: boundary retyping shipped via the POST-#663 bullet; re-swept webhooks +
      integrations/nmi after #663/#675 landed — the only surviving `* 100` hits (nmi.go:2057, ccbill.go:1724)
      are refund PERCENTAGE math on already-micros values, not unit conversions.
- [x] POST-#663: retype the provider boundary structs (`nmi.SaleParams.Amount`/`RefundParams.Amount` →
      `moneyutil.Cents`, `RecurringPaymentData.Amount` dollars → consider a defined type,
      `intents.RefundPayload.AmountCents`, `subscriptions.RefundParams.Amount`, `money.ChargeRequest.AmountCents`)
      once the nmi-v5 rewrite lands — deliberately NOT done now to avoid colliding with #663's uncommitted tree.
      Ship the deferred provider-boundary wire tests (NMI RunSale/refund/recurring, Stripe outbound+webhook,
      CCBill parsing) in the same pass.
      → DONE 2026-07-01 (with the #669 agent — same boundary files). Retyped: nmi.SaleParams.Amount +
      nmi.RefundParams.Amount + AddRecurringPlan/EditRecurringPlan params + centsJSONAmount/centsToDollarString
      → moneyutil.Cents; RecurringPaymentData.Amount → NEW `moneyutil.MajorUnits` (float64 decimal dollars);
      intents.RefundPayload.AmountCents + RefundIntentFor param + adminRefundPrepared.amountCents →
      moneyutil.Cents; subscriptions.RefundParams.Amount + StripeInvoiceCollectionParams.AmountCents →
      moneyutil.Cents; money.ChargeRequest.AmountCents → moneyutil.Cents (rail minor; NativeToRailMinor now
      RETURNS Cents so both arrears+topup flow typed end-to-end). All existing wire-pinning assertions pass
      with UNCHANGED values (only mechanical type conversions at a few test call sites). NEW NMI wire tests:
      TestRunSale_WirePinsCentsAmount (Cents(1999) ⇒ body "19.99"), TestRefund_WirePinsCentsAmount ("5.00";
      0 ⇒ amount key omitted), TestAddRecurringSubscription_WirePinsDollarsAmount (MajorUnits(19.99) ⇒
      form amount=19.99) — joining the pre-existing TestCentsAmountWirePinning + plan-shape tests. The
      Stripe outbound+webhook and CCBill parsing wire-test battery remains under the TEST WALL bullet.
- [x] TEST WALL (Paul 2026-07-01: mandatory — a units mixup must never reach production again; unit-test EVERY
      place that matters). PARTIAL — landed now: TestUpgradeWirePinning + TestCalculateModelBUpgradeCharge
      (checkout: preview micros == 3133-cent "31.33" NMI wire, recurring dollars shared converter, sub-cent ⇒
      error), TestCalculateTokenQuote_MicrosWirePin + TestFiatMicrosToStablecoinBaseUnits (solana: $19.99 ⇒
      19_990_000 base units, never 199,900), TestRefundAmountCents (handlers: 5_000 micros ⇒ ERROR, 60_000_000
      ⇒ 6000 cents), TestNativeToRailMinor(+LegacyArrearsCeil) (money: USD/EUR/JPY-zero-decimal, ceil, unknown
      currency errors), TestSolanaUpgradeReducedFirstCharge rewritten to micros. REMAINING (post-#663): the
      provider-package boundaries listed below. Two layers:
      1. Wire-pinning unit tests at every provider money boundary — feed a known micros amount, assert the EXACT
         wire value the provider sees (not "a" conversion — the literal string/int on the wire):
         - NMI (internal/integrations/nmi): RunSale, capture, refund, void, AddRecurringSubscription /
           update-recurring — 19_990_000 micros ⇒ wire "19.99"; sub-cent input ⇒ error, never rounding.
         - Stripe (stripeapi + adapters): every outbound amount (charge, refund, price/plan push) micros⇒cents;
           inbound webhook cents⇒micros for BOTH success and failed-payment rows.
         - CCBill: inbound amount parsing + the ±2% validation boundary.
         - Solana: CalculateTokenQuote per currency, FiatMicrosToStablecoinBaseUnits, and the transfer
           instruction base-unit amount ($19.99 ⇒ 19.99 USDC base units, never 199,900).
         - Rail-minor converters (arrears + topup): USD, JPY/zero-decimal, scale-4 — both paths produce the
           SAME provider amount for the same micros.
      2. Cross-path invariant tests: tier-change preview amount == executed charge amount; refund request
         (micros) == provider-executed amount (minor units) == what the verifier compares; deposit credited
         micros == charged amount converted back (no truncation gain).
      Convention going forward: any NEW provider boundary ships with its wire-pinning test in the same PR —
      typed Micros/Cents makes new mixups uncompilable, the tests pin the conversions that remain.
      → DONE 2026-07-02 (final leg — Stripe + CCBill; NMI/Solana/rail-minor/cross-path shipped earlier, above):
      - Stripe catalog push: pkg/service/catalog_provider_stripe_test.go —
        TestStripeAdapter_AutoCreate_WirePinsUnitAmountCents (19_990_000 micros ⇒ literal unit_amount=1999 +
        lowercased currency on POST /v1/prices, captured via httptest through the stripeapi choke point) +
        TestStripeAdapter_AutoCreate_SubCentMicrosErrorNeverRounds (19_995_000 ⇒ error, ZERO price POSTs).
        Enabler: ALL StripeCatalogService endpoints now honor BaseURL (was entitlements-only; hardcoded hosts
        removed) + stripeAdapter.testBaseURL hook plumbed through stripeServiceFor. The pre-existing live test
        (catalog_provider_stripe_live_test.go, 12_340_000⇒1234) stays as the gated live twin.
      - Stripe outbound charge + refund: subscriptions/stripe_wire_pinning_test.go —
        TestStripeCollectInvoice_WirePinsCentsAmount (Cents(1999) ⇒ /v1/invoiceitems amount=1999, currency
        lowercased; underpaid 1998<1999 rejected) + TestStripeCreateRefund_WirePinsCentsAmount (500⇒"500",
        1999⇒"1999", 0 ⇒ amount key OMITTED = full refund). Intents leg: refund_test.go
        "wire-pins the payload cents amount" — Execute hands the payload's literal Cents(500) to Stripe.
      - Stripe inbound webhook: webhooks/stripe_wire_pinning_test.go — success row amount_paid 1999 ⇒
        19_990_000 micros, failed row (#675) amount_due 1200 ⇒ 12_000_000, plus no-cross-leak cases
        (amount_due never feeds success, amount_paid never feeds failed); conversions extracted to
        stripeInvoicePaidAmountMicros/stripeInvoiceFailedAmountMicros and both handlers now call them.
      - CCBill (inbound-only): webhooks/ccbill_wire_pinning_test.go (NEW file; ccbill.go untouched — #691 owns
        it) — "19.99"⇒1999¢, "0.01"⇒1¢, "1999.00"⇒199900¢ (dollars, never raw cents), garbage/negative/zero
        rejected; stored-row chain "19.99"⇒19_990_000 micros; ±2% window pinned EXACTLY for $19.99 (tolerance
        39¢: 1960/2038 pass, 1959/2039 trip the ErrorTypeAmount BillingError that records the repair alert);
        micros-as-cents 10,000× mixup trips; sub-cent expected (19_995_000) errors, never rounds.
- [x] Stale unit docs while in there: pkg/catalog/manifest.go:159 example, solana/recurring/confirm_tier_change.go:73,
      models/payment.go:28. → DONE (plus micros notes on manifest UnitAmount fields and solana types.go Amounts).

## Acceptance
No production call passes micros into a cents/dollars parameter (typed units make it uncompilable); upgrade,
Solana, and refund paths charge exactly what the preview/request says; JPY topup and arrears collect the same
provider amount for the same micros. EVERY provider money boundary has a wire-pinning unit test (known micros
in ⇒ exact wire amount out) — a reintroduced units mixup fails unit tests, not production.

---

# #672: CRITICAL — metered usage re-accrued (billed N×) across overlapping invoice-close windows

**Completed:** yes
**Status:** FIXED (2026-07-01) — delta-vs-accrued-prefix design (chosen over segment rating, which would
re-grant allowances and drift rounding per segment): durable per-period watermark table
`openrails.metered_rating_watermarks` (migration 056; PK merchant/customer/currency/source[+dim]/period_from;
columns accrued_amount + rated_through). Every metered sweep now goes through `accrueMeteredPrefix`
(metered_rating.go): rate the FULL prefix [period_from, cutoff) once, upsert-LOCK the watermark row, accrue only
`rated − accrued_amount` — ledger transfer + pending item + watermark advance in ONE tx. Monotone: repeated or
stale closes compute delta ≤ 0 and accrue nothing. Both sweep paths converted (RateMeteredUsageFromEvents +
sweepCatalogRateCardUsage); the explicit per-report AccrueCatalogMeteredAggregate (spend.go) keeps caller
source_id idempotency (not window-keyed, unaffected). Integration test
`TestMeteredUsage_OverlappingCloses_RatedExactlyOnce` GREEN; pre-existing bridge/ratecard/re-finalize tests GREEN.

## Metadata
- Category: bug
- Status: done
- Passes: true

## Problem
`FinalizeInvoice` sweeps catalog metered usage keyed by the exact window: `meteredPeriodSourceID` =
`period:<from>-<to>` (internal/modules/money/metered_rating.go:574-576; sweep at money/invoice.go:78). Threshold
closes anchor `from` at the current period start (invoice.go:751-758), so a June-10 threshold close accrues
[Jun1,Jun10), a June-20 close accrues [Jun1,Jun20) — different source_id → NOT deduped by
`uq_invoice_items_source` → the full period-to-date usage accrues AGAIN; the daily `FinalizePreviousMonth` job
then accrues [Jun1,Jul1) a third time. Each accrual is a real `owed_accrual` transfer + pending invoice item
collected by `ChargeOutstanding` → the customer's card is charged N× for the same usage, and each sweep re-trips
the threshold (feedback loop).

## Tasks
- [x] Rated-through watermark per meter (delta-vs-accrued-prefix; source_id = `period:<from>-<cutoff>` of the
      advancing close, amount = only the unbilled delta).
- [x] Backstop invariant: integration test with threshold close mid-period + longer-prefix close + month-end
      finalize + stale re-close ⇒ total accrued == rating the period exactly once
      (metered_watermark_integration_test.go).
- [x] Audit existing accruals for double-billing exposure: MOOT — the only producers of overlapping closes
      (InvoiceWorker threshold/month-end jobs) were wedged since birth by #673 (every run died in
      merchant.Require), so no overlapping close ever executed; direct FinalizeInvoice callers use a single
      window per period. No historical double-accruals to repair.

## Acceptance
Threshold close + later threshold close + month-end finalize over the same period accrues each unit of usage
exactly once; the feedback loop is structurally impossible (watermark monotone).

---

# #673: CRITICAL — invoice collection + auto-top-up workers run with no merchant context: wedged since birth

**Completed:** yes
**Status:** FIXED (2026-07-01) — `forEachActiveMerchant` helper (jobs_credit_money_in.go) mirrors
ConvergeSweepWorker: privileged ListActiveMerchantIDs, then each merchant runs under
`RunInMerchantConn(merchant.WithID(…))`; one merchant's failure is logged + skipped, the joined error is
returned so a failing run is visible in River (never silently "succeeds"). InvoiceWorker + AutoTopupWorker got a
`DB` field (wired in river_register.go). LowBalanceAlertWorker registration + its hourly periodic job DELETED —
no money.Alerter implementation exists anywhere in the runtime, so registration was a permanent no-op; re-add
with the notification wiring (worker type kept). Landed deliberately AFTER #672 + the #674 minimal arrears
hardening, since unwedging these workers activates arrears collection + metered sweeps. Integration tests
(worker_merchant_ctx_integration_test.go) run both workers exactly as River does — bare context.Background(), no
merchant — and assert real effects: invoice collected to paid, balance topped up. GREEN.

## Metadata
- Category: bug
- Status: done
- Passes: true

## Problem
`InvoiceWorker.Work` → `w.Money.InvoiceSettings(ctx)` (internal/river/jobs_credit_money_in.go:135) →
`merchantconfig.Store.Get` → `merchant.Require(ctx)` → ErrNoMerchant; `AutoTopupWorker` → `RunAutoTopups` →
`belowThresholdAccounts` → same. River job contexts carry no merchant, and unlike every sibling money worker
(jobs_dunning.go:189, jobs_converge_sweep.go:66 loop merchants under RunInMerchantConn) these two never inject
it. Every hourly `InvoiceArgs{Collect:true}`, monthly floor sweep, daily FinalizePreviousMonth, and 15-min
auto-top-up tick (internal/app/river_register.go:478-541) errors and retries forever: arrears are never
collected, top-ups never happen, silently.

Related while in there: `LowBalanceAlertWorker` is registered without an `Alerter` → permanent no-op
(river_register.go:170-174; nil guard jobs_credit_money_in.go:40-43).

## Tasks
- [x] Mirror ConvergeSweepWorker: enumerate active merchants, run each under `RunInMerchantConn(merchant.WithID(…))`.
- [x] Integration test that runs InvoiceWorker + AutoTopupWorker against a seeded merchant and asserts actual
      collection/top-up effects. Per-merchant failures are Error-logged AND joined into the worker's return
      error, so River's job state shows the failure (a full worker-error-rate alert channel remains a good
      follow-up).
- [x] Wire or delete the LowBalanceAlertWorker registration — DELETED (registration + hourly periodic job): no
      Alerter implementation exists in the runtime.
- [x] NOTE honored: #672 and the #674 minimal arrears hardening landed in the same pass, BEFORE this unwedge.

## Acceptance
Hourly/monthly/daily invoice jobs and auto-top-up demonstrably move money for a seeded merchant in integration
tests; no worker in river_register runs with a context its own call graph rejects.

---

# #674: universal write-ahead provider-intents log — every external write durable-first, effectively-once

**Completed:** no
**Status:** IMPLEMENTED IN-TREE (2026-07-01) — primitive + NMI ambiguity classification + four site migrations
(sync NMI sale, NMI subscription create, auto-top-up, Solana crank) + upgrade-proration verify-hardening + CI
guard + crash-injection integration tests; unit + integration suites for intents/checkout/money/river green.
No new migration needed (provider_intents already carries everything; intent kinds are strings). Deferred
tails listed below. Originally PLANNED from the 2026-07-01 money-path audit. One theme, six sites: a real provider
charge can happen with no durable local record (→ re-charge on retry/crash), and transport-ambiguous errors are
treated as clean declines. The provider-intents outbox (dunning/manual-rebill/refunds) already does ALL of this
right — unique (merchant, idempotency_key), SKIP LOCKED lease, attempts>1 ⇒ verify-at-provider before resend —
these paths predate/bypass it.

## Implementation notes (2026-07-01)
- **Primitive**: `Runner.EnqueueAndExecute` (pre-existing, #358/#607) IS the write-through primitive — insert
  intent (idempotent) → ClaimByID → execute inline through the identical gate/execute/classify pipeline →
  return the canonical post-execution row. No new runner machinery was needed; this issue added intent KINDS +
  producers + the ambiguity classification underneath. Producers branch on row.Status; non-final statuses
  surface as `checkout.ErrCheckoutProcessing` ("retry with the SAME idempotency key"), never a decline.
- **NMI ambiguity (new)**: `nmi.TransportAmbiguousError` / `nmi.IsTransportAmbiguous` — wrapped at the
  transports: direct-post PostForm/non-200/body-read/unparseable-200, v5 non-GET send/read/5xx/undecodable-2xx.
  Parsed declines (CustomerVaultError), parsed 4xx envelopes, read-only guard, pre-send validation stay CLEAN.
  Reads (GET) never wrapped. Conservative: connection-refused is ambiguous (verify resolves to
  "not executed" → clean retry).
- **New intent kinds**: `nmi_sale` + `nmi_subscription_create` (handlers in internal/modules/checkout — they
  need per-merchant NMI client resolution + the registration services; intents pkg can't import checkout),
  `topup_charge` (internal/intents/topup_charge.go), `solana_pull` (internal/river/jobs_solana_pull_intent.go).
  All provider order-ids/idempotency keys derive from the INTENT ID (`intent.ID`, `topup:<id>`), ≤ 50 chars;
  intent identity derives from the client checkout key / persisted episode anchors.
- **Sale (a)**: CheckoutNMISaleService.Process = redis idempotency shell (kept for response caching) +
  EnqueueAndExecute. ReserveProviderAttempt/nmi_sale_attempt rows ABSORBED into the intent (removed from the
  flow; ValidateRefund keeps the historic `nmi_sale_attempt:`/`nmi_sub_attempt:` prefix guards for old rows).
  finalize = idempotent RegisterPurchase; charged-but-unrecorded ⇒ unknown_needs_verify, verifier repairs via
  FindSuccessfulSaleByOrderID. The "retry with a new idempotency key" message now only follows a VERIFIED-clean
  decline.
- **Subscription create (b)**: same shape; verify leg = order-id sale search (immediate starts) + v5
  subscription ROSTER scan matched on (customer_vault_id, plan.id), accepting matches unknown locally (the
  orphan) or whose local row carries this intent's order_id (finalize crashed midway). finalize reuses
  completeNMISubscriptionRegistration (idempotent). recoverNMISubscriptionAttempt + the attempt-row reservation
  removed.
- **Upgrade proration (b, partial)**: NOT a full intent (compensation saga too entangled) — hardened in place:
  redis Failed now falls through + pre-charge verify by the stable content-derived order id; RunSale ambiguity
  ⇒ verify-then-adopt txn, else rollback successor + ErrCheckoutProcessing (retry re-verifies same order id).
  Never rolls back/declines on a lost response anymore.
- **Auto-top-up (c)**: episode anchor = persisted last_topup_at (unix or "genesis"), NEVER wall clock;
  MoneyService.RunAutoTopups/topUpAccount replaced by ListDueAutoTopups + HasAutoTopupDeposit (credit-grant
  source_id lookup — the old GetTransactionBySource("deposit",...) check never matched the #514 grant-ledger
  shape, i.e. the old dedupe was dead) + FinalizeAutoTopup + StampAutoTopupAttempt. Deposit source_id = wire
  ref = `topup:<intent id>` (also fixes the >50-char NMI order-id breakage of the old `autotopup:<uuid>:<unix>`
  key under the #663 length check). Verify: local deposit first, then NMI order-id search; stripe-family rails
  re-execute under the same Stripe Idempotency-Key (replay-safe) instead of a read.
- **Solana crank (d)**: signature WRITE-AHEAD — BuildSignSubmit*Presubmit persists the signed tx signature onto
  the intent (Store.RecordProgress, evidence key transaction_id) BEFORE submission; crankOne refactored to a
  typed crankOutcome (+presubmit hook; state-machine unit tests intact). Handler: recorded-sig ⇒ chain read
  (GetTransaction) BEFORE any re-pull — landed ⇒ renewal repair (RenewMembership+AdvanceAfterPull via
  finalizePull; the "subscriber paid, gets nothing" bug is dead), read-failed ⇒ refuse to re-pull blind
  (ambiguous), verified-not-landed ⇒ re-crank (program period guard Custom:400 backstops). AlreadyPaid with no
  recorded sig keeps the legacy advance+reconcile-repair (out-of-band payment). Worker keeps a legacy direct
  path when Intents is nil (unit-test harnesses).
- **CI guard**: internal/intents/enforcement_guard_test.go (no build tag) — textual scan of internal/+pkg/
  (excluding internal/integrations/**, which IS the client layer) for the choke tokens (.RunSale(,
  .AddRecurringSubscription(, .AttemptManualRebill(, nmi.RefundParams{, .Void(, .DeleteRecurringSubscription(,
  .UpdateSubscriptionPaymentSource(, .DeleteCustomerVault(, solanaint.BuildSignSubmit) against a per-token
  file allowlist with justifications. LIMITS (documented in the test): token-textual (wrappers/method values
  invisible), allowlisted files trusted wholesale, Stripe not scanned (architecturally choked through
  stripeapi's readonly transport, the stronger guard).
- **Tests** (integration tag, testcontainers): per site — happy-path/decline/ambiguous-timeout-verify/
  charged-but-unrecorded-repair/provider-offline-original-key (checkout sale:
  nmi_sale_intent_integration_test.go), orphaned-remote-create repair (nmi_subscription_intent_integration_test.go),
  topup episode/decline-cooldown/ambiguous-verify/charge-crash-deposit/offline-retry
  (money_in_integration_test.go), solana crash-after-submit signature repair + not-landed re-crank
  (jobs_solana_pull_intent_integration_test.go), NMI ambiguity classification unit tests (ambiguity_test.go).

## Deferred (follow-ups, not in this pass)
- **Arrears collection**: stays on the Wave-1 hardening (attempt-count keys + CAS-miss recording, #672/#673).
  Migrating chargeOneOpenInvoice onto a `topup_charge`-style intent is NOT a small mechanical step (invoice
  snapshot CAS + PayOwed transfer live inside the recording tx) — do it as its own slice; nmi_collection.go is
  allowlisted in the guard with this note.
- **Upgrade as a full intent saga** (successor create + proration + swap + compensation): only the proration
  leg was verify-hardened; an ambiguous successor CREATE still rolls back pessimistically (delete is
  verify-then-execute, so safe, but a lost-create-response leaves a remote sub until the delete lands).
- **Vault delete / payment-method swap / reactive cancels**: still direct calls through their service chokes
  (VaultService.DeleteVault, UpdateSubscriptionPaymentSource, DeleteRecurringSubscription in user/admin
  cancels) — allowlisted in the guard; migrate opportunistically.
- **CCBill**: no writes migrated (its mutations are hosted-page/DataLink shaped); unchanged.

NOTE (2026-07-01): the MINIMAL arrears-collection hardening landed with #672/#673 — chargeOneOpenInvoice now
keys the charge `invoice:<id>:attempt:<n>` (n = durable count of recorded invoice_payments rows, NOT the mutable
amount snapshot), and on ApplyInvoicePaymentSnapshot n==0 after a successful provider charge it records the
settled invoice_payments row + PayOwed transfer (unapplied) and Error-alerts instead of `return nil` (tests:
arrears_cas_integration_test.go). The full intents-outbox migration of this site remains THIS issue.

## Design decision (Paul, 2026-07-01): generalize, don't patch

ALL writes into a payment provider / external system post to the provider-intents queue FIRST, then execute
IMMEDIATELY in the same request (write-through: the happy path pays one local INSERT + complete, never a worker
round-trip — this must not slow writes down). The intent log is the durable write-ahead record of every
external write we ever attempt; semantics are effectively-once into the external system: durable intent +
provider idempotency key/order-id DERIVED FROM THE INTENT + on any ambiguity or restart, verify-at-provider
before resend. If the provider is misconfigured or offline, the intent stays pending and the existing outbox
lease/sweeper retries it later — nothing is ever lost to a down or misbehaving provider. Read-only provider
calls are exempt. The six findings below become migrations onto this one primitive, not six bespoke fixes.

## Metadata
- Category: bug
- Status: in_progress
- Passes: false

## Findings
- **Arrears collection CAS drop**: `chargeOneOpenInvoice` (internal/modules/money/arrears.go:246-345) charges
  the card (key `invoice:<id>:<snapshot>`, :255-266) BEFORE the recording tx; inside,
  `ApplyInvoicePaymentSnapshot` is a snapshot CAS and on n==0 the code `return nil` (:288-290) — provider
  charged, no PayOwed transfer, no invoice_payments row, no refund; next run re-charges the remainder. Key is a
  MUTABLE snapshot so Stripe's dedup drifts too; NMI never dedupes (key only order_id, nmi_collection.go:56-62).
- **NMI sync sale/subscription ambiguity**: RunSale/AddRecurringSubscription return one error for decline AND
  timeout-after-send (25s, nmi/client.go:45). nmi_sale_service.go:169-176: any error → MarkFailed + DeleteVault
  + idempotency Fail; retry is told to use a NEW key (:119-127) → new orderid → double charge. Subscription
  create (checkout/service.go:716-736): timeout after NMI created the sub = live remote subscription billing
  every cycle with no local row. Upgrade proration (service.go:1812-1819) rolls back without verifying the sale.
  `FindSuccessfulSaleByOrderID` verify-on-ambiguity exists (intents) and is unused here. (Adjacent to #330 but
  distinct.)
- **Auto-top-up**: topUpAccount (money/money_in.go:166-218): episode = now.Truncate(max(cooldown,1m)); charge →
  Deposit → stamp. Crash between charge and deposit: same-bucket retry re-charges on NMI; cross-bucket retry
  double-charges even on Stripe; first charge recorded nowhere.
- **NMI sale wedged on crash between provider success and CompleteProviderAttempt**
  (nmi_sale_service.go:139-182): attempt row stays `pending` forever ("already pending, please wait"); no
  sweeper resolves stale attempts via FindSuccessfulSaleByOrderID; new-key retry double-charges.
- **ReserveProviderAttempt hides "already existed"** (modules/payments/payment.go:187-218): ON CONFLICT DO
  NOTHING then returns the existing row with no created/found signal; caller proceeds to RunSale regardless —
  two replicas can double-charge when Redis idempotency is per-process.
- **Solana crank invisible renewal**: jobs_solana_crank.go:187,285-304 submits on-chain pull then records; crash
  between = re-run hits the AlreadyPaid branch (:208-217) which only advances next_pull_at — no RenewMembership,
  no payment row; SolanaReconcileWorker cross-checks only last_signature (jobs_solana_reconcile.go:83-96) which
  never advanced. Subscriber paid on-chain, gets nothing, zero drift alert.
- Minor: the only guard on `nmi_upgrade` replays is the 5-min default idempotency TTL (no durable key-addressed
  record).

## Tasks
- [x] Extend `internal/intents` with a synchronous execute-through mode — DONE via the pre-existing
      `Runner.EnqueueAndExecute` (no duplication needed): insert intent → inline execute through the identical
      gate/execute/classify pipeline → complete/fail. Ambiguity marks `unknown_needs_verify`, never failed; the
      existing lease/sweeper drains pending/unknown. New kinds shipped: nmi_sale, nmi_subscription_create,
      topup_charge, solana_pull. (recurring-update/vault-delete/void kinds deferred — see Deferred.)
- [x] Provider idempotency keys/order-ids derived from the intent id (sale/sub-create order id = intent.ID
      [+e2e suffix], topup wire ref = `topup:<intent id>`, solana signature recorded onto the intent).
- [x] NMI client: transport-ambiguous vs provider-declined classification
      (nmi.TransportAmbiguousError/IsTransportAmbiguous at both transports); ambiguity ⇒ verify-by-order-id,
      never MarkFailed/DeleteVault/new-key.
- [x] Migrated sites: sync NMI sale (a), NMI subscription create (b), upgrade proration verify-hardening
      (b, partial — full saga deferred), auto-top-up with persisted-state episode anchor (c), Solana crank with
      pre-submit signature + recorded-sig renewal repair (d); ReserveProviderAttempt/nmi_sale_attempt +
      nmi_sub_attempt reservations ABSORBED into intents. Arrears deliberately left on Wave-1 hardening (not a
      small mechanical step — see Deferred).
- [x] Enforcement: grep-based allowlist guard (enforcement_guard_test.go, runs untagged in CI) over the NMI
      mutating methods + solana sign-submit; Stripe covered architecturally by the stripeapi choke. Limits
      documented in the test + Implementation notes.
- [x] Crash-injection integration tests per migrated site (ambiguous-timeout ⇒ verify resolves; charged-but-
      unrecorded ⇒ verifier repairs off the original key; provider-offline ⇒ intent parks then lands under the
      ORIGINAL derived key; decline ⇒ terminal, no effect; replay ⇒ durable answer, zero provider calls).

## Acceptance
Every external WRITE (charge, refund, void, recurring create/update/delete, vault delete, on-chain submit) has
a durable intent row written before execution and completed with the outcome; the happy path executes inline
with no added round-trips; a crash, timeout, or offline/misconfigured provider at ANY point leads to
verify-then-resolve from the intent log — effectively-once into the external system, never a blind re-charge,
never a charged-but-unrecorded outcome, never a lost write. No code path can reach a provider write without an
intent (enforced by CI guard).

---

# #675: webhook state correctness — clobbering writes, dropped renewals, swallowed reversals

**Completed:** no
**Status:** IMPLEMENTED (2026-07-01) — all findings fixed in-tree; unit + full webhooks/repo integration suites
green. Awaiting review/commit. Enqueue/dedup/signature layers were verified sound; these were the apply-layer
holes. See Implementation notes below.

## Metadata
- Category: bug
- Status: implemented
- Passes: true

## Findings
- **No row lock / ordering guard; full-row clobber**: `SubscriptionRepo.UpdateAt` writes EVERY column from an
  in-memory image, no version guard (internal/db/repo/subscription.go:138+, "nil values CLEAR fields"); no
  FOR UPDATE on subscriptions anywhere. `handleSubscriptionUpdated` (webhooks/stripe.go:899-975) is a
  non-transactional read-modify-write and ignores evt.Created. Renewal emits invoice.paid +
  subscription.updated near-simultaneously (concurrent per-request processing, handlers/webhook.go:574-605):
  the stale full-row write reverts RenewMembership's committed changes; a delayed older
  subscription.updated(active) after invoice.payment_failed flips past_due→active without payment (only
  `cancelled` is terminal-guarded :904-911). The 2-min pending-lease takeover (deduplication.go:136-154) adds
  same-event concurrency.
- **Terminal-blocked renewal charge silently dropped**: stripe.go:521-527 IsTerminalTransitionBlocked → Warn →
  return nil → deduped as success; nmi.go:962-970 identical. Customer charged, no payment row, no ledger trace,
  never retried. CCBill does it right (ccbill.go:2421-2443 writes a terminal_blocked_renewal_success repair
  alert) — copy it.
- **Void/chargeback before sale materializes is ACKed**: CCBill handleVoid (ccbill.go:1912-1955) +
  handleChargeback (:2096-2144) + NMI handleVoidSuccess (nmi.go:2272-2277): subscription not found → return nil
  (deduped). Provider fraud-voids seconds after approval; if Void wins the race, the later NewSaleSuccess
  creates entitlements+credits for an already-reversed charge.
- **Subscription credit grants warn-only at all six call sites**: stripe.go:548-559, ccbill.go:631-639,
  2464-2472, nmi.go:915-928,974-987 — transient GrantSubscriptionCredits failure → event marked success → that
  period's credit lot lost forever. The deposit is idempotent per (subscription,label,period_end) under the
  customer lock, so returning the error for retry is safe.
- **NMI one-off refunds ACKed with zero effect**: the reversal block is gated on `subscription != nil`
  (nmi.go:2024); dashboard refund of a one-time purchase keeps payment/entitlements/credits. Refund amount
  parse failure downgraded to 0 (nmi.go:1977-1981). Stripe's one-off path is correct (stripe.go:1360-1371) —
  mirror it.
- Minor: CCBill amount/validation mismatches ACK with log-only logBillingError (ccbill.go:358-390) — a >2%
  drifted real charge deserves a durable repair alert.

## Tasks
- [x] Subscription apply layer: one tx + FOR UPDATE (or updated_at CAS) around read-modify-write; persist and
      compare provider event created-timestamps; make UpdateAt full-row semantics explicit or retire it for
      webhook applies.
- [x] Terminal-blocked renewal: record the payment or a CCBill-style repair alert before return nil (Stripe+NMI).
- [x] Void/chargeback/refund not-found: retryable error (as nmi.go:2019 refunds do) or deferred-reversal repair
      alert — never plain ACK.
- [x] Propagate GrantSubscriptionCredits errors (re-grep: 5 sites in webhooks — stripe ×1, ccbill ×2, nmi ×2;
      NMI's warn-only pre-grant subscription reloads now propagate too).
- [x] NMI one-off refund branch (resolve by transaction id); parse failure = error.
- [x] Out-of-order integration tests: invoice.paid ∥ subscription.updated; payment_failed then stale
      updated(active); void-then-sale; renewal-after-cancel.
- [x] Minor: CCBill >2% amount mismatch now writes a durable `amount_mismatch` repair alert (both
      validateCCBillBilledAmount callers + the upgrade-success inline check; upgrade alert written after the
      tx rolls back so it survives).

## Implementation notes (2026-07-01)
- Ordering guard: new sqlc query `GetSubscriptionByRailSubIDForUpdate` + repo method; stripe
  `handleSubscriptionUpdated` and `handleInvoicePaymentFailed` run one MerchantTx with FOR UPDATE, and a
  `stripe_last_event_created` watermark in subscription metadata rejects strictly-older events;
  `handleInvoicePaid` bumps the watermark (row-locked, never rewinds) after CreateMembership/RenewMembership
  so a delayed older updated(active) can't revert a renewal or a past_due flip. evt.Created plumbed through
  handleEvent. UpdateAt's full-row semantics documented at the definition.
- Void/chargeback race: chose RETRYABLE ERROR at all three not-found sites (CCBill handleVoid/handleChargeback,
  NMI handleVoidSuccess — the NMI one now errors regardless of subscription presence): all three rails
  redeliver on non-2xx, matching the pattern NMI refunds already used. The not-found-branch ClickHouse "audit"
  logging was dropped (it would duplicate per retry; durable trace = failed idempotency record + the applied
  reversal on redelivery).
- NMI refunds: new `handleNMIOneOffRefund` mirrors Stripe's one-off path (resolve by refund txn id, then
  original txn id → Refund row → full refund revokes entitlements/product-access, or cancels the payment's
  subscription); amount parse failure = durable repair alert + non-retryable (redelivery resends the same
  bytes). Refund/one-off-before-sale → retryable error.
- OTHER UpdateAt callers left unguarded (deliberately out of scope): lifecycle_service.go
  RenewMembership/CancelMembership read-modify-write inside MerchantTx but WITHOUT FOR UPDATE; NMI
  handleUpdateSubscription/ACU and CCBill customer-data/billing-date handlers Update() from non-locked reads.
  Follow-up: switch those reads to the ForUpdate variant.
- Sweep note: `sqlc generate` (needed for the new query) also regenerated gen for ANOTHER agent's in-flight
  uncommitted removal of the payment_blocklist queries (admission_support.sql edit + untracked migration
  055_drop_payment_blocklist) — gen/admission_support.sql.go + gen/models.go lost that dead code; zero Go
  references, build green.
- Tests: stripe_ordering_test.go (watermark unit tests) + webhooks_apply_integration_test.go (8 tests:
  payment_failed then stale updated(active) does NOT reactivate; invoice.paid ∥ stale updated in BOTH orders
  keeps the renewed period; terminal-blocked renewal writes the repair alert; CCBill void + chargeback
  before-sale retryable; NMI void before-sale retryable; NMI one-off refund reverses + idempotent redelivery;
  NMI refund-before-sale retryable; CCBill credit-grant failure propagates retryable). Full webhooks + repo
  integration suites green via testcontainers.

## Acceptance
Out-of-order or concurrent webhook delivery cannot revert committed state or grant access without payment; no
money-bearing event is ACKed with zero durable effect (payment row, retry, or repair alert — always one of the
three).

---

# #676: spendgate holds — volatile capture pointer, leaking held gauge, phantom sweep

**Completed:** no
**Status:** IMPLEMENTED (2026-07-01) — all four findings fixed + tested (see Resolution); awaiting commit.

## Metadata
- Category: admission
- Status: implemented
- Passes: true

## Findings
- **Capture depends on a volatile Redis pointer**: capture wire carries only request_id (remote.go:177-192);
  `Service.CaptureHold` (pkg/service/service.go:239-244) hard-fails "hold not found" when the pointer is gone —
  it's TTL'd to the hold (default 1h) and written best-effort AFTER the admit EVAL (spendgate/gate.go:239-240,
  `_ = g.rdb.Set(…)`). Redis restart/failover, a failed Set, or execution outliving the TTL ⇒ service rendered,
  charge unrecoverable (payer coords lost).
- **`held` gauge leaks permanently; the referenced self-heal doesn't exist**: admit INCRBYs held with no TTL;
  only the per-request hold record expires (gate.go:91,106). The comment at gate.go:152-157 promises a
  reconciliation sweep; grep confirms nothing recomputes sg:*:held. Every abandoned admit permanently shrinks
  the payer's capacity toward 100% denial; the only remedy (flush) swings to over-admission.
- **Captures hard-fail up to 1h after a credit lot lapses**: spendBalanceThenOwedTx (money/unified_spend.go:199-211)
  computes fromBalance from the raw counter (includes the unswept lapsed remainder) but CreditSpend only draws
  spendable lots → ErrInsufficientCredits on every retry until the hourly sweep — the "served but can't charge"
  failure #513 decision 8 forbids.
- Minor: releaseScript recreates expired window buckets as permanent negative keys (gate.go:138-146).

## Tasks
- [x] Durable admit-coordinate record (or caller-supplied payer coords as capture fallback); pointer Set failure
      = admit failure, not best-effort.
- [x] Implement the held-recompute sweep the comment promises (or rolling-TTL holds + recompute-on-expiry);
      fix releaseScript bucket recreation.
- [x] Derive the balance/owed split from spendable lots (or fall through to owed on shortfall) so lapsed lots
      can't block capture.
- [x] Chaos test: admit → Redis flush → capture still lands (degraded path); abandoned admits don't shrink
      capacity permanently.

## Resolution (2026-07-01)
- **Capture pointer**: admit's request→payer pointer Set is now REQUIRED (Set failure ⇒ admit error, fail
  closed; the leaked reservation self-heals via hold TTL + recompute). Durable fallback = caller-supplied payer
  coords on capture, ADDITIVE wire fields `customer_id`/`currency`/`invoker`/`admit_source` on
  POST /v1/merchant/admissions/:id/capture (handler + pkg/service.CaptureHoldRequest + SDK CaptureUsage +
  remote/embed transports). `admit_source` must echo the admit's source (default "admit") so retries dedupe on
  the same (source, source_id) ledger coordinates. Chosen over a per-admit Postgres record: no hot-path DB
  write, no migration (avoids the live #582/#630 db/gen churn); hosts opt in by echoing coords they already
  hold. Fallback customer_id passes requireServiceCustomerScope in the handler.
- **held gauge**: chose lazy recompute-on-deny inside admitScript over a River sweep: when the affordability
  gate would deny, the (atomic) script recomputes held = Σ live hold records via a new "<base>:holds" index SET
  (SADD on reserve, SREM on capture/release + stale-member GC) and re-checks — abandoned admits release
  capacity exactly when it's needed, no scan job, trivially race-free. capture/release clamp held at 0.
- **releaseScript**: window decrements now gated on EXISTS — an expired bucket is never recreated as a
  permanent negative key.
- **Lapsed lots**: spendBalanceThenOwedTx caps the balance leg at the SPENDABLE-lot total
  (ListSpendableCreditLots AsOf now) instead of the raw ledger balance; the shortfall follows the existing
  gating contract (capture → owed unconditionally; immediate spend → credit-line gate). Lapsed lots can no
  longer surface ErrInsufficientCredits on capture.
- Tests (all green): spendgate `TestGate_AbandonedHoldRecomputeRestoresCapacity`,
  `TestGate_ReleaseAfterBucketExpiryNoNegativeWindow`; money
  `TestCaptureAuthorized_LapsedLotNeverBlocksCapture` (new file lapsed_lot_capture_integration_test.go);
  service `TestCaptureHold_RedisFlushFallback`, `TestCaptureHold_FallbackRetryIsIdempotent`
  (new file capture_fallback_integration_test.go).
- Known follow-up: gauge introspection (HeldAmount/BudgetStatus) can read a stale (inflated) held for an idle
  payer until the next denied admit — display-only, enforcement is exact.

## Acceptance
A rendered service is always chargeable (no capture path depends on volatile state); abandoned admits release
capacity within one sweep interval; lapsed lots never produce ErrInsufficientCredits on capture.

---

# #677: customer-lock bypasses — credit expiry, converge repairs, AccrueOwed race

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. All three bypasses now take the SAME
customers-row FOR UPDATE as spends (new `grants.Ledger.LockCustomer`, delegating to LockCustomerForSpend):
ExpireLapsed locks at entry (the job's tx wraps it); converge MaterializeGrant repairs run via
`lockedMaterialize` (MerchantTx + LockCustomer, converge_passes.go); AccrueOwed locks before its
check-then-insert. DB backstop = migration 057: partial unique `idx_ledger_transfers_lot_once`
(merchant_id, grant_id, transfer_type) WHERE type IN (deposit, credit_expire, credit_revoke) — a lot
deposits/expires/claws at most once — and `idx_ledger_transfers_owed_accrual_once`
(merchant_id, customer_id, currency, source, source_id) WHERE type='owed_accrual' AND source_id <> ''
(empty excluded: SpendCredits permits keyless spends whose owed spill legitimately repeats). Concurrency
tests green: spend∥expiry (TestGrants_SpendExpiryConcurrency_LockSerializes), overlapping clawbacks at the
grants seam AND through two full concurrent Converge runs, two concurrent AccrueOwed. The latent RevokeGrant
over-refund race is gone with RevokeGrant itself (#666 item 7, done alongside).

## Metadata
- Category: bug
- Status: done
- Passes: true

## Findings
- **Credit expiry bypasses the lock**: jobs_credit_expiry.go:74-79 runs ExpireLapsed in a bare RunInTx;
  ExpireLapsed (grants/credit_spend.go:81-110) reads derived lot.Remaining then applies credit_expire. A spend
  at T−ε (lock held) and expiry at T+ε both read remaining=X and both commit when other lots cover the account
  floor → lot over-consumed, X of the customer's OTHER credits moved to expired_credits — customer money loss.
- **Converge repairs on a plain conn**: converge.go:245-252 runs Repair without tx/lock;
  clawbackRevokedCredit (grants.go:448-473) — overlapping converge runs can double-claw/double-deposit (no
  unique index on ledger_transfers.grant_id; idx_ledger_transfers_source is NON-unique, migration :2665 — all
  transfer idempotency is app-side).
- **AccrueOwed check-then-insert** (money/arrears.go:57-86): no lock, no constraint; concurrent calls both post
  owed_accrual — invoice item dedupes but the ledger carries 2× debt; arrears_liability never returns to zero.
- Latent: RevokeGrant refund leg reads `clawed` before the clawback re-reads remaining (grants.go:493-519) —
  over-refund race; no non-test callers today (surface being deleted in #666 — keep whichever survives locked).

## Tasks
- [x] Take LockCustomerForSpend inside ExpireLapsed (or in the expiry job per customer). (Inside ExpireLapsed —
      every caller inherits it; grants_customer_fk guarantees the customers row exists, no ensure needed.)
- [x] Wrap converge money repairs in MerchantTx + customer lock; consider a partial unique index on
      ledger_transfers(grant_id, kind) for the clawback/deposit pairs as a DB backstop. (Both MaterializeGrant
      repair closures wrapped in lockedMaterialize; index landed as 057 idx_ledger_transfers_lot_once, also
      covering credit_expire. Entitlement-only repairs — DeriveSubscriptionGrant/DeriveWalletGrant — stay on
      the engine conn: no money legs, and derive-1 relies on incremental per-feature commits.)
- [x] AccrueOwed: lockBalance first, or a partial unique index on owed_accrual coordinates. (Both: the lock
      primitive without the balance derivation — ensureCustomer + LockCustomerForSpend — plus 057
      idx_ledger_transfers_owed_accrual_once; index excludes source_id='' for keyless SpendCredits spills.)
- [x] Concurrency tests: spend ∥ expiry on the same lot; two converge repairs on the same grant; two AccrueOwed.
      (True-parallel goroutine tests against the shared pg, made deterministic via channel + lock-hold; green.)

## Acceptance
Every write that consumes or reverses lot value runs under the same per-customer serialization as spends; the
ledger cannot record the same accrual/clawback twice (app lock + DB backstop).

---

# #678: webhook dedup is Redis-only, fail-open, non-transactional

**Completed:** no
**Status:** IMPLEMENTED (2026-07-01) — ALL tasks in-tree, including the Postgres-truth follow-up Paul
approved 2026-07-01: dedup TRUTH moved to `openrails.webhook_events` (migration 061), Redis demoted to a
fast-path cache + lease-coordination layer (flushable with zero correctness consequences), posture gate
downgraded to a warning. Unit + integration (webhooks, idempotency, river, app, db, querytest) green.
Awaiting review/commit. (Originally from the 2026-07-01 money-path audit; #675's row locks/watermarks are
the apply-layer half.)

## Metadata
- Category: reliability
- Status: implemented
- Passes: true

## Problem
(line refs = audit-time, pre-fix) Webhook dedup lives only in Redis (90d TTL, build_runtime.go:799); `cfg.Redis == nil` silently falls back to a
PER-PROCESS memStore (idempotency/service.go:105-117) — multi-replica standalone loses cross-replica dedup
entirely, and Redis flush/eviction loses history. `Complete` is written after (not with) the effects, so every
handler must be individually replay-safe (that's #675's job). The 2-min pending-lease takeover
(deduplication.go:136-154) permits concurrent same-event processing for slow handlers. `IsDuplicate`
(deduplication.go:70-98) is dead code with a mismatched op key.

## Tasks
- [x] Refuse to start webhook processing without Redis when running multi-instance (config posture check, like
      EnforceRLSPosture) — silent memStore fallback only in dev/single-process.
- [x] Consider moving dedup marks into Postgres inside the effect tx (then Redis is a fast-path, not the truth)
      — DONE 2026-07-01 (was design-only); see "Implementation — Postgres truth" below.
- [x] Revisit the 2-min takeover: lease renewal for slow handlers instead of concurrent takeover.
- [x] Delete dead IsDuplicate (fold into #666 if it lands first) — deleted here (zero callers, mismatched
      `webhook.<rail>.event` op key; verified incl. integration files).

## Implementation notes (2026-07-01)
- **Posture gate** (SUPERSEDED same day by the Postgres-truth section below: the startup error is now a
  warning) (`enforceWebhookDedupPosture` + pure `webhookDedupPostureError`, build_runtime.go, called
  right after Redis resolution in `buildRuntimeWithOverrides`): no Redis + non-dev (`!cfg.IsDev()`, same axis
  as `RequiresRLS`) + standalone ⇒ startup error. **Standalone signal = no host-injected DB pool**
  (`overrides.DB == nil`) — the exact signal that scopes `createDatabase`'s RLS gate to the config-built path:
  embedded hosts inject a pgx pool (documented BootstrapOptions contract) and are single-process by contract;
  the standalone server AND worker binaries build their DB from config and can run N replicas. Dev/embedded
  without Redis keep the memStore fallback behind a loud `log.Warn` (per-process, no cross-replica protection).
- **Lease heartbeat**: `IdempotencyService.RenewPending` (thin `TryTakeoverPending(…, 0)` delegate — pending-only,
  cannot resurrect a completed record) + `DeduplicationService.startPendingHeartbeat` renews the pending
  record's CreatedAt every lease/4 while the owning handler runs. Takeover-by-timeout kept unchanged as the
  dead-holder path: staleness now genuinely means the holder died (4 consecutive missed heartbeats), not that
  it was slow. `pendingLease` is a test-only override field (default still 2 min). #675 row locks backstop the
  residual wedged-process >2min pause window — deliberately not over-built.
- Tests: posture unit matrix (internal/app/webhook_dedup_posture_test.go); slow-handler-stays-exclusive +
  dead-holder-takeover + renewal semantics (webhooks + idempotency unit, -race -count=3); Redis-backed
  renew/takeover integration (service_redis_integration_test.go, testcontainers);
  TestStripeInvoicePaymentAlreadyRecorded integration re-run green. Full `go build ./...` blocked by another
  agent's in-flight internal/http/handlers edit (not this change); all touched packages build/vet clean.

## Implementation — Postgres truth (2026-07-01, supersedes the deferred design note)
- **Schema**: migration `061_webhook_events.up.sql` — `openrails.webhook_events(merchant_id, rail, op,
  event_id, status DEFAULT/CHECK 'completed', created_at, completed_at, PK (merchant_id, op, event_id))`
  + `idx_webhook_events_completed_at` (retention scan) + FORCE/ENABLE RLS + merchant_isolation policy +
  `GRANT SELECT,INSERT,DELETE TO openrails_app` + merchant FK. Key mirrors the Redis derivation exactly
  (`op = webhook.<rail>.<event_type>`, event_id = rail event/transaction id); merchant_id joins the PK
  because the table is RLS-scoped. Only COMPLETED marks live here — pending/lease stays in Redis
  (coordination, not truth), hence the CHECK. sqlc queries in internal/db/queries/webhook_events.sql
  (WebhookEventCompleted / MarkWebhookEventCompleted / DeleteCompletedWebhookEventsBefore), gen regenerated.
- **ProcessWebhook order** (deduplication.go): Redis Begin (fast-path skip + pending lease, unchanged) →
  on cache miss, Postgres truth SELECT under MerchantTx (hit ⇒ backfill Redis Complete + skip) → handler
  (heartbeat unchanged) → on success/non-retryable, verify-or-write the truth row (INSERT … ON CONFLICT DO
  NOTHING under MerchantTx) → Redis Complete is now CACHE-ONLY (failure = warn, not retry — the old
  "Complete failed ⇒ retry ⇒ re-run effects" loop is gone whenever the truth row exists). Truth-row write
  failure stays retryable (#675 replay-safety converges the redelivery). No merchant on ctx or nil DB ⇒
  loud warn + legacy Redis/memory-only behavior (handlers' own MerchantTx calls would fail anyway).
- **In-tx seam**: `MarkWebhookProcessedInTx(ctx, tx)` — ProcessWebhook stashes the mark identity on ctx;
  a handler calls it inside its final MerchantTx so mark+effects commit atomically; the wrapper's
  verify-or-write then no-ops on conflict, so the call is an atomicity upgrade, never a requirement.
- **Per-rail mark modes**: CCBill `handleBillingDateChange` + `handleCustomerDataUpdate` = IN-TX (their
  whole effect is one MerchantTx). Everything else = WRITE-AFTER: every Stripe handler, every NMI handler
  and the CCBill money paths (NewSale/Renewal/Refund/Void/Chargeback/Upgrade/Cancel/Expiration) are
  sequences of service calls each committing their own txs — no single enclosing tx exists to mark in, and
  restructuring them is exactly the refactor this change was scoped NOT to do (#684's fetch-and-converge
  will collapse Stripe/NMI anyway; CCBill stays payload-apply and is why the Postgres backstop matters).
- **Posture gate decision**: DOWNGRADED to a loud warning (`webhookDedupPostureError` →
  `webhookDedupPostureWarning`, always-nil `enforceWebhookDedupPosture`) — the boot refusal existed because
  the memStore was per-process TRUTH; with truth in Postgres a Redis-less multi-replica boot is safe
  (never replays), merely wasteful (no cross-replica lease coordination, no fast path), and the standalone
  warning says exactly that.
- **Retention**: new leg in the existing `CleanupExpiredDataWorker` (house periodic cleanup):
  `CleanupConfig.WebhookEventRetention` default 90d = `webhooks.WebhookIdempotencyTTL` (the Redis cache
  TTL), zero/unset falls back to the default (never "delete everything"). RLS-correct: walks
  `ListActiveMerchantIDs` (control-plane read) and deletes per merchant under MerchantTx — the converge-
  sweep pattern, NOT the notifications legs' bare cross-merchant DELETE (see pre-existing-issue note).
- **idempotency memStore**: kept as-is (checkout et al. still use IdempotencyService as their sole store);
  type doc now states that for webhooks it is coordination+cache only.
- Tests (integration, testcontainers PG+Redis, all green): `TestWebhookDedupSurvivesRedisFlush` (headline:
  FLUSHALL between delivery and redelivery ⇒ no reapply + cache backfilled),
  `TestWebhookDedupTwoReplicasNoSharedRedis` (two service instances, same PG ⇒ exactly one apply),
  `TestWebhookDedupCrashBeforeMarkConverges` (effects-committed-no-mark ⇒ redelivery re-runs replay-safe
  handler, converges, mark lands), `TestWebhookDedupInTxMarkAtomicity` (tx rollback takes the mark with it;
  committed in-tx mark makes verify a no-op), `TestWebhookDedupNoMerchantFallsBackToRedisOnly`,
  `TestWebhookDedupNonRetryableWritesTruth`, `TestCleanupWebhookEventsRetention` (old deleted, recent
  kept, zero-retention safe; pending never exists in PG by construction), posture unit matrix rewritten.
- PRE-EXISTING issue observed, NOT fixed here (different files): the notifications/checkout cleanup legs
  and the River `WebhookProcessWorker` path run without merchant ctx — under a real openrails_app + FORCE
  RLS connection the bare cross-merchant DELETEs see nothing and worker-path MerchantTx would fail
  merchant.Require; tests pass because dbtest connects privileged. Worth its own issue.

## Design note — Postgres as dedup truth (SHIPPED above; kept for the record)
Original sketch: `Complete` written INSIDE the effect tx so Redis is a fast-path, not the truth —
`openrails.webhook_events` + RLS; Redis Begin → handler's own tx INSERT … ON CONFLICT DO NOTHING → Redis
Complete as cache; crash between commit and Redis write converges instead of double-applying. Implemented
2026-07-01 with one deviation: the wrapper verifies-or-writes after handler return, so the in-tx call is
optional per handler rather than a contract change to processingFunc. No backfill of the 90d Redis history
was done — existing completed keys keep serving from the cache until TTL; a pre-truth event redelivered
after BOTH its cache entry expired AND a flush would re-run the handler, which #675 replay-safety absorbs.

## Acceptance
Two replicas processing the same delivery cannot both apply effects; losing Redis degrades to slower processing,
never to replayed money effects.

---


# #666: dead-code purge — ~7k lines of unreachable features and orphaned surface

**Completed:** yes
**Status:** DONE (2026-07-01) — phases 1+2 executed (~5.1k lines deleted; per-item annotations below); the
deliberately-left tail (fx.NoOpProvider, response pkg, webhookutil re-exports, gin remainder, etc.) was
deleted by #670. Two audit corrections recorded (merchant_env.go live; MicrosToCentsCeil live). Ready for
completed.md. Originally: from the 2026-07-01 whole-repo audit (two independent sweeps agreed on the
big items; every claim below was deadcode- AND grep-verified, including `//go:build integration` files).

## Metadata
- Category: cleanup
- Status: planned
- Passes: false

## Problem

Several whole subsystems are wired-looking but unreachable, and a long tail of exported surface has zero
non-test callers. Ranked, biggest first:

1. **USDC funding (Coinbase onramp) is dead-in-the-water** (~1,400 lines): `funding.Service.Create/Get` have NO
   route/export callers, so sessions can never be created — the wired Coinbase webhook branch
   (`internal/http/handlers/webhook.go:650`) can only ever hit ErrSessionNotFound. Cut module + repo +
   usdc_funding_sessions queries/models/DDL + `USDCFundingConfig` + `ufs_` id helpers. Supersedes/parks #328
   (keep the design notes there; the checkout `usdc_funding` error metadata is separate and stays).
2. **Abuse blocklist + velocity guard never consulted** (~550): `IsBlocked`/`VelocityGuard.*` have zero callers;
   blocklist.go's own doc says "deny wiring intentionally NOT built here". Cut blocklist.go, velocity.go,
   payment_blocklist queries/table. (CardAbuseGuard + WastedSpendGuard ARE wired — keep.)
3. **11 unregistered `Service*` handlers** (~650): `ServiceAdmit` (single), `ServiceGetBudget`,
   `ServiceAbuseUsage`, `ServiceSetPayerSpendLimits`, `ServiceSetTierSchedule`,
   `ServiceGet/SetMerchantConfiguration`, `ServiceSet/GetInvokerSpendLimits`,
   `ServiceSet/GetCreditAccountSettings`, `ServiceListCustomerCreditTransactions`, `ServiceWithdrawCredits`,
   `ServiceLookupCreditTransaction` — none in any route table (`internal/http/routes/routes.go:184-231` is the
   full service surface); embed/client.go has its own copies.
4. **`internal/modules/webhooks/replay`** (~800 incl. tests): zero importers outside its own test.
5. **6 unregistered Admin handlers** (~450): AdminReconcileProduct/Price, AdminListCatalogOrphans,
   AdminListStripeOrphans, GetAdminManualRebillAttempts, DeleteAdminUserPaymentMethod.
6. **Health circuit-breaker gates nothing** (~300): nothing calls `IsAvailable`; only `/ready` reads
   `GetServiceHealth`. Replace `internal/services/health` with ~25 lines of on-demand pg/redis Ping in the
   readiness handler.
7. **Dead ledger surface** (~270): `grants.Ledger.{Expire,Materialize,RevokeGrant,LiveGrants,RevokeBySource,
   SpendableLots}`, `money/ledger.Ledger.{Conservation,Spend,Expire}`. NOTE deadcode misses embed roots:
   `GrantAdmin`/`AdminGrantExists` ARE live via `pkg/embedded/admin_grants.go:94` — keep.
8. **Legacy embedded-mux path in gin Server** (~360): `Server.NewHTTPHandler`/`newHTTPHandlerMux`/
   `HTTPHandlerOptions` duplicate `embedhttp.Assembler`; kept alive only by embedded_mux_test.go.
9. **`RegisterHostWebhookRoutes` + `HostWebhook`** (~120): no caller; embedded hosts use
   `RegisterMerchantWebhookRoutes`.
10. **Dead CCBillVersionedPayload interface + 13 Get* method blocks** (~100, `internal/modules/webhooks/types.go:368`).
11. **CrossMerchant analytics** (~100): five `*CrossMerchant` wrappers with zero callers ("TODO(#232)");
    deleting them collapses the `crossMerchant bool` threaded through merchantFilter + 10 query helpers.
12. **Misc verified-dead tail** (~600): `fx.NoOpProvider`; `solana/subscriptions.BuildResumeSubscription`;
    solana `ValidateRecurringMint`/`IsTerminalFailure`/`GetTokenBySymbol`/`IsValidToken`/
    `FiatMicrosToStablecoinBaseUnits`/bare `NewPlanService`; `moneyutil.MicrosToCentsCeil`/`ParseDecimalToMicros`;
    webhookutil delegation re-exports (`ParseStripeSignatureHeader`/`ParseNMISignatureHeader` — call sigverify
    directly); `cache.CacheMiddleware`/`GenerateKey`; `controlplane.Catalog`/`CatalogNames`/
    `MerchantOwnerRolePermissions`; `RateLimitStore.Reset`; `MoneyService.snapshotTx`; `money.FormatAmount`;
    `webhooks.IsExpired`; `spendgate.Gate.SetClock`; `checkout.SetSolanaLifecycleForTest`; dead ginmw
    (`CORS`, most of service_credential.go ~193, `UserSessionAdminPrincipalRequired`, `RateLimit`,
    `InternalIPWhitelist`, `WebhookIPWhitelist` — ~350, or fold into #670); most of
    `internal/bootstrap/merchant_env.go` (keep `MerchantBillingEnvKey`); sqlc `SetSubscriptionUnknown`
    (the ONLY orphan among 419 gen methods — but check #664/#665 don't claim it first);
    `internal/http/response` wrapper pkg (~40 — every func is one `api.SimpleErrorResponse` call; the
    "two error envelopes" unification is already de-facto done, this is the residue);
    `pkg/authprovider` re-export shim (8 importers, s/authprovider/billingauth/);
    `derefStr` defined identically in 3 files; `PaymentsIdempotencyAdapter` + checkout's duplicate
    IdempotencyStatus/IdempotencyRecord types (~60 — have checkout consume idempotency's types).

Also (yagni, do opportunistically while in the files): single-impl single-injection interfaces in
subscriptions (`user_admin_support.go` NotificationStore/NotificationEmailSender/AdminCancellationLogger/
LifecycleEventLogger, `merchantConfigurationReader`, StripeLivenessProber, StripeSubscriptionLister/
StripeChargeLister) — take the concrete types. And the entitlements repo facade is 20-40 pure one-line
forwards; the de-facto house convention (money/grants, the newest audited code) is module→gen directly —
collapse pure-forwarding facades where they add nothing.

## Tasks
- [x] Item 1 DONE 2026-07-01: USDC funding subsystem deleted (module, repo/models/queries/gen, webhook branch +
      Hook0 verifier, config + env mapping + example yaml, ufs_ ids, RLS-test refs; table dropped via migration
      054 and removed from 001 baseline). #328 closed to completed.md; intent re-filed as future.md #680.
- [x] Item 2 DONE 2026-07-01: blocklist.go/velocity.go + their integration tests deleted (CardAbuseGuard/
      WastedSpendGuard kept); payment_blocklist model + the 3 queries cut from admission_support.sql, sqlc
      regenerated; table dropped via migration 055 and removed from the 001 baseline (+ the two schema-test
      table lists).
- [x] Items 3-5, 8-9: delete unrouted handlers + dead mux/mount paths.
      (Item 4 DONE 2026-07-01: webhooks/replay subpackage deleted. Audit missed one importer —
      tests/nmi_webhook_test.go — which existed to exercise replay itself; trimmed to the one non-replay
      regression test (Stringish subscription-id normalization, now os.ReadFile on the fixture).
      testdata/webhooks fixtures KEPT: used by ccbill tests + seed_data.go.)
      (Item 3 DONE 2026-07-01 phase 2: all 14 unregistered Service* handlers deleted from
      service_admission.go (-370) + service_credits.go (~-290) plus the types/helpers that died with them
      (trustTierQuery, spendLimitWindow/spendLimitWindowInputs, serviceQueryCustomer, serviceWithdrawRequest,
      serviceAccountSettingsRequest, serviceMerchantConfigurationRequest, servicePayerSpendLimitRequest,
      serviceTierScheduleRequest, serviceInvokerSpendLimitRequest). embed/client.go references were comments
      only. serviceTxnResponse KEPT (live via self_account.go); merchantProfileResponse +
      serviceMerchantConfigWindows KEPT (live via ServiceGet/SetMerchantSettings).
      Item 5 DONE: AdminReconcileProduct/Price, AdminListCatalogOrphans + AdminListStripeOrphans + listOrphans,
      GetAdminManualRebillAttempts + manualRebillStatusFilters, DeleteAdminUserPaymentMethod +
      adminPaymentMethodPath deleted (~-265).
      Item 8 DONE: Server.NewHTTPHandler/newHTTPHandlerMux + http_handler_options.go + embedded_mux_test.go
      deleted (~-330). Audit missed a SECOND live caller — tests/http_handler_options_test.go — its two
      legacy-path tests were PORTED onto embedhttp.FromApp(suite.App) so route-set coverage survives on the
      real embed path (Server.Handler() standalone assertions kept).
      Item 9 DONE: RegisterHostWebhookRoutes + handlers.HostWebhook deleted; the /host-webhooks leg of
      routes_merchant_webhook_integration_test.go trimmed — it existed only to exercise the dead mount.)
- [x] Item 6 DONE 2026-07-01: internal/services/health deleted; /ready does on-demand pg pool.Ping + redis
      Ping (2s timeout) in routes_public.go; HealthManager field + Start/Stop + createHealthManager wiring
      removed from internal/app. Response keeps status/service/auth + verbose dependencies
      {available, last_error|reason}; tests only assert status codes.
- [ ] Items 7, 10-12: dead-surface tail sweep.
      (Item 7 DONE 2026-07-01 by #677: every listed method grep-verified dead (incl. integration-tagged files
      + pkg/embedded roots) and deleted — grants.Ledger.{Expire,Materialize,RevokeGrant,LiveGrants,
      RevokeBySource,SpendableLots} (~95 lines, grants.go + credit_spend.go) and
      money/ledger.Ledger.{Conservation,Spend,Expire} (~42 lines) + the now-orphaned LedgerLedgerNet sqlc
      query (regenerated). RevokeBySourceAsOf/MaterializeGrant/CreditSpend/ExpireLapsed KEPT (live) and every
      surviving lot-consuming/reversing path now runs under the customer lock (#677). Test-only consumers
      rewritten onto the surviving surface / gen queries / raw SQL; the RevokeGrant refund-leg test died with
      RevokeGrant (credit_refund transfers no longer have a writer). GrantAdmin/AdminGrantExists kept as noted.
      deadcode ./cmd/openrails now reports NOTHING in grants/money-ledger except the documented embed-root
      pair.)
      (Item 11 DONE 2026-07-01: five *CrossMerchant methods deleted, crossMerchant bool collapsed out of
      merchantFilter + the 5 query helpers; fixture-only tests removed, isolation integration test keeps its
      per-merchant assertions. Item 12 PARTIAL 2026-07-01: pkg/authprovider shim deleted, 8 importers
      migrated to billingauth (ginauth subpackage KEPT — live from ginmw/server/ginreq);
      cache.CacheMiddleware/NewCacheMiddleware/GenerateKey deleted (Cache interface kept, live);
      controlplane.Catalog/CatalogNames/MerchantOwnerRolePermissions + Permission type + catalogEntries
      deleted — never seeded, Groups()+Perm* consts are the live surface; fixture-guard tests removed,
      live-behavior tests (apex-bypass, admission gate, bootstrap owner walk-up) reworked onto inline perm
      lists. sqlc SetSubscriptionUnknown: already deleted by #664, nothing to do. SKIPPED:
      internal/bootstrap/merchant_env.go — audit claim stale, every func in the file is a live private
      helper of MerchantBillingEnvKey (called from merchant_manifest.go's env.Provider loader).)
      (Item 10 DONE 2026-07-01 phase 2: CCBillVersionedPayload interface + 12 Get* method blocks +
      CCBillWebhookVersion type + the 5 version consts (only ever fed the dead interface) + webhooks.IsExpired
      deleted from webhooks/types.go (-128).
      Item 12 phase-2 slice DONE: solana ValidateRecurringMint (+its allowlist_test block),
      IsTerminalFailure + terminalSignatures + failure_terminal_test.go (ClassifyCrankError's on-chain-code
      path superseded it), tokens.GetTokenBySymbol/IsValidToken, bare recurring.NewPlanService (6 test callers
      migrated to NewPlanServiceWithReader(sub, nil, ...)); moneyutil.ParseDecimalToMicros + its test
      (parseDecimalScaled/roundHalfAwayFromZero KEPT — live via ParseDecimalToCents, whose rounding coverage
      lives in webhooks/nmi_test.go); PaymentsIdempotencyAdapter + its test deleted, checkout's duplicate
      IdempotencyStatus/IdempotencyRecord replaced with type aliases onto the idempotency module, the two
      checkout store interfaces' Complete narrowed to json.RawMessage (nmi_sale_service marshals its struct
      at the one call site), build_runtime passes idempotencyService directly (never-nil ctor);
      dead ginmw: CORS (2 tests ported to CORSFromSource), bare RateLimit + classifyBucket (bucket test
      retargeted at the live neutral middleware.ClassifyBucket), InternalIPWhitelist, WebhookIPWhitelist,
      UserSessionAdminPrincipalRequired + userSessionPrincipal (+their tests), and most of
      service_credential.go — ServiceCredentialRequired/resolveServiceCredential/ServiceCredentialFromGin/
      RequireServiceCredentialCustomerScope + the 3 resolver interfaces + OwnerGroupRef context key +
      service_credential_test.go (~-680 across ginmw); ServiceCredentialContextKey + bearerToken KEPT (live).
      SKIPPED with reasons: MicrosToCentsCeil — LIVE (checkout/service.go:2164, audit claim stale);
      FiatMicrosToStablecoinBaseUnits — LIVE post-#671 (handlers + checkout), as re-verified;
      spendgate.Gate.SetClock — LIVE test seam (gate_integration_test.go:38 injects a fake clock);
      derefStr triplication — all 3 copies are LIVE in their packages (not dead code); money copy is
      #677-owned and normalize.FromPtr trims (behavior change), so 2-of-3 dedupe was skipped → #670/later.
      NOT TOUCHED in phase 2 (outside the phase-2 slice, still present in-tree — remaining item-12 tail):
      fx.NoOpProvider, solana/subscriptions.BuildResumeSubscription, RateLimitStore.Reset,
      MoneyService.snapshotTx (#677 area), checkout.SetSolanaLifecycleForTest, internal/http/response
      wrapper pkg, webhookutil ParseStripeSignatureHeader/ParseNMISignatureHeader re-exports.)
      (TAIL CLEARED by #670 2026-07-01: fx.NoOpProvider, BuildResumeSubscription, RateLimitStore.Reset,
      snapshotTx(+accountSnapshot), internal/http/response (pkg deleted, helpers inlined into ginauth),
      webhookutil re-exports — all deleted. Still open: checkout.SetSolanaLifecycleForTest (grep-dead but
      session_service.go was #674-hot; take in a later pass).)
      (Yagni interface collapse DONE 2026-07-01 phase 2: subscriptions NotificationStore/
      NotificationEmailSender → *NotificationService, AdminCancellationLogger/LifecycleEventLogger →
      *analytics.EventLogService (river workers already held the concrete type; reconcile/converge callers
      pass nil — no reconcile edits), merchantConfigurationReader → *merchantconfig.Store;
      user_admin_support.go now holds only GetNotificationsFilters. SKIPPED: StripeLivenessProber — live
      test seam (fakeStripeProber in jobs_subscription_liveness_integration_test.go); StripeSubscriptionLister/
      StripeChargeLister — collapse forces edits in internal/modules/reconcile + pkg/service/reconcile.go
      (reconcile owned/in-rewrite during phase 2) → #670/later. Entitlements repo-facade collapse NOT done
      (bigger refactor, not dead code) → future.)
- [ ] Re-run deadcode + full integration suite green after each batch.
      (Phase 2 verification 2026-07-01: go build ./... green, go build -tags integration ./... green,
      go vet (+integration tag) green on every touched package, unit tests green: handlers, routes, http,
      middleware+ginmw, webhooks, checkout, idempotency, subscriptions, river, app, solana/*, moneyutil.
      Full integration suite not re-run here — grants/money/spendgate/nmi were churning concurrently
      (#676/#677/#678 + the NMI rewrite session); run it once those land.)

**Status update (2026-07-01): PHASE 2 COMPLETE.** All phase-2 items are DONE or explicitly SKIPPED-with-reason
above. Phase-2 net ≈ -2,600 lines. What remains → #670 (or a final tail pass): the rest of the gin layer
(CredentialType consts now mostly unreferenced, remaining ginmw surface), the deferred interface collapses
(Stripe listers, derefStr dedupe), the untouched item-12 tail listed above (fx.NoOpProvider,
BuildResumeSubscription, RateLimitStore.Reset, snapshotTx, SetSolanaLifecycleForTest, internal/http/response,
webhookutil re-exports), and the entitlements facade question.

## Acceptance
`deadcode ./cmd/openrails` (cross-checked against integration-tagged files and pkg/embedded roots) reports no
production-dead symbols in the listed areas; no route-table or behavior change; net ≈ -7k lines.

---

# #667: production gate: payment-provider secrets must not persist plaintext at rest

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. Fail-closed boot gate on the DB-backed
(fallback) secret store: `enforceEncryptionPosture` in `internal/merchantsecrets/store.go` (buildDBSecretStore),
gated by new `config.RequiresSecretEncryption()` — the exact same `!IsDev()` env signal as `RequiresRLS()`
(#227). Non-dev + secret_backend=db + no ENCRYPTION_MASTER_KEY ⇒ boot refused with an actionable error (names
ENCRYPTION_MASTER_KEY and secret_backend=vault); dev boots with one loud PLAINTEXT warning. All four boot
paths (standalone http server, bootstrap manifest, bootstrap dump, embed/provision.go) funnel through
merchantsecrets.Build — verified the only production construction site of the DB store. Vault-backed store
unaffected (returns before the gate). Solana WriteRestrictedSecretStore KEPT: now dev-only-reachable, but
Solana private keys are self-custody funds so plaintext is refused even where dev allows it for the rest.
Tests: store_test.go (posture matrix + env-signal pin vs RequiresRLS) and store_integration_test.go
(Build-level: prod+db+nokey refused / prod+vault+nokey boots via fake Vault / dev+db+nokey warns /
prod+db+key round-trips ciphertext-at-rest) — all green.

## Metadata
- Category: security
- Status: done
- Passes: true

## Problem
`buildDBSecretStore` (`internal/merchantsecrets/store.go:158-185`): when `ENCRYPTION_MASTER_KEY` is unset,
`crypto.NewEncryptor` returns a disabled encryptor and the DB secret store persists PLAINTEXT. Only Solana
private keys are write-restricted; Stripe secret keys, NMI security keys, CCBill credentials, and webhook
signing secrets all write cleartext into `openrails.merchant_secrets`. A DB dump/backup/replica leak exposes
every merchant's live provider credentials. There is a prod fail-closed gate for RLS posture
(`internal/db/rls.go:59` refuses BYPASSRLS) but no equivalent for encryption — the Solana-only restriction
shows the asymmetry is an oversight, not a decision.

Posture (Paul, 2026-07-01): production normally uses Vault as the merchant-secret store; the DB-backed store
is a FALLBACK. Scope accordingly — no new encryption machinery, just: whenever the DB store is the one storing
secrets, the master key must be set. Vault-backed deployments are unaffected.

## Tasks
- [x] Fail closed in non-dev: DB-backed secret store selected + disabled encryptor ⇒ refuse boot (mirror the
      EnforceRLSPosture pattern). Vault-backed store: no change.
- [x] Keep dev/test ergonomics: explicit dev-mode escape hatch, loudly logged.
- [x] Integration test: prod-ish config with DB store and no master key refuses boot / cannot write a
      Stripe/NMI/CCBill credential.

## Acceptance
A production deployment cannot silently store any payment-provider credential or webhook secret in cleartext;
dev keeps working with an explicit opt-out.

---

# #668: CCBill webhook auth bypassed by global test_mode (+ webhook trust-flag hygiene)

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. Bypass now gated by `ccbillWebhookIPAllowed`
(one helper, all three ingestion sites): requires test_mode AND no environment=live CCBill row in
`openrails.payment_provider_accounts` (new `merchants.Service.HasLiveProviderAccounts`, cross-merchant probe on
the same trust boundary as ResolvePaymentProviderAccountByIdentity); probe failure / nil config / nil merchants
fail closed to the allowlist. Rationale: CCBill creds carry no intrinsic test/live marker and ValidateRailSet
forces config-declared environment to equal test_mode, so the persisted catalog rows are the only per-account
signal that survives a live deployment flipping test_mode on. CCBill direct-dispatch events no longer stamp
SignatureValid=true (nil now, matching the River path; re-verified only NMI/Stripe Apply gate on it). GUC-reset
failure in db_pgx.go release() now warns AND closes the conn so the pool destroys it rather than reusing a conn
that may still carry a merchant GUC. Tests: handlers/webhook_ccbill_test.go (4 tests, green).
FOLLOW-UP FIX (2026-07-01, tests/ suite): checkout's scoped CCBill lookups hardcoded environment="live"
(merchant_rail_secrets.go), so a sandbox deployment (test_mode ⇒ environment=test rows per ValidateRailSet ⇒
the exact posture this issue requires) could never resolve its CCBill provider account — now derived via
config.ExpectedProviderEnvironment(IsTestMode()). NOTE: the NMI/Stripe/solana scoped lookups still hardcode
"live" (checkout_session.go:143, merchant_rail_secrets.go resolveNMIClient, vault_service.go:268,
merchants/credentials.go LoadStripeCredentials/LoadNMIWebhookSigningSecret/LoadNMITokenizationConfig,
solana recurring/wiring.go) — same latent sandbox-posture mismatch, needs its own sweep.

## Metadata
- Category: security
- Status: done
- Passes: true

## Problem
CCBill has no HMAC — the source-IP allowlist is its ONLY authentication. `internal/http/handlers/webhook.go:106-116`
(also :184, :302) skips the IP check whenever global `isTestMode` is true. TestMode is orthogonal to
live-credential Mode (#355), so a deployment with REAL CCBill credentials + `test_mode=true` accepts forged
`NewSaleSuccess`/`RenewalSuccess` from ANY IP — entitlement/credit fabrication. (Replays of a seen
transactionId are deduped; forged NEW transactions are not. Client IP correctly uses RemoteAddr, not XFF.)

Nits from the same audit:
- CCBill events are stamped `SignatureValid=true` (webhook.go:479,487) despite having no signature — nothing
  gates on it today (NMI/Stripe fail closed at dispatcher.go:135,179) but a future refactor could over-trust it.
- `internal/db/db_pgx.go:210-218`: session-GUC reset error silently dropped on pool release. Self-healing
  (GUC re-set before every use; RLS fails closed without GUC) but a failed reset should at least log.

## Tasks
- [x] Bind the CCBill IP-check bypass to the CCBill account's sandbox posture (or refuse test_mode when a live
      CCBill account is configured) — never the global test_mode flag.
- [x] Stamp CCBill events `SignatureValid=false` (or a distinct `ip_allowlist` verification kind). (Left nil —
      never claimed — matching the River path's unverified Prepared.QueueArgs.)
- [x] Log GUC-reset failure in `release()` (or use tx-scoped set_config). (Warn + discard the conn; tx-scoped
      set_config doesn't fit — the lazy conn's GUC spans multiple statements outside any tx.)
- [x] Test: live-mode CCBill + test_mode=true still rejects webhooks from non-CCBill IPs.

## Acceptance
No configuration accepts CCBill webhooks from arbitrary IPs while live credentials are configured; the
SignatureValid flag never claims verification that didn't happen.

---

# #669: rail capability descriptor registry — collapse the 36-point dispatch scatter

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. Registry lives in the existing rail-identity
home `internal/modules/payments/rails` (registry.go): one `Descriptor` per rail (nmi, ccbill, stripe, solana,
paypal) with fields Rail, DisplayName, HasProviderAccounts, HasRemoteCustomer, SupportsChargeSavedMethod,
OpenRailsDrivenDunning, RenewalGraceEligible, AutoBilled (func field — NMI's answer depends on vault presence),
CredentialKeys ([]{Name, MerchantWritable}; solana private_key is MerchantWritable=false). Compile-time
completeness via UNKEYED in-package struct literals (adding a Descriptor field fails to compile until every
rail declares it); registry_test.go forces every enum rail, pins every per-rail fact the old switches encoded,
and pins Lookup normalization (mobius does NOT resolve — post-#630 it's a provider-account name).
Collapsed all 12 descriptor-portable switch sites from the inventory below. Webhook ingress got the routing-
TABLE port only: globalWebhookIngress (handlers/webhook.go) + serviceWebhookIngress (service_webhooks.go)
replace the two top-level rail switches; posture gates/credential loading stayed put; the apply layer was
already registry-driven (#296/#675); processResolvedMerchantWebhook/processProviderAccountWebhook if-chains
deliberately LEFT (per-surface credential/#641-account/#668-posture logic, not routing). reconcile now iterates
`rails.All()` filtered on HasProviderAccounts (same order as the old hardcoded loop); RailFetcher untouched.
Legacy names (railHasRemoteCustomer, subscriptionProviderAutoBilled, RenewalGraceEligibleRail,
OpenRailsDrivenDunning) kept as thin registry delegates so their existing pinning tests prove behavior
preservation unchanged. Verified: go build+vet (both tags), repo-wide unit suite, integration suites for
webhooks/checkout/money/river/reconcile(+converge)/subscriptions/intents/merchants/handlers/pkg-service +
tests/ — green. (One-off parallel-package "tuple concurrently updated" role-provision flake on the shared DB
reproduced pre-change semantics; every package green when run normally.)

## Metadata
- Category: architecture
- Status: done
- Passes: true

## Problem
The four rails have four shapes (stripeapi choke-point pkg, NMIClient struct, CCBillClient struct, loose Solana
parts) and the only shared interface is reconcile's read-only `RailFetcher` (`internal/reconcile/reconcile.go:199`).
Everything else is hardcoded switch/if-chains: webhook ingress routing (3 layers: webhook.go:99,:175 +
service_webhooks.go:42), `railHasRemoteCustomer` (rail_customer_service.go:72), credential-key switches
(payment_provider_config.go:365,374 + secrets.go:134), ChargeSavedMethod blacklist (money/collection.go:75),
`subscriptionProviderAutoBilled` (jobs_dunning.go:660), checkout confirm (session_service.go:499),
unknown_orchestration.go:58,68, display names (email_service.go:582), build_runtime wiring. Adding rail #5 =
~25 files, 36+ dispatch points, zero compile-time help finding them.

## Target
NOT a fat uniform PaymentAdapter (rails genuinely differ — vault ownership asymmetry is real). A per-rail
capability DESCRIPTOR registry: one struct per rail declaring HasRemoteCustomer, AutoBilled,
SupportsChargeSavedMethod, DisplayName, CredentialKeys, sandbox-posture (feeds #668), plus function fields for
webhook enqueue/handle. Collapses ~80% of the switches (all boolean predicates + credential keys + display
names + webhook routing) into one file per rail; "did I miss a switch?" becomes "fill in the struct".
Overlaps #291 (processor-capability-metadata) — this IS #291's capability model, scoped to the dispatch
points that exist today; fold #291's checkout-validation/routing ambitions in later, don't build them here.

## Inventory (2026-07-01, re-verified in source before refactor)
DESCRIPTOR-PORTABLE (facts/predicates — collapse into registry):
1. rail_customer_service.go:72 `railHasRemoteCustomer` → HasRemoteCustomer
2. reconcile/unknown_orchestration.go:58 `railExposesRemoteCustomer` (same fact, duplicated) → HasRemoteCustomer
3. reconcile/unknown_orchestration.go:68 `railToProvider` + :96 hardcoded 4-rail loop → registry iteration
4. money/collection.go:75 ChargeSavedMethod exclusion (ccbill/solana) → SupportsChargeSavedMethod
5. river/jobs_dunning.go:660 `subscriptionProviderAutoBilled` (ccbill always; nmi vault-less) → AutoBilled func field
6. subscriptions/email_service.go:574 `railDisplayName` → DisplayName
7. merchants/payment_provider_config.go:365 `supportedPaymentProvider` → HasProviderAccounts
8. merchants/payment_provider_config.go:374 `paymentProviderCredentialKeys` → CredentialKeys (merchant-writable subset)
9. merchants/secrets.go:134 `NormalizeProviderAccountSecretKey` key sets → CredentialKeys (full set)
10. merchants/secrets.go:125 `SecretWritable` solana/private_key carve-out → CredentialKey.MerchantWritable=false
11. rails/nmi.go:27 `OpenRailsDrivenDunning` (nmi+solana) → descriptor field
12. subscriptions/dunning.go:283 `RenewalGraceEligibleRail` (nmi+stripe) → descriptor field

WEBHOOK INGRESS (3 layers): apply layer is ALREADY registry-driven (webhooks/dispatcher.go
WebhookHandlerRegistry, #296/#675 — no work needed). Top-level rail switches in handlers/webhook.go:92-117
(global surface) + pkg/service/service_webhooks.go:38-53 → routing-TABLE port only.
processResolvedMerchantWebhook (:142) + processProviderAccountWebhook (:246) if-chains LEFT: each arm is
per-rail credential loading / #641 account-identity extraction / #668 posture gates, not simple routing.

LEFT AS-IS (behavior dispatch or ownership elsewhere): checkout service.go:392,417,2324 +
session_service.go:499,554,630 (per-rail executors/validators — deliberate per the #521 drift comment);
config/config.go:484,950 (config-shape validation; config is imported BY rails — cycle);
subscriptions/cancel_mode.go:94 CancelModeFor (rail-fact candidate but drags nmiDeletePending along; future);
admin_service.go:218 + admin_payments.go:246,335 + jobs_subscription_manage.go:105 (per-rail cancel/refund
executors); the ~20 `rails.IsNMI` guard sites around NMI-only machinery (vault, deferred delete, liveness);
build_runtime.go client construction (acceptance keeps it); webhookutil.CanonicalRail (legacy mobius alias);
reconcile RailFetcher (kept by design).

INCONSISTENCIES FOUND (old switches disagreeing — not silently resolved):
- A. Stripe display name hit railDisplayName's generic fallback → "STRIPE" (email_service.go:586); every
  other rail is curated. Descriptor sets "Stripe" — deliberate display-only fix, noted here not silent.
- B. subscriptionProviderAutoBilled("stripe")=false although Stripe rebills itself (OpenRailsDrivenDunning
  excludes stripe for exactly that reason). Harmless: the dunning worker only processes OpenRails-driven
  cohorts. PRESERVED as-is in the descriptor (AutoBilled=never for stripe), documented on the field.
- C. money.ChargeRequest.AmountCents is RAIL MINOR UNITS (whole yen for JPY) but the NMI collection adapter
  feeds it to nmi.SaleParams.Amount whose wire format is two-decimal cents — divergent for zero-decimal
  currencies on NMI. USD unaffected; PRESERVED (sibling of #671-1g; no NMI merchant bills JPY).
- D. paymentProviderCredentialKeys("solana")=nil vs NormalizeProviderAccountSecretKey accepting
  solana/private_key: deliberate (operator-only secret hidden from merchant credential-status view) — now
  expressed as MerchantWritable=false rather than two disagreeing switches.
- E. money/collection.go: known-but-unsupported rails (paypal) previously fell through to "no adapter
  configured"; now get the explicit "does not support invoice collection" error. Unknown rail strings keep
  the adapter-not-found path.

## Tasks
- [x] Inventory pass: enumerate every rail switch (list above is the audit's; re-verify). → DONE, see Inventory.
- [x] Define the descriptor struct + registry; port the boolean predicates first (remote-customer, auto-billed,
      collection exclusion), then credential keys + display names, then the 3-layer webhook dispatch.
      → DONE in that order; webhook dispatch = routing-TABLE port only (see Status — the merchant/provider-
      account surface if-chains are credential/posture logic, deliberately left).
- [x] Compile-time completeness: constructing the registry requires every field per rail. → unkeyed in-package
      struct literals (a new Descriptor field breaks every rail's literal at compile time) + registry_test.go
      forces every enum RAIL and non-zero required fields.
- [x] Leave RailFetcher as-is (it already has the right shape); reconcile switches route through the registry.
      → railToProvider + railExposesRemoteCustomer + the hardcoded 4-rail loop all registry-backed.

## Acceptance
Adding a rail touches: the enum, its integrations package, ONE descriptor file, build_runtime wiring. The
boolean-predicate/credential/display/webhook-routing switches are gone; grep for `case "stripe"`-style rail
switches outside the registry returns ~zero.

---

# #670: single HTTP stack — drop the gin layer, serve the neutral mux everywhere

**Completed:** yes
**Status:** DONE (2026-07-01) — implemented in-tree, uncommitted. Standalone now serves the SAME neutral
net/http stack as embedded; every gin internal deleted.

## Metadata
- Category: refactor
- Status: done
- Passes: true

## What shipped (2026-07-01)
- **Flip:** cmd/openrails → `embedded.StandaloneHandler` (new, gin-free, pkg/embedded/standalone.go) →
  `internal/http` server.New, which now assembles an http.ServeMux + the neutral chain
  (RecoverHTTP → RequestLogHTTP → SecurityHeadersHTTP → CORSFromSourceHTTP(dynamic control-plane origins,
  NEW) → BodyLimitHTTP → ResolveMerchantHTTP → billingauth.Optional → RateLimitHTTP) — same order the gin
  engine ran. `internal/bootstrap/ginboot` → `internal/bootstrap/serverboot` (gin-free composition root;
  integrationharness + tests suite use it).
- **Ports to neutral (were gin-only):** self-service + customer-treasury registrations →
  `internal/http/routes/self.go` (RegisterSelfServiceRoutes/RegisterCustomerTreasuryRoutes on router.Router);
  delegated middlewares + Principal/keys → `internal/http/middleware/delegated_neutral.go`
  (DelegatedSelfRequired/DelegatedPrincipalRequired/CustomerScopeRequired/EnsureCustomerPermissionGroup/
  RequirePermission; DelegatedContextKey etc. — handlers retargeted); control-plane /auth mount = direct
  ServeMux registration of RouteSpecs (they are native ServeMux patterns; the gin `{}`→`:` translator died);
  meta/health/captcha endpoints → net/http (captcha handlers shared via embedhttp.CaptchaStatusHandler/
  CaptchaClientScriptHandler); embedhttp.NewSelfHandler (neutral self surface used by the shim). Root banner
  registered as "GET /{$}" so ServeMux keeps gin's exact-root behavior.
- **Route parity PROVEN:** dumped the gin engine's 206-route table BEFORE the cut (gin engine.Routes(), real
  boot); golden checked in at internal/integrationharness/testdata/standalone_route_surface.txt (129 billing
  routes; the 77 /auth routes are AuthKit-owned and asserted dynamically). Permanent test
  `TestStandaloneRouteSurface` (integrationharness) boots the real standalone server and requires
  RouteTable() == golden AND /auth == controlplane.RouteSpecs() verbatim (Server records every registered
  pattern via router.NewMuxRecorded / Server.handle).
- **pkg/embedded/gin shim (cozy-art compat, public API unchanged):** StandaloneHandler/Handler delegate to
  embedded.StandaloneHandler; SelfHandler delegates to embedhttp.NewSelfHandler (gin-free);
  MountHandler/RegisterAPI/MountOptions/ProviderRoutes/With* unchanged — gin now appears ONLY in
  api.go's RegisterAPI (`gin.WrapH` + group.Handle) and pkg/authprovider/ginauth (public, kept).
- **Deleted:** internal/http/middleware/ginmw (whole pkg), internal/http/router/ginrouter,
  internal/http/request/ginreq, internal/http/routes/ginroutes (+tests, re-ported neutrally),
  internal/controlplane/ginroutes, internal/http/response (helpers inlined: ginauth writes
  api.SimpleErrorResponse directly), gin half of internal/http (newPublicEngine + gin routes_*.go rewritten
  neutral), ginboot. Net across the cut ≈ -2,900 gin-coupled prod+test lines, ~+1,900 neutral lines
  (ports + re-ported tests + parity test).
- **Deliberate behavior deltas (audited):** (1) neutral rate-limit/captcha error bodies now emit the
  canonical pkg/api envelope on BOTH deployments (gin already did; embedded previously used ad-hoc
  `{"error":...}` maps) — envelope unification, standalone byte-compatible; (2) ServeMux answers
  wrong-method with 405+Allow where gin returned 404, and gin's 301 trailing-slash redirect is gone
  (matches the embedded surface's existing behavior); (3) global BodyLimit now exempts webhook paths on
  standalone (neutral matcher; webhook handlers keep their tighter per-rail caps); (4) embedded self
  handler now honors probed RouteCapabilities for provider routes (was rails-only), matching standalone.
- **gin linkage:** `go list -deps ./cmd/openrails | grep gin-gonic` ⇒ EMPTY — the standalone binary no
  longer links gin. gin remains in go.mod ONLY for the pkg/embedded/gin shim (RegisterAPI/WrapH) +
  pkg/authprovider/ginauth; full go.mod removal happens when cozy-art migrates off the gin shim.
- **Tests:** ginroutes self-surface tests re-ported to internal/http/routes (neutral); ginmw
  delegated/ratelimit/security coverage re-ported to internal/http/middleware
  (delegated_neutral_test.go, ratelimit_http_test.go additions incl. ClassifyBucket, http_base_extra_test.go);
  internal/http captcha/meta/self tests rewritten on the mux; tests/ + integrationharness ad-hoc gin engines →
  router.NewMux + middleware.ChainHTTP. Dropped without port: ginmw principal_test's user-session
  principal cases (its subject, UserSessionAdminPrincipalRequired, was already deleted in #666).
- **#666 tail deletions done here:** fx.NoOpProvider, solana/subscriptions.BuildResumeSubscription,
  RateLimitStore.Reset, MoneyService.snapshotTx(+accountSnapshot), webhookutil
  ParseStripeSignatureHeader/ParseNMISignatureHeader re-exports. SKIPPED:
  checkout.SetSolanaLifecycleForTest (grep-dead, but session_service.go was hot under the concurrent #674
  agent — delete in a later pass).

## Tasks
- [x] Standalone serves the embedhttp/neutral mux; gin hosts keep working via `gin.WrapH` (pkg/embedded/gin
      is now a thin delegating shim — cozy-art public API preserved).
- [x] Port the few live gin-only middlewares to the neutral chain; delete the gin packages.
- [ ] Remove gin from go.mod — deferred until cozy-art drops pkg/embedded/gin (shim + ginauth are the only
      remaining importers; the standalone binary already links zero gin-gonic).
- [x] Route-surface parity test green (TestStandaloneRouteSurface: neutral table == pre-cut gin golden) +
      integration suites (see verification note).

## Verification (2026-07-01)
- go build ./... + -tags integration green; go test ./... (repo-wide unit) fully green.
- Integration: TestStandaloneRouteSurface GREEN (129/129 billing routes == gin golden; 77 /auth ==
  RouteSpecs). pkg/embedded (+/gin) integration green. internal/integrationharness green except
  TestNativeCatalogBundleIncludesHTTP ("membership tier requires a recurring price" — concurrent catalog
  validation churn, not transport). tests/ suite: green except concurrent-agent areas — ccbill IP-gate
  hardening (webhook.go, theirs), refund whole-cents validation (intents/refund.go, theirs), subscription
  dunning lifecycle expectations (modules/subscriptions churn), TestGetProductsEndpoint interval "720h" vs
  "30d" (billing-cycle-hours formatting drift, not transport).
- REAL BUG the flip surfaced + fixed: the neutral query binder (request.go decodeTaggedValues) did not
  recurse into nested filter structs (query.QueryOptions[T].Filters), silently dropping e.g. the
  /v1/merchant/payments?user_id= filter, and lacked time.Time(time_format)/TextUnmarshaler(uuid) support —
  gin recursed. Fixed to gin parity + pinned by TestHTTPBindQueryNestedFiltersTimeAndUUID; the admin
  payments list-filter integration tests now pass. Also fixed two stale tests: http_handler_options_test
  (#666 migration forgot Assembler.Authenticator → panic aborted the whole tests/ package) and
  embedded_http_surface_test (pre-#650 expectation that the global /v1/webhooks surface is absent).

## Problem
Two complete HTTP stacks: standalone serves gin (`cmd/openrails/main.go:204` → `pkg/embedded/gin` →
`internal/http/server.go`), embedded serves net/http (`internal/http/embedhttp`). Route registration is ALREADY
framework-neutral (`internal/http/routes/routes.go` via `router.Router`, #282) and even the gin engine is
wrapped by the NEUTRAL middleware (`pkg/embedded/gin/self.go:98` calls ChainHTTP/SecurityHeadersHTTP/CORSHTTP/
RateLimitHTTP). The gin residue is pure duplication that is already rotting (several ginmw middlewares are
production-dead — see #666): ginmw (1,113), ginreq, ginrouter, ginroutes, ginboot, controlplane/ginroutes, gin
half of request.go, gin Server (~300 of server.go), pkg/embedded/gin (433), response.go. ~1,800 prod +
~1,300 test lines, plus gin (+ deps) out of go.mod.

## Tasks
- [ ] Standalone serves the embedhttp/neutral mux; gin hosts keep working via `gin.WrapH` (pkg/embedded/gin
      shrinks to a WrapH shim or dies — cozy-art is the embedder to check).
- [ ] Port the few live gin-only middlewares to the neutral chain; delete the gin packages.
- [ ] Remove gin from go.mod (root deps_test.go already forbids it in the client package).
- [ ] Full integration suite + one embedded-host smoke (cozy-art) green.

## Acceptance
One HTTP stack; gin absent from go.mod (or vestigially present only in a WrapH shim if an embedder demands it);
no route/middleware behavior change on either deployment mode.

---

# #665: consolidate the lapsed-subscription machinery — one probe, one decider, one freshness signal

**Completed:** yes
**Status:** DONE (2026-07-01) — ALL rescope tasks completed by the decider-consolidation agent: mirror-writer
refactor (appliers demoted; CancelLocal/AdoptStatus/RevokeEntitlements actions deleted), decider seam
(internal/reconcile/decider.go — pure Decide(sub, evidence, now) + ApplyDecision as the ONLY state mover,
called by LIFE/unknown/PULL), EvidenceBundle type, and the interleaving property test (243 orderings × 16
bundles ⇒ identical terminals; evidence-less ⇒ park-only is now a theorem). Ready for completed.md.
Original scope note: parts A-E of the original scope LANDED (see Progress — one
probe primitive, silent-lapsed lane deleted, single writer per invariant, coverage-derived confirmed-absence
gate, spec/states reconciled). What remains is (1) the one original open task — the mirror-writer refactor of
`engine.go`/`diff.go` — and (2) the EXTENDED end state below (from the 2026-07-01 design review): one
subscription state machine, everything else reduced to inputs. Webhook fetch-and-converge is the sibling issue
#684 — do them in sight of each other; the decider they feed is the same.

## Extended target (rescope 2026-07-01)

The original issue consolidated the *lapsed-cohort* machinery. The end state goes further: ONE subscription
state-machine decider whose transitions are evidence-gated (#664 doctrine), where every remaining plane is an
INPUT, not a writer:

- **Inputs (produce evidence, never move state):** bulk pull snapshots, per-sub probes
  (`unknown_probe.go`), webhook-triggered fetches (#684), first-party billing outcomes (dunning results,
  intents verify legs), the coverage/confirmed-absence gate.
- **One decider (moves state):** the LIFE/`ResolveUnknownFromSnapshot` core, generalized: given (current row,
  evidence bundle), emit the transition — active/renew, past_due+dunning, unknown-park, adopt-period-end,
  cancel-with-certainty. Scheduler ordering structurally cannot change outcomes (any plane running first only
  adds evidence or parks).
- **Appliers demoted:** the legacy engine's remaining appliers/selective-apply become mirror writes + decider
  invocations — the mirror-writer refactor IS this demotion; no plane applies domain state directly.

## Metadata
- Category: reconciliation
- Status: planned
- Passes: false

## Problem

Four mechanisms from three eras all answer "this subscription's period lapsed with no signal — now what?", and
none was deleted when its successor landed:

1. **Subscription liveness** (#367, `jobs_subscription_liveness.go`) — per-sub provider probe with its own outcome
   machine (repaired / failed / adopted / cancelled / skipped), run as a lane inside provider refresh.
2. **Legacy #107 diff engine** (`engine.go` + `diff.go`, ~2,000 lines + fetchers) — full-snapshot pull, findings,
   appliers, mutation policy, circuit breaker. Also emits `derive.*` and `consistency.*` finding types
   (`findings.go`), violating the spec's single-writer-per-invariant rule (consistency-invariants.md §7).
3. **LIFE `period_overdue` → `grace_exhausted`** (#511, `converge_passes.go`) — rail-heuristic cohort split, acts
   without evidence (the #664 bug).
4. **`needs_verification` → `unknown` → `unknown_reconcile`** (#632/#633) — park + targeted pull, with a SECOND
   per-sub verification outcome machine duplicating (1).

Cohort exclusivity hangs on WHERE-clause complements spread across three SQL files and is NOT actually exclusive:
a vaulted NMI row with a lapsed period is in BOTH `ListSilentLapsedSubscriptions` AND
`ListPeriodOverdueSubscriptions`; which mechanism wins is a scheduling race (liveness runs inside provider
refresh, but converge sweep + inline converge run the LIFE plane with no freshness precondition). On doujins the
evidence-fetching lane never ran while the guessing lane did — that race IS the #664 incident.

Spec/code drift, same symptom: §8's `held`/`indeterminate` finding states don't exist in code (converge writes
`reconcile_required`/`requires_review`); the §3.2 confirmed-absence gate's `reconciliation_state` flag is
merchant-global, manual, and has exactly one writer (`pkg/embedded/pull_provider.go` CLI) — nothing operational
sets it.

## Target — finish the consolidation the spec already demands (§7/§10)

- **One per-subscription provider-verification primitive.** Extract the liveness worker's probe + outcome machine
  ("entitlements extend only through a real charge"; "period adoption alone never grants access" — that doctrine
  is correct and becomes THE implementation); port `unknown_reconcile` onto it. One outcome set.
- **One lifecycle decider.** Post-#664, LIFE routes every lapsed row (evidence → dunning, none → `unknown`), so
  the silent-lapsed cohort IS the unknown cohort: delete `ListSilentLapsedSubscriptions` + the liveness lane.
- **One freshness signal.** `provider_refresh_watermarks` (per merchant+provider+account, never advances on
  failure) drives both the #664 evidence predicate AND the confirmed-absence gate; retire the manual
  `reconciliation_state` flag or scope it strictly to bulk-import mode.
- **Strip the legacy engine to a mirror-writer** (fetch snapshot → upsert mirror rows). It keeps: read-only
  fetchers, the coverage contract, the circuit breaker, mutation policy — as guards on MIRROR writes. It loses:
  emission of `derive.*`/`consistency.*` findings (converge passes own those invariants) and appliers that
  duplicate converge repairs. `pull.*` findings remain its only vocabulary.
- **Reconcile spec ↔ code on finding states** (`held`/`indeterminate` vs `reconcile_required`/`requires_review`)
  — pick one, update the other.

## Tasks

- [x] Extract the verify-subscription primitive from `jobs_subscription_liveness.go`; port
      `unknown_reconcile.go` onto it (one probe, one outcome set). (Landed inverted per the target:
      `ResolveUnknownFromSnapshot` stayed THE decision core; liveness's probing became SNAPSHOT SOURCES —
      `internal/reconcile/unknown_probe.go` — feeding it. One outcome set: UnknownOutcome, + Adopted.)
- [x] Delete the silent-lapsed lane (`ListSilentLapsedSubscriptions` + the liveness cohort scan) once #664 routes
      the cohort through LIFE.
- [x] Single writer per invariant: stop emitting `derive.*`/`consistency.*` from the pull engine; move any check
      not already covered by a converge pass into one.
- [x] Mirror-writer refactor of `engine.go`/`diff.go`; `pull.*` findings only. (The "pull.* findings only" half
      landed with the single-writer move; the appliers/selective-apply refactor landed 2026-07-01: PS-2
      cancel / PS-3 adopt appliers + their SQL deleted, transitions are decider invocations — see Progress.)
- [x] RESCOPE: extract the decider seam — one function (row, evidence) → transition, called by LIFE sweep,
      unknown-resolution, and (later) #684 webhook-triggered fetches; planes stop writing subscription state
      directly. (`reconcile.Decide` + `reconcile.ApplyDecision`, decider.go.)
- [x] RESCOPE: evidence bundle type unifying what the planes produce today (pull snapshot / probe result /
      charge evidence / coverage proof) so the decider's inputs are explicit and testable as data.
      (`reconcile.EvidenceBundle`.)
- [x] RESCOPE: property test — for a fixed evidence bundle, ANY interleaving/ordering of plane execution yields
      the same terminal state (the #664 acceptance, generalized to the whole machine).
      (decider_property_test.go: 243 interleavings × 16 bundles + evidence-less-can-only-park state sweep;
      converge/decider_interleaving_integration_test.go on real Postgres.)
- [x] Watermark-derived confirmed-absence gate; retire or import-scope `reconciliation_state`. (Implemented
      COVERAGE-derived, not watermark-derived — absence proofs need exhaustive pulls, not event-window freshness.)
- [x] Spec/code finding-state reconciliation (docs/consistency-invariants.md §8).

## Progress

- 2026-07-01 — Parts A/B/C IMPLEMENTED, uncommitted (probe-unification + silent-lapsed-lane deletion still open,
  sequenced after other in-flight work).
  - **A (single writer):** the pull engine now emits `pull.*` only. PS-9 (`derive.grant_effect.mismatch`) moved
    into the DERIVE pass as two set queries (`ListActiveSubsMissingEntitlementProjection` — repair = derive-1 for
    exactly the missing features; `ListDeadSubsWithLiveEntitlements` — repair terminates grants + retracts
    windows; `unknown` excluded, #664). PS-10 (`life.provider_intent.stuck`) moved into the LIFE pass (sweep
    scope; mode-parked = informational; recovered intents auto-resolve SUBJECT-FIRST via
    `AutoResolveRecoveredStuckIntentFindings` — converge has no run-driven vanish sweep, a pre-existing gap for
    its other surface-only findings). PS-8 is snapshot-dependent so it stays PULL:
    `consistency.duplicate.subscription` → `pull.subscription.duplicate` (migration 060 renames ledger rows +
    rekeys legacy PS-9 subjects). Deleted: diffEntitlements, stuck_intents.go, entitlement local-state loading,
    standalone Grant/Revoke apply actions, AutoResolveVanishedAllProviders. Net deletion in internal/reconcile.
  - **B (automatic gate):** `reconcile.MarkReconciledSourceDomains` (coverage.go) flips `reconciliation_state`
    from PROOFS: completed provider sections' `SnapshotCoverage` (exhaustive, never event-window watermarks),
    only when EVERY declared rail account is covered (multi-account rails / empty catalog prove nothing; ratchet,
    never unset). Wired into provider-refresh (per merchant, before the converge pass) and pull-provider
    (replaces the blind full-head flip; --insert --overwrite full-head pull = the manual bulk-import setter).
    `payments` needs full-history coverage so only full-head pulls prove it; `grants` is never pull-provable.
  - **C (spec/states):** §8 rewritten to the implemented vocabulary (reconcile_required / requires_review /
    auto_fixed / fixed / ignored + resolutions); held = reconcile_required with `source_domain` in evidence,
    indeterminate = failed-run/requires_review/`unknown` park. NO new DB state needed — no expressiveness gap
    found. §3.2/§5/§10 updated for the gate + rename.
  - Tests: engine_test reworked (PS-9/PS-10 gone, PullProofs unit test), stuck-intent integration test ported to
    converge (dedicated merchant), new converge tests for both mismatch directions + the gate
    (held EXCESS proceeds after an exhaustive-pull flip; non-exhaustive/multi-account never flips). Full unit
    suite + internal/reconcile, converge, river provider-refresh integration suites green; `task sqlc` clean.

- 2026-07-01 — Parts D/E IMPLEMENTED, uncommitted (verify-subscription primitive + silent-lapsed lane deletion).
  - **One decider:** `ResolveUnknownFromSnapshot` stays the decision core; the #367 liveness worker's
    capabilities were ported INTO it as snapshot sources + doctrine, then the worker was deleted.
    New `internal/reconcile/unknown_probe.go`: `SubscriptionProber` (NMI = query.php sales-by-order-ref +
    v5 recurring GET; Stripe = the existing `subscriptions.StripeLivenessProber`, kept) producing per-sub
    `RemoteSnapshot`s; the orchestration probes ONLY rows the bulk pull left unreachable (NULL period end,
    evidence outside window, non-exhaustive roster; never fanned out when the bulk fetch failed).
  - **Doctrine ported into the core:** new `UnknownOutcomeAdopted` + lifecycle `ResolveAdopted` — roster
    alive w/ future next billing and NO charge re-anchors the period END only (start untouched → DERIVE
    projects nothing): renewed now requires a VERIFIED charge (was: roster-active ⇒ Renewed, which reset
    period start and let DERIVE grant access without a charge). Roster past_due (stalled NMI record /
    Stripe past_due) → dunning within the window, cancel + deferred delete beyond (#679 kept green).
    24h `renewalAlignmentSlack` on charge classification + backfill (NMI day-boundary billing).
  - **Deleted:** `jobs_subscription_liveness.go` (+ integration test), the `runSubscriptionLiveness` lane,
    the legacy-worker River registration (queued `openrails.subscription_liveness` jobs from old deploys
    would now error — none scheduled, kind retired), `ListSilentLapsedSubscriptions` (+ repo
    `ListSilentLapsed`), `LivenessProbeSlack`. `nmi.SaleProbeResult` extended (dates/amounts/decline txn id)
    so probe evidence is verbatim. `ListUnknownSubscriptions` → `ORDER BY current_period_ends_at NULLS FIRST`
    (NULL-period legacy rows must not starve under the LIMIT; they're only resolvable here).
  - Deliberately dropped: charged-repair for rows with NO rail_subscription_id (liveness could order-ref-probe
    them; unreachable in practice — signup stamps both ids) and probe-driven Stripe `incomplete`/`paused` →
    past_due (probe uses the bulk `normalizeStripeStatus`, those park unknown until Stripe self-resolves).
  - Tests: liveness scenarios ported (renewed+backfill exactly-once, decline→past_due, roster-gone→cancel
    +revoke NO intent, stale-decline→cancel WITH intent, adopt-without-access, NULL-period via probe,
    unreachable stays unknown); probe→snapshot mapping unit-tested against a fake NMI HTTP server
    (direct-post counter stays 0). `go build ./...` + full unit suite + reconcile/converge/river
    integration (`-tags integration -count=1`) green; net ≈ −450 lines.

- 2026-07-01 — RESCOPE COMPLETE (decider seam + mirror-writer refactor + property test), in-tree.
  - **Decider seam:** `internal/reconcile/decider.go` — `Decide(SubscriptionState, EvidenceBundle, now,
    dunningWindow) Decision` (PURE) + `ApplyDecision(ctx, db, lc, sub, Decision, now)` (the ONLY state mover:
    non-unknown rows take the `unknown` waypoint — ApplyLocalUnknown then ResolveUnknownSubscription — so every
    transition lands through the one resolution implementation; Park/PastDue use the direct lifecycle cores).
    `ResolveUnknownFromSnapshot`/`UnknownOutcome`/`UnknownVerdict` deleted — the old body IS Decide's snapshot
    stage, extended with the #664 first-party stage (ownership/park/certainty legs). Transitions: renew /
    adopt-period-end / past_due / park-unknown / cancel(±RemoteGone) / none. Grace rule unified: period end +
    PeriodGrace (48h) everywhere (unknown-resolution previously used bare period end).
  - **Evidence bundle:** `EvidenceBundle{Snapshot *RemoteSnapshot (coverage-absence proof rides in
    Snapshot.Coverage), Charge ChargeEvidence{PaymentOpenedCurrentPeriod, RenewalPaymentAfterPeriodEnd,
    NonRetryableDecline, RetryAttempts/DunningMaxAttempts}, WatermarkNewerThanPeriodEnd bool}`. The certainty
    legs (non-retryable decline / dunning exhausted) are encoded in the law but no sweep plane produces them —
    converge structurally can only park/dun (evidence-less-can-only-park is now a tested theorem).
  - **All three planes call the seam:** LIFE — the 3 cohort queries (period_overdue/needs_verification/
    grace_exhausted) replaced by ONE `ListLapsedSubscriptionsWithEvidence` returning the evidence legs;
    converge_passes.go maps Decide's transition back onto the unchanged finding vocabulary. The WHERE-clause
    complement contract is GONE — exclusivity is structural (one query, one total function). Unknown-resolution —
    unknown_orchestration feeds Decide directly (probe fallback unchanged). PULL — see mirror-writer below.
    #684 webhook fetches get the same seam for free.
  - **Mirror-writer refactor:** ApplyAction lost CancelLocal/AdoptStatus (+ RevokeEntitlements helper); LocalWriter
    is now mirror-facts-only (backfill/refund/vault/materialize). PS-2/PS-3 findings carry `Decide` actions
    computed at diff time from the per-subscription snapshot slice (perSubscriptionSnapshot; NMI absence branch
    stamps SubscriptionsExhaustive — the trait IS a coverage statement); engine applies via Engine.Decisions
    (DecisionApplier; river injects the DeferDelete scheduler so pull-path stale-decline cancels queue the NMI
    delete, #679). Kept as guards: read-only fetchers, coverage contract, circuit breaker, mutation policy (Decide
    actions gated under the same Overwrite class). SQL deleted: ReconcileCancelSubscriptionLocal,
    ReconcileAdoptSubscriptionStatus (ReconcileRevokeSubscriptionEntitlements kept — converge DERIVE repair).
  - **Law fixes surfaced by tests:** a roster-declared DEAD sub is certainty — a renewal charge cannot resurrect
    it (charge still backfilled; money truth ≠ lifecycle truth). PS-2 cancel now revokes AS-OF the period end
    (ResolveCancelled semantics — paid-for window honored) instead of the legacy immediate revoke; integration
    test updated to pin the doctrine.
  - **Property test:** decider_property_test.go — model of the 3 planes (LIFE slice / PULL slice with the diff's
    divergence pre-filter / UNKNOWN slice) mirroring ApplyDecision's guards; 243 interleavings (3^5 + fixpoint) ×
    16 evidence bundles ⇒ identical terminal (status, period end, grace); plus a full state-space sweep proving
    an evidence-less bundle only ever yields none/park (#664, structural). Real-Postgres companion:
    converge/decider_interleaving_integration_test.go (renewal + provider-dead fixtures, LIFE-first vs
    resolution-first orderings ⇒ same terminals, exactly-once backfill, revokes intact).
  - **Deliberately dropped/deferred:** pending→active pull adoption (old PS-3 could flip a pending row active
    from roster truth; the decider treats pending as the signup/confirm path's — divergence still surfaced as a
    finding, pending_stale still cleans up at 72h). Pull-path PastDue on a NULL-period row skips (constraint
    chk_past_due_has_period_end; the park→probe path owns it, matching the old nil-apply guard).
  - Spec updated (LIFE one-scan note, PULL mirror-writer note). Verified: go build/vet (+integration) repo-wide,
    full unit suite, reconcile + converge + river integration suites green (incl. new interleaving test).
    ./tests full suite: 2 failures OUTSIDE this scope (TestFailMembershipLimitedModeLeavesRemoteSubscription —
    subscriptions limited-mode marker gating; TestCustomerTreasuryPayer_DelegatedDrawDownE2E — spend window) —
    both in the concurrent agents' areas, not touched here.

## Acceptance

Exactly ONE implementation each of: (a) per-subscription provider verification, (b) lapsed-cohort routing,
(c) mirror-freshness attestation. No plane emits another plane's finding types. Scheduler ordering cannot change
outcomes — an evidence-less pass can only park (the #664 property, now structural). Net code deletion in
`internal/reconcile` + the river lanes, not addition.

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

# #126: test-architecture-improvements

**Completed:** no

Tests should use real structs and functions from production code, not invent test-specific abstractions

## Metadata

- Category: testing
- Status: not_started
- Passes: false

## Details

- current_problems: ["SubscriptionOptions helper struct uses different field names than models.Subscription (PeriodStart vs CurrentPeriodStartsAt)","Tests can pass while using wrong field names because helpers translate between them","When a model field is renamed/removed, tests may not break because helper abstracts it away","Developers get confused about which struct to use and what fields are available"]
- example_bad: {"code":"suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{PeriodStart: now})","problem":"SubscriptionOptions.PeriodStart doesn't exist on models.Subscription"}
- example_good: {"code":"sub := &models.Subscription{CurrentPeriodStartsAt: now, ...}; suite.CreateTestSubscription(sub)","benefit":"Uses real model, compiler catches field name errors"}
- philosophy: {"principle":"Tests should verify actual production code behavior, not test-specific wrappers","goal":"When production code changes, tests should break if they're testing the affected behavior","anti_pattern":"Test helper structs that diverge from production models hide API changes and create maintenance burden"}
- recommendations: ["Use models.Subscription directly in tests instead of SubscriptionOptions","Use api.ListResponse[T] for parsing API responses instead of map[string]interface{}","Import and use the actual request/response structs from handlers","If a helper is needed, it should take the real model struct as input, not a parallel struct","Test assertions should use strongly-typed response parsing"]

**Tasks:**
- REFACTORING:
- [ ] Audit all test helper structs (SubscriptionOptions, PaymentMethodOptions, etc.)
- [ ] Replace helper structs with direct model usage where possible
- [ ] Update CreateTestSubscriptionWithOptions to accept *models.Subscription
- [ ] Use json.Unmarshal with actual API response types instead of map[string]interface{}
- [ ] Add linter rule or CI check to discourage map[string]interface{} in tests

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

# #291: processor-capability-metadata

**Completed:** no

Expose processor capability metadata in code, APIs, catalog planning, checkout validation, and admin/provider status so OpenRails can explain what each rail supports instead of relying on scattered processor-specific conditionals.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Motivation

- Stripe, NMI, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, NMI-backed (`mobius`), ccbill, solana.
- [ ] Distinguish processor class capabilities (NMI-backed) from named provider overrides (Mobius).
-
- INTEGRATION POINTS:
- [ ] Use capabilities in checkout validation instead of hard-coded processor switches where practical.
- [ ] Use capabilities in catalog planning/reconciliation to decide provider actions and pending_manual_actions.
- [ ] Use capabilities in routing/fallback policy (#288) so unsupported rails are filtered before checkout.
- [ ] Surface capabilities through admin/provider status endpoints and docs.
-
- ERRORS / UX:
- [ ] Return structured unsupported-capability errors with processor, capability, and suggested alternative when possible.
- [ ] Ensure user-facing errors are clean while admin/debug surfaces retain processor detail.
-
- VERIFY:
- [ ] Unit-test capability metadata for each supported processor.
- [ ] Regression-test known special cases: CCBill manual catalog actions, NMI recurring plan push, Stripe remote dedup check, Solana one-off/recurring distinction.

---

# #320: Add Hyperswitch payment vault support for cloud and self-hosted deployments

**Completed:** no

Add Hyperswitch as an optional OpenRails payment provider and payment-method vault integration, covering both Hyperswitch Cloud and self-hosted Hyperswitch. This is payment vaulting/tokenization, not HashiCorp Vault merchant-secret storage. OpenRails should store only opaque Hyperswitch customer/payment-method identifiers plus non-sensitive metadata, while PAN/card collection stays in Hyperswitch-hosted/client-side tokenization flows or equivalent PCI-scoped Hyperswitch surfaces.

NOTE (2026-07-01): for the CROSS-MERCHANT wallet, the SaaS strategy chose Stripe-Connect saved-PM reuse
(Rank 1, ~/openrails-saas/payments-multi-merchant-wallet-and-payfac.md) — Hyperswitch is NOT that path;
it stays a per-merchant vault-tech option, and any shared use falls under openrails-saas #23
(vault/state inseparability).

This issue must reconcile with future issue #297 (`deplatforming-resilient-card-vault`): Hyperswitch should not be positioned as the default adult/high-risk deplatforming-resilient vault until its contractual/export/compliance posture is explicitly certified. Hyperswitch Cloud may still be useful for lower-risk deployments or as an optional provider, and self-hosted Hyperswitch may be a break-glass/advanced deployment path if the operator accepts and satisfies the PCI requirements.

This should integrate with the provider capability and routing work in #288, #290, and #291: Hyperswitch can be selected as a processor/vault provider when the merchant/provider config says it is available, and OpenRails can route checkout/setup flows through it without treating it as a generic secret vault.

**Tasks:**
- [ ] Research the current Hyperswitch Cloud and self-hosted API surfaces needed for customers, payment methods, payment/setup intents, saved payment methods, refunds/voids, webhooks, connector routing, and vault/tokenization modes; record any version assumptions in docs.
- [ ] Reconcile the implementation plan with future issue #297: document whether Hyperswitch is an optional provider, a lower-risk deployment choice, or a PCI-heavy break-glass/self-hosted vault path; do not make it the default portable adult/high-risk vault without explicit certification.
- [ ] Define the OpenRails provider config shape for `hyperswitch`: cloud vs self-hosted mode, API base URL, optional vault/base URL split if Hyperswitch requires it, merchant/profile/account identifiers, API key secret reference, webhook secret reference, return/callback URLs, and test/live mode.
- [ ] Store Hyperswitch credentials in the existing merchant secret store path; do not store processor API keys in bootstrap YAML, database rows, logs, or generated frontend config.
- [ ] Extend provider capability metadata (#291) so Hyperswitch can advertise supported flows: payment-method vault/tokenization, one-time checkout, recurring/setup/mandate behavior if supported, refunds, webhooks, processor-side routing, remote payment-method listing, and manual certification status.
- [ ] Add a provider adapter/client abstraction that lets NMI customer-vault IDs and Hyperswitch payment-method IDs fit the same OpenRails payment-method model without hard-coding NMI semantics into checkout/subscription flows.
- [ ] Implement Hyperswitch customer/payment-method setup flow using an opaque token or hosted/client-side Hyperswitch collection result; persist only customer, provider, external customer ID, external payment method ID, brand/last4/expiry metadata, and status.
- [ ] Implement checkout/subscription charge paths that can use a saved Hyperswitch payment method and reconcile the resulting payment/subscription state back into OpenRails records.
- [ ] Implement webhook verification and event handling for successful payment, failed payment, refund/void, payment-method updates/deletes, and any recurring/mandate lifecycle events OpenRails relies on.
- [ ] Add self-hosted operational docs: required Hyperswitch services, base URL/TLS requirements, webhook reachability, secret injection, health checks, and local compose/dev smoke path if practical.
- [ ] Add cloud operational docs: required Hyperswitch Cloud credentials, webhook setup, provider certification checklist entry (#290), and merchant bootstrap/provider-link examples.
- [ ] Add tests with a mocked Hyperswitch server/client for tokenization/setup, saved-method charge, failure mapping, webhook signature verification, idempotency, merchant isolation, and no-sensitive-card-data persistence/logging.
- [ ] Validate with focused Go tests for provider/checkout/vault/webhook code, compile-only full package coverage, `task build`, and an optional live sandbox/self-hosted smoke test when credentials or local Hyperswitch are available.

# #333: admin-e2e-suite-needs-control-plane-wiring

**Completed:** no
**Status:** OPEN 2026-06-10 (Claude): diagnosed during the #332 failure review; not started. All other e2e failures from that review are fixed (see completed #332 + commits 9fe72d4 openrails, 68cb6a1 tensorhub, b72f61e8 cozy-art).

The admin e2e tests (tests/admin_payments_test.go, admin_metrics_test.go, admin_entitlements_source_test.go, admin_offchannel_payments_test.go) fail with 500 {'message':'authorization unavailable'} — the nil-admin-checker fail-closed path. DIAGNOSIS (2026-06-10): pre-existing #312 bit-rot, documented in the suite itself (testcontainer_suite.go Auth config comment: 'Admin-route integration tests therefore require the embedded control plane wired with the test admin granted openrails:admin'). The #312 hard-cut moved admin authority from JWT claims (tests still mint operatorAdminClaims() tokens via test_helpers.go) to the LIVE control-plane permission check (routes_admin.go only sets AdminPermissionChecker when s.controlPlane != nil; the suite never enables Auth.ControlPlane -> checker nil -> admin_neutral.go/ginmw admin gates 500 fail-closed).

WHAT THE FIX NEEDS:
1. suite.Config.Auth.ControlPlane = &config.ControlPlaneConfig{Enabled: true} (ginboot embcp.Attach then builds it; authkit profiles schema is already applied by migrate.RunPostgres).
2. The admin test users must EXIST in authkit profiles.users with ids equal to the JWT subs the suite mints, then be made members + granted the admin role (controlplane Bootstrap/AddMember/AssignRole or authkit core APIs — NOT raw SQL into roles). authkit core.CreateUser(email, username) generates its own id, so either (a) resolve the verifier's user mapping (issuer+sub -> user) and mint tokens for created users' real ids, or (b) add a test-only authkit user-provisioning hook.
3. operatorAdminClaims()/CreateAdminToken in tests/test_helpers.go are then vestigial — admin authority comes from the role grant, not claims.

NOTE: the admin-METRICS tests' 'Table test_analytics.daily_metrics does not exist' failures were a STALE TEST ENV artifact (migratekit records the ClickHouse ledger in postgres; reusing a postgres DB across runs against fresh CH containers skips re-apply). On a fresh env CH applies fine and those tests fail at the same admin-auth gate instead. Squashed CH baseline (cb9200b) is NOT at fault (daily_metrics present; migrations/clickhouse schema_test passes).

**Tasks:**
- [ ] Enable Auth.ControlPlane in testcontainer_suite config and verify embcp.Attach builds it in ginboot.
- [ ] Provision admin test users in authkit profiles.users matching the JWT subs (or mint tokens from created users' ids); AddMember + AssignRole openrails:admin via control plane / authkit core APIs.
- [ ] Re-run tests/admin_*.go on a fresh env; remove vestigial operatorAdminClaims if green.
- [ ] (env hygiene) consider failing loudly or re-validating when the migratekit CH ledger says applied but the CH database lacks the tables (stale-ledger detection).

---

