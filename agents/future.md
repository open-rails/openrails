<!-- openrails issue tracker — PLANNED/future issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


---

# #693: operator findings dashboard — triage UI over the #692 queue API

**Completed:** no
**Status:** FUTURE (2026-07-01, Paul) — the frontend for the #692 operator findings queue. Deliberately parked:
the #692 backend (queue API, structured recommendations, approve/ignore, self-verifying resolution) ships first;
this issue is the UI plan to refine when frontend work begins. Doujins/hentai0 embed openrails, so this likely
lives in the HOST admin panel consuming the /admin/findings API — decide host-side vs openrails-served when
picked up.

## Metadata
- Category: admin / frontend
- Status: future
- Passes: false

## Dashboard sketch

**Header — the three gauges (#690), one glance = system health:**
`duplicate_coverage` (critical, red when > 0) · `freeloaders` (high, amber when > 0) · `verification_pressure`
(count + max-age; green while young, amber when age trends up). All zero/young → green bar: "billing state
machine healthy". Gauges come free in the #692 list response.

**Queue view — the work list:**
- Open findings sorted severity desc, then age desc; badge per severity; row shows finding_type, subject (user
  + product where applicable), age, one-line recommendation.
- Filters: severity, finding_type, rail. Counts per filter chip.
- One-at-a-time triage flow: selecting a row opens the detail pane; j/k or arrow navigation for queue grinding.

**Detail pane — everything needed to decide, nothing hidden:**
- The recommendation sentence up top (from `recommended_action`).
- Evidence rendered PER FINDING TYPE, not raw JSON: a duplicate shows the user + both subscriptions side by side
  (created date, price, rail, paid-through, last payment) with the to-cancel one pre-highlighted but SWAPPABLE
  (the #692 override_params); a freeloader shows the window, its claimed source, and why the chain is broken;
  held_bulk shows the volume vs budget and the affected intent list.
- Actions: **Approve** (primary; confirmation modal restates EXACTLY what will execute — "cancel sub B via
  provider intent + refund payment P ($X)"; disabled when the finding has no structured recommendation) and
  **Ignore** (requires a note; warns it permanently silences this subject).
- Post-approve state: "executed — awaiting verification" until the next sweep auto-confirms (finding vanishes)
  or the finding persists (surface "fix did not take" loudly). The UI never marks anything green on its own —
  it reflects re-measurement (#692 step 6).

**History tab:** resolved findings (fixed / ignored / auto_vanished) with resolver identity (`resolved_by`),
notes, timestamps — the audit trail as a browsable log.

## Open decisions (when picked up)

- Host-panel integration (doujins admin already exists; hentai0 = doujins billing merchant) vs openrails-served
  minimal UI — leaning host-panel consuming the API.
- Auth: host admin session → /admin/findings gate (embedded pattern) — confirm against the #692 wiring.
- Notification hook (email/webhook when a critical finding opens) — separate small issue if wanted; the gauges
  are pull-based today.

Acceptance (to refine): an operator can triage the entire queue one item at a time from a browser — see health
at a glance, understand each finding without reading JSON, approve with an exact preview or ignore with a note,
and watch resolutions verify themselves.

---

# #657: per-user archived provider-account cutover on card re-entry

**Completed:** no — future follow-up to #655.

When a subscriber on an archived provider account reaches a natural lapse point
(expiry, failed rebill, cancelled/re-subscribe, or explicit card update), move
that single subscriber to the active account on the same rail/environment. This
is the normal passive-drain cutover, not the emergency migration campaign in
#656.

## Tasks

- [ ] Detect the existing subscription/payment method's archived
      `provider_account_id` at card re-entry time.
- [ ] Capture/tokenize the replacement card on a non-archived provider account.
- [ ] Create the replacement provider subscription anchored to the current
      period end when preserving paid-through access.
- [ ] Cancel or mark the old provider subscription according to the old rail's
      capability.
- [ ] Re-point the OpenRails subscription/payment-method rows to the new
      `provider_account_id` only after the new provider obligation is confirmed.
- [ ] Add idempotency and rollback behavior so retries do not double-subscribe.

Acceptance: one subscriber can migrate from archived A to active B by re-entering
their card, with no bulk import, no silent card copy, and no proactive mass move.

---

# #656: emergency payment-provider migration (A→B in weeks, not months)

**Completed:** no — future direction only. Not important now; plan when a real termination risk is imminent.

The DISASTER version of #655. Normal case (#655): a merchant chooses to switch processors and the old account
`archived` drains gradually over months–years as cards naturally lapse. This issue is the opposite: **provider A
suddenly TERMINATES the merchant** (common for high-risk/adult — doujins/hentai0), AND A owns the vaulted cards +
recurring logic. There is no time to drain — you must move the active book A→B in **≤1 month**, or lose it.

## The hard constraint (unchanged)

Same ceiling as [[provider-account-migration-vault-ownership]]: OpenRails never holds the card, and a terminated
provider will NOT release its vault. So emergency migration is NOT a silent move — it is a **mass, proactive,
time-boxed re-tokenization campaign**: get every active subscriber to re-enter their card on B before A goes dark
or their next bill misses. Everything else is about doing that fast and losing as few subscribers as possible.

## Target model

- **Warm standby is the #1 accelerator.** The whole thing is far faster if B is already provisioned and ready
  BEFORE the emergency. Recommend keeping a backup processor account warm (identity declared, secrets loaded,
  catalog synced) so "flip B active + archive A" is instant. Emergency-migration speed is mostly a
  preparedness problem.
- **Operator-declared emergency mode** on top of #655: archive A immediately (no new work) AND mark it
  emergency-draining (drives the outreach + grace behavior below), targeting the active account B.
- **Proactive mass outreach:** enumerate all active subscriptions on A; notify each subscriber with a time-boxed
  "re-enter your card to keep your subscription" flow → SetupIntent/Checkout on B; reminders on a schedule.
- **Aggressive migrate-on-any-touch:** any login/app interaction shows an interstitial to re-enter the card on B
  (not just at natural lapse, as in #655). Reuse the #655 per-user cutover back-half once a card lands on B.
- **Bounded grace extension:** keep entitlements/access alive through the migration window even when A can no
  longer rebill, so users don't lose access and churn out of frustration mid-migration. Bounded, so it isn't
  indefinite free service.
- **Graceful degradation when A goes fully dark:** rebills stop, `sub_A` may not be cleanly cancellable, and
  refunds/chargebacks on A's historical charges may be unmanageable (dead account). Plan for the revenue cliff
  on the un-migrated cohort, mark them explicitly, and decide how in-flight refunds/disputes are handled (may
  have to eat them or work them via the bank directly). Accept involuntary churn for non-responders.
- **Anti-phishing / trust:** "re-enter your credit card" emails look exactly like phishing and tank response
  rates. Design for trust — official in-app prompts, verified channels, support comms — or the 1-month window
  is unreachable.

## Non-goals

- No silent/bulk card move (impossible per the vault-ownership constraint; Stripe's account-copy is explicitly
  out per #655's decision).
- Not the normal gradual drain — that is #655.

## Tasks (planning-level; flesh out when prioritized)

- [ ] Define the operator-declared emergency-migration mode atop #655's `archived` flag (archive A + emergency
      drain + target B).
- [ ] Write the warm-standby runbook: pre-provision + keep a backup processor account ready so B can go active
      instantly; document what must be in place before an emergency.
- [ ] Design the mass outreach campaign: enumerate active A subscriptions, notify, time-box, remind, track
      per-subscriber state.
- [ ] Aggressive migrate-on-touch (interstitial at every login/interaction), reusing the #655 per-user cutover.
- [ ] Bounded grace-window extension so access survives an un-rebillable migration period.
- [ ] A-goes-dark handling: stop expecting A rebills, mark the un-migrated cohort, handle in-flight
      refunds/chargebacks, accept the revenue cliff.
- [ ] Anti-phishing/trust comms design for the re-entry ask.
- [ ] Migration progress reporting: % of active subs and % of MRR migrated, un-migrated cohort, churn.

## Acceptance

- A documented, runnable emergency playbook that moves the active subscriber book A→B via mass re-tokenization
  within a bounded window (target ≤1 month), preserves access during the window, tracks progress, and degrades
  gracefully (bounded grace + accepted involuntary churn) when the source provider goes dark. Builds on #655
  (archived flag + per-user cutover); does not attempt any silent card move.

---

# #118: merchant-admin-dashboard-and-metrics

**Completed:** no — future direction only.

OpenRails will eventually need a merchant admin dashboard plus per-merchant routes that expose the dashboard data. Do not build this as part of the current merchant route/RBAC recut; the exact dashboard cards, route shapes, and metric definitions should be designed when there is a real UI.

## Target Model

- Dashboard data is merchant-scoped and authorized through merchant/org RBAC.
- Prefer a small number of dashboard/reporting endpoints over narrow implementation endpoints such as `manual-rebill-attempts`.
- Aggregate dunning/rebill information as metrics, for example attempted/succeeded/failed counts by period.
- Use `repair-alerts` for operator-actionable failures; use subscription/payment event history for drill-down.
- Keep metric definitions explicit when this is built: revenue, subscriber counts, churn, failed payments, refunds, disputes, dunning outcomes, provider breakdowns, and period comparisons.
- Avoid caching, ClickHouse-specific rollups, or many endpoint variants until the first dashboard needs them.

## Tasks

- [ ] Define the first merchant admin dashboard screens before committing route shapes.
- [ ] Choose the smallest per-merchant metrics/reporting API that supports those screens.
- [ ] Define metric semantics and units, including recurring revenue, one-time payments, refunds, disputes, failed payments, and dunning/rebill outcomes.
- [ ] Decide whether dashboard data comes from Postgres queries, existing analytics tables, or a later rollup store.
- [ ] Add merchant/org RBAC permissions for dashboard reads when routes are actually added.
- [ ] Add integration tests with seeded payments/subscriptions/dunning events once the API shape exists.

## Acceptance

- OpenRails has a merchant admin dashboard route/API design backed by an actual UI need.
- Dashboard metrics include dunning/rebill aggregate outcomes without exposing a bespoke `manual-rebill-attempts` endpoint.
- Operator-actionable payment problems still go through repair alerts, not dashboard counters.

---

# #512: Merchant-downloadable report artifacts backed by S3-compatible storage

**Completed:** no — future direction only.

OpenRails now has CLI/reporting surfaces that can produce large human-readable outputs, especially `audit` and
`reconcile check` / `reconcile fix`. Local CLI runs can write timestamped files, but standalone/server deployments need
a durable remote place to store report artifacts so merchants can list and download them later. This should use a
merchant-scoped S3-compatible bucket, similar to the bucket patterns already used by `~/doujins` and `~/hentai0`.

The first report artifacts should be:
- audit detailed findings from `openrails audit`
- reconcile check reports
- reconcile fix reports

This is separate from the reconciliation ledger. Postgres should remain the source of truth for standing reconcile
state, open findings, acknowledgements, resolutions, and run metadata. The bucket stores immutable human/downloadable
run artifacts.

## Target Model
- Keep this deliberately minimal: a small S3-compatible report writer plus enough metadata to let merchants download
  their own reports.
- Support AWS S3, Cloudflare R2, and MinIO-compatible local/dev stacks through one configuration shape.
- Store artifacts under merchant-scoped object keys, for example:
  `merchants/{merchant_id}/reports/{kind}/YYYY/MM/DD/{run_id-or-timestamp}.{ext}`.
- Persist artifact metadata in Postgres:
  merchant ID, report kind, source run ID if any, object key, filename, content type, format, size, SHA-256, created time,
  retention/expiry, and actor/admin context when available.
- Expose the smallest merchant-authorized HTTP surface needed to download report artifacts.
- Prefer signed object-store URLs if they are already easy with the chosen S3 client; otherwise proxy-stream through
  OpenRails and defer signed URLs.
- Keep stdout bounded:
  command output should print summary counts plus the local path or remote artifact URL/ID, not the full per-account list.
- Keep local-only development usable:
  if no artifact bucket is configured, CLI commands can continue writing a timestamped local file.

## Scope
- Build a reusable artifact/report storage boundary, not an audit-only special case.
- Avoid building a general document-management, export, search, sharing, or lifecycle system in this issue.
- Wire audit detailed table output into artifact storage.
- Wire reconcile check/fix run reports into artifact storage.
- Keep reconciliation standing state in the database.
- Do not use the bucket as the source of truth for reconcile finding status.
- Remove the `openrails reconcile report` sub-command once check/fix artifacts are downloadable.
- Treat future report access as a generic log/report download or stream surface for both audit and reconcile.
- Do not implement invoice export or general CSV exports in this issue, but leave the artifact model reusable for them.

## Questions To Resolve
- Should artifact storage be required in standalone production mode, or optional with a warning when absent? Prefer
  optional for the first pass unless a deployment explicitly needs remote downloads.
- Should the first stored audit artifact be table text only, JSON only, or both text + JSON?
- Should reconcile check/fix always write an artifact, even when there are zero findings?
- Should artifact downloads use short-lived signed URLs by default, or should the first pass proxy-stream downloads to
  avoid extra backend-specific signing complexity?
- Should report artifacts have a simple default retention window, or be retained until explicit merchant/admin deletion?
- Should audit report metadata get its own run table, or should a generic `report_artifacts` table be enough for now?
- Which merchant/admin permission should authorize artifact listing and download?
- What PII or payment-sensitive fields must be redacted before writing downloadable artifacts?
- Should live log streaming be deferred? Prefer yes; start with completed artifact download only.

## Tasks
- [ ] Inspect the bucket configuration and object-key conventions in `~/doujins` and `~/hentai0` before finalizing the
      OpenRails config shape.
- [ ] Add report artifact config:
      backend, bucket, region, endpoint URL, prefix, force-path-style, access-key source, secret-key source, and retention.
- [ ] Add a small `internal/reports` or `internal/artifacts` interface with only the first-pass operations:
      write artifact and open/download artifact.
- [ ] Implement an S3-compatible backend using the chosen AWS SDK or existing repo dependency pattern.
- [ ] Implement a local filesystem backend for tests and local development.
- [ ] Add the smallest Postgres metadata table needed for report artifact lookup, scoped by merchant.
- [ ] Add sqlc queries for creating, listing, and reading report artifact metadata.
- [ ] Wire `openrails audit` table mode so detailed findings are written to a report artifact when storage is configured.
- [ ] Wire `reconcile check` and `reconcile fix` so each run writes a downloadable report artifact.
- [ ] Keep CLI stdout to summary + artifact location.
- [ ] Add minimal HTTP endpoints for merchant/admin report listing and download.
- [ ] Defer live log streaming unless it is needed for the first remote workflow.
- [ ] Enforce merchant isolation on every artifact metadata query and download path.
- [ ] Remove the `openrails reconcile report` sub-command once downloadable artifacts cover human run output.
- [ ] Delete or simplify any reconcile-report-only formatting/storage code that is no longer needed after artifact download
      is available.
- [ ] Document local MinIO/R2/S3 configuration and production bucket requirements.
- [ ] Add retention cleanup only if the metadata model includes expiry in the first pass; otherwise leave retention to the
      bucket lifecycle policy.

## Acceptance
- A merchant can run or trigger audit/reconcile and receive a compact summary plus a report artifact reference.
- In server/remote mode, report details are stored in the configured S3-compatible bucket.
- A merchant admin can list and download only their own report artifacts.
- Reconcile standing state remains queryable from Postgres and is not replaced by log files.
- `openrails reconcile report` is gone; humans use downloadable report artifacts instead.
- The implementation is small and report-focused, while still not audit-only.
- Local development works with either local filesystem storage or MinIO.

## Validation
- [ ] Unit tests for object key construction and metadata validation.
- [ ] Unit tests for local artifact storage.
- [ ] Integration test against MinIO or another S3-compatible test backend.
- [ ] HTTP tests proving merchant A cannot list or download merchant B's artifacts.
- [ ] CLI tests proving audit stdout stays summary-only and writes/uploads the detailed artifact.
- [ ] CLI tests proving reconcile check/fix write/upload report artifacts.
- [ ] `go test ./internal/artifacts ./cmd/openrails`
- [ ] `go test ./...`

---

# #511: Develop the audit finding taxonomy into a clearer operator-facing system

**Completed:** no — future direction only.

OpenRails now has an internal audit finding taxonomy (`P-E-1`, `SS-1`, `S-E-1`, etc.) for local DB consistency checks.
The current compact IDs are stable and useful in logs, but they are not self-explanatory for operators. They read a bit
like HTTP status codes, but unlike HTTP they are not a shared industry standard and do not describe request outcomes.
They are OpenRails domain-invariant failures: payment↔entitlement, subscription state, subscription↔entitlement,
duplicates, temporal anomalies, and reference mismatches.

The next step is to turn this from "short internal labels" into a deliberate operator-facing taxonomy with clear names,
documentation, stable identifiers, and output/reporting that scales beyond a few findings.

## Current State
- `check_id` is a compact internal code:
  `P-E-1`, `SS-1`, `S-E-1`, `D-2`, etc.
- `check_name` is already the more readable slug:
  `completed_payment_missing_entitlements`, `active_subscription_past_period_end`, etc.
- Table-mode stdout is now summary-focused and writes the detailed per-account list to an audit log file.
- `docs/consistency-invariants.md` documents the target unified taxonomy (M/D/L/X/I) that supersedes these compact codes.

## Design Goals
- Preserve stable machine/log references for historical audit runs.
- Make human output understandable without memorizing prefixes.
- Make codes searchable in docs and support runbooks.
- Separate identity from presentation:
  stable ID, readable slug, title, category, severity, remediation, and owner domain.
- Avoid pretending these are industry/payment-network codes.
- Keep the taxonomy small enough that operators can scan it, but precise enough that repeated incidents can be grouped.

## Questions To Resolve
- Should compact IDs stay as the canonical `check_id`, or should slug names become canonical?
- Should every finding expose both:
  `check_id = P-E-1` and `check_code = payment_entitlement.missing_one_off_grant`?
- Should compact IDs be renamed now, or retained forever as historical stable labels?
- Should check IDs use domain prefixes (`P-E`, `SS`) or severity/class prefixes like HTTP-style families?
- Should `check_name` strings be normalized to dotted slugs instead of snake_case?
- Should findings include a short human title separate from the longer description?
- Should findings include an explicit `owner_domain` / `data_owner` field:
  payments, subscriptions, entitlements, catalog, admin_grants, payment_methods?
- Should remediation be typed:
  `auto_fixable`, `manual_refund`, `rerun_worker`, `contact_customer`, `configuration_error`, etc.?
- Should audit and reconcile taxonomies converge, or stay separate?
  Audit checks local DB invariants; reconcile checks local-vs-processor truth. They overlap conceptually but are not the
  same system.

## Proposed Direction
- Keep compact `check_id` stable for backward compatibility with historical logs and runbooks.
- Add a readable dotted `check_code` or promote `check_name` to this style:
  - `payment_entitlement.missing_one_off_grant`
  - `subscription_state.active_past_period_end`
  - `subscription_entitlement.active_missing_grant`
  - `duplicate.charge_same_period`
- Add a short title field for terminal/report display:
  "Payment missing entitlement grant", "Active subscription past period end", etc.
- Keep stdout grouped by readable title first, compact ID second:
  `payment_entitlement.missing_one_off_grant (P-E-1)`.
- Generate or maintain a taxonomy table in docs from the live check registry so docs cannot drift.
- Use the detailed audit log for individual entities and stdout for counts/groupings only.

## Tasks
- [ ] Inventory every active `internal/audit` check:
      compact ID, current `Name()`, category, severity, entity type, auto-fixability, and recommendation.
- [ ] Identify inactive/historical IDs that are still referenced in completed issues, docs, or runbooks.
- [ ] Decide canonical identifier model:
      compact-only, slug-only, or compact + slug.
- [ ] Decide whether slug style should be snake_case or dotted namespace.
- [ ] Add a title/display-name field to the audit check interface if needed.
- [ ] Add structured taxonomy metadata if needed:
      owner domain, remediation type, data source, and whether the failure is prevented by DB constraints in new installs.
- [ ] Update stdout `By Check` output to show the chosen readable label before or alongside compact IDs.
- [ ] Update JSON output to include all taxonomy fields without breaking existing `check_id` consumers.
- [ ] Update CSV output to include the readable slug/title fields.
- [ ] Generate or update `docs/consistency-invariants.md` with a complete table of active checks.
- [ ] Add a short "how to read audit findings" section:
      ID, severity, category, entity, user/customer, details file, and remediation.
- [ ] Decide whether `internal/reconcile` finding types should share terminology with audit findings or stay in a
      separate "processor reconciliation" taxonomy.
- [ ] Add tests proving compact IDs remain stable and every active check has complete taxonomy metadata.
- [ ] Add tests proving stdout uses readable labels and detailed logs still include compact IDs for searchability.

## Acceptance
- Operators can understand common audit output without memorizing `P-E`, `S-E`, `SS`, etc.
- Every active audit check has:
  stable compact ID, readable code/name, category, severity, title, and remediation guidance.
- The docs contain a complete current taxonomy table.
- Stdout remains compact and readable when hundreds of findings exist.
- Detailed logs preserve per-entity evidence and stable IDs.
- JSON/CSV output remains usable for automation and includes the improved readable taxonomy fields.

## Validation
- [ ] `go test ./internal/audit ./cmd/openrails`
- [ ] `go test ./...`
- [ ] Manual smoke:
      run `openrails audit` against a DB with findings and confirm stdout is readable and bounded.
- [ ] Manual smoke:
      confirm the detailed audit log contains per-account findings with compact ID + readable code/title.

---

# #509: Solana invoice collection authorization and settlement

**Completed:** no — future direction only.

Solana invoice collection is valuable, but it is materially different from Stripe/NMI automatic collection. A linked
wallet is not a saved payment method that OpenRails can charge. Before Solana can collect arrears invoices, OpenRails
needs an explicit authorization or prefunding model that defines what funds are available, who can move them, how much
can be collected, when collection expires, and how settlement finality is detected.

This is intentionally split out of #508 so Stripe and NMI/Mobius invoice collection can ship first.

## Target Model
- Solana invoice collection must require a prior customer-approved agreement:
  escrow/prefund, delegated token authority, program-controlled allowance, durable nonce/signature flow, or another
  explicit mechanism chosen after design.
- Ordinary linked wallets are never collectible by default.
- OpenRails invoices remain the receivable source of truth; Solana only supplies a settlement rail.
- Collection should record transaction signatures, token account/source details, amount, currency/asset, network, and
  confirmation status.
- Invoice payment should become settled only after sufficient on-chain confirmation and local reconciliation.
- Failed or expired Solana authorization leaves the invoice open/past-due and feeds admission/dunning like other failed
  collection rails.

## Questions To Resolve
- Which authorization model is simplest and safest for USDC on Solana: prefunded escrow, SPL token delegate allowance,
  program escrow, or customer-signed one-shot payment request?
- Should collection support partial invoice payments, or require invoice-total availability before collecting?
- How should authorization limits map to arrears credit limits?
- What confirmation depth/finality is required before marking an invoice payment settled?
- How are refunds/voids handled if an invoice is later adjusted?
- How does this interact with existing Solana checkout/subscription/funding-session code?

## Tasks
- [ ] Inventory existing Solana wallet, funding-session, checkout, subscription, and signer code that could be reused.
- [ ] Inventory current Solana DB tables and models:
      linked wallets, funding sessions, Solana subscriptions, signer keys, vault/signer configuration, and payment rows
      that already store signatures.
- [ ] Inventory current Solana HTTP/service APIs that create wallet links, funding sessions, checkout payments, and
      recurring/subscription state.
- [ ] Decide whether invoice collection uses prefunded escrow, SPL token delegate allowance, program escrow, or
      customer-signed one-shot payment request.
- [ ] Document the chosen model's customer consent text, revocation path, maximum exposure, and who can submit
      transactions.
- [ ] Document why ordinary linked wallets are not collectible and add this as an explicit invariant.
- [ ] Define whether partial invoice collection is supported in the first version.
- [ ] Define how Solana authorization amount maps to arrears credit limit and invoice amount due.
- [ ] Define confirmation/finality policy:
      commitment level, number of confirmations if applicable, timeout, and retry behavior.
- [ ] Define refund/void/adjustment behavior for Solana-collected invoice payments.
- [ ] Add a Solana invoice authorization table or payment-method metadata:
      merchant_id, customer_id, network, mint, source account, destination/escrow account, authorized amount,
      consumed amount, expires_at, revoked_at, status, and created/updated timestamps.
- [ ] Add uniqueness/idempotency constraints so one authorization cannot be consumed twice for the same invoice attempt.
- [ ] Add sqlc queries to create, revoke, list, and load active Solana invoice authorizations by merchant/customer/asset.
- [ ] Add sqlc queries to reserve authorization capacity for an invoice attempt without racing another worker.
- [ ] Add sqlc queries to mark an authorization consumed/released after confirmed settlement or failed attempt.
- [ ] Add Solana invoice collection capability metadata separately from Stripe/NMI capabilities.
- [ ] Implement a deterministic fake Solana ledger:
      balances, allowances/escrows, submitted signatures, confirmation states, failed transactions, and dropped
      transactions.
- [ ] Add fake-ledger test for successful authorization reservation before writing any real Solana client.
- [ ] Add fake-ledger test for confirmed settlement applying an invoice payment exactly once.
- [ ] Add fake-ledger test for dropped/ambiguous transaction requiring reconciliation before retry.
- [ ] Implement collection scope validation:
      invoice merchant/customer matches authorization merchant/customer and asset/network.
- [ ] Implement amount validation:
      invoice currency maps to the configured USDC mint/network and amount does not exceed available authorization.
- [ ] Implement transaction construction/submission for the chosen authorization model.
- [ ] Record a pending `invoice_payments` row with processor `solana`, signature or pending attempt id, network, mint,
      and authorization id.
- [ ] Implement confirmation polling or callback handling to settle pending Solana invoice payments.
- [ ] Apply confirmed Solana payment to the invoice without overpaying or double-applying under retry.
- [ ] Implement reconciliation:
      verify signature status, handle expired blockhash/signature, handle dropped transactions, and repair pending state.
- [ ] Add failure handling:
      no authorization, insufficient authorization, expired authorization, revoked authorization, wrong network/mint,
      failed transaction, ambiguous confirmation, and signer/RPC failure.
- [ ] Add admission/dunning feedback so failed Solana invoice collection blocks or reduces further arrears capacity.
- [ ] Add admin/service APIs to create/revoke/list Solana invoice authorizations.
- [ ] Add admin/support reporting for authorization id, signature, network, mint, confirmation status, and failure reason.

## Acceptance
- A finalized/open invoice can be collected through Solana only when a valid prior authorization or prefunded balance
  exists for that customer/merchant/asset/network.
- OpenRails cannot collect from a normal linked wallet without explicit authorization.
- Retried collection cannot double-spend or double-apply invoice payment.
- Invoice payment is settled only after confirmed on-chain settlement.
- Failed or expired authorization leaves the invoice receivable intact and blocks/reduces further arrears capacity.

## Validation
- [ ] Deterministic fake-ledger integration test:
      finalized invoice -> authorized USDC collection -> confirmed invoice payment.
- [ ] Failure-path integration tests:
      no authorization, insufficient authorization, expired authorization, wrong network/mint, ambiguous confirmation,
      retry/idempotency, and cross-merchant authorization misuse.
- [ ] Authorization lifecycle integration test:
      create authorization -> reserve capacity -> revoke/expire prevents future collection.
- [ ] Partial-vs-full collection test based on the chosen first-version policy.
- [ ] Confirmation/reconciliation test:
      missed confirmation is repaired from signature lookup.
- [ ] Admission feedback test:
      failed or expired Solana collection blocks/reduces further arrears capacity.
- [ ] Devnet integration checklist once the local model is stable.

---

# #498: Deliberate admin audit logging and global break-glass, if needed later

**Completed:** no — future direction only.

The old Postgres-backed `platform_audit`, `platform_break_glass`, and
`merchant_credential_audit` features were removed as unnecessary platform slop.
Do not recreate those tables by default.

If OpenRails later needs this capability, design it deliberately:

- Admin action audit logging should probably be structured event logging in
  ClickHouse or another analytics/audit pipeline, not hot Postgres tables added
  to the control-plane schema by default.
- Global break-glass/admin takeover should be a separate, explicit security
  feature with clear operational controls, expiry, approval/justification model,
  alerting, and tests. It should not be implied by ordinary platform metrics or
  merchant-admin routes.

Bring this back only when there is a concrete hosted-operations requirement.

---

# #479: OpenMeter-parity ideas — first-class configurable METERS + priority-based grant burn-down

**Completed:** no — future direction (not blocking). Surfaced comparing OpenRails to OpenMeter
(openmeterio/openmeter). OpenRails is already a superset (metering + entitlements + its own
billing/payments/dunning + multi-merchant control plane + admission/rate-limit/fairness-policy +
spend-graduated tiers), but two OpenMeter concepts are worth adopting:

1. **First-class configurable METERS.** Today usage is recorded as `usage_events` (#289) + dimensions
   and aggregated implicitly for billing; throughput units (#472) and budget windows (#475/#476) are
   semi-separate. OpenMeter models a **Meter** as a named, configurable aggregation —
   `agg(event.field) over window` (sum/count/unique/min/max), e.g. `sum(tokens)`, `count(requests)`,
   `unique(model)`. Make meters first-class and have limits/entitlements/tiers *reference a meter*
   instead of hardcoded unit strings (`token`/`image`/`request`). Unifies throughput + budget windows
   + usage analytics under one substrate; adding a metered dimension becomes config-only.

2. **Priority-based grant burn-down.** OpenMeter's prepaid credits consume grants in a defined
   PRIORITY order (which grant/credit to spend first) with reset/rollover. OpenRails' credit/grant
   model could formalize the same — relevant to #475 custom credits + entitlement grants (e.g. spend
   promotional/expiring grants before purchased balance).

NOT in scope for the rate-limit/tier system (which is done). Build when a real need appears. Reference:
OpenMeter (metering + entitlements; explicitly has NO request scheduler/fair-queuing — that stays the
orchestrator's job, confirming OpenRails' boundary: gate/meter/bill, not dispatch-reorder).

---

# #228: private admin UI and hosted OpenRails SaaS console boundary

**Completed:** no

Build a private-mode OpenRails admin UI for merchant operations, and keep public hosted onboarding/registration in a
separate `openrails-saas` repo. OpenRails itself should ship the source-available/admin console needed by private
standalone installs; the hosted public platform can build a richer SaaS product on top without changing OpenRails'
core route/auth model.

## Product vision — the shared dashboard (owner, 2026-06-19)
Model it like Stripe's dashboard. ONE admin-dashboard codebase, shipped in TWO modes:
- **standalone-private OpenRails** (source-available, this repo) — public registration PERMANENTLY OFF; operators are seeded via bootstrap/AuthKit.
- **~/openrails-saas** (separate private repo) — public registration ON: a user signs up, then creates an **org + merchant** (the Stripe "create account" flow). All onboarding/registration/marketing lives in openrails-saas, never here.

In BOTH modes the dashboard is the same product surface; only the entry (registration vs seeded) differs.

**Users (operators) in OpenRails are purely access-control:** "who may access which merchant dashboards, within their limited roles." They are AuthKit users + org membership + RBAC (AuthKit #91) — NOT customers, NOT billing identities. An operator logs in, and (scoped by their roles) can:
- **Merchant config** — provider configurations (Stripe/NMI/CCBill/Solana credentials, test/live mode, routing). [#228 PROVIDER+CATALOG OPS]
- **Customer actions, PROVIDER-NEUTRAL** — cancel subscriptions, refund, comp/grant entitlements, off-channel payments, etc. across any processor. *The API for this already exists* — it's the #528 delegated `/v1/admin/*` surface (`DELETE /subscriptions/:id`, `POST /payments/:id/refunds`, entitlement + product-access writes — all provider-neutral). The dashboard is a client of it; the only net-new backend piece is the customer LIST/search to FIND the customer first (domain #2 below).
- **Catalog items** — products, prices, entitlements (change prices, add products, edit entitlement specs). Backed by catalog-as-code apply + the catalog services.

So the dashboard composes three already-mostly-built backends (AuthKit directory for operators, #528 delegated admin for provider-neutral customer actions, catalog/provider services) + two net-new lists (customers, merchants). It is the productized UI over the existing capability-gated routes (#510), not a new auth model.

**Operators ARE the merchant's AuthKit org members (CONFIRMED, owner 2026-06-19) — no new authz model needed.** Each OpenRails merchant already has a backing AuthKit org whose `owner` is the registered issuer (#527). So "operators of merchant X" == "members of merchant X's AuthKit org, with org roles," and dashboard access scopes by org membership + org RBAC (the same model that gates everything else). The Stripe-style onboarding is literally #527's provisioning shape: **create AuthKit org → provision the backing OpenRails merchant → invite operators as org members with roles.** Standalone-private seeds that via bootstrap; openrails-saas wraps it with public registration. Net: operator management = AuthKit org membership/roles UI (reuses #91 + AuthKit's org-members API), and there is NO OpenRails-side operator/user store.

## Three management domains — who owns the list/search/sort (decided 2026-06-19)
The standalone console manages THREE distinct entity sets; do NOT conflate them:
1. **Dashboard operators/staff** (the humans who LOG IN to this console) → **AuthKit** owns this. List/search/sort/invite + role assignment = AuthKit's user directory + RBAC (AuthKit #91, the SAME surface doujins/cozy-art reuse). OpenRails builds NONE of this — the console calls AuthKit's `GET /admin/users` for operator management. (Public/SaaS adds registration/onboarding in the separate `openrails-saas` repo.)
2. **Customers** (the merchant's PAYERS — delegated subjects from the merchant's issuer; NOT the same people as operators in the SaaS case) → **OpenRails billing domain**. `openrails.customers` holds only `(merchant_id, org_id, issuer, subject, created_at, last_seen_at)` — NO email/username. So OpenRails owns a customer LIST/search/sort by BILLING attributes (subject, subscription status, entitlement, spend, last_seen), and ENRICHES identity from AuthKit when AuthKit is the merchant's issuer (the inverse of the doujins enrich seam); when the issuer is external, it shows subject + billing only. This is net-new OpenRails work (no admin customer-list query exists today).
3. **Merchants** (multi-merchant public/platform deployments) → **OpenRails SaaS operator surface**. OpenRails core no longer exposes `/v1/platform/*` or a `PermPlatformSuperadmin` route gate; design merchant list/search/sort in the SaaS layer if/when hosted operations need it. Single-merchant private installs skip this.

So: OpenRails DOES build customer + merchant admin listing (billing/control-plane entities it owns); it does NOT build operator/user-directory listing (AuthKit's). In the EMBEDDED-host case (doujins) operators==customers==AuthKit users (one set, two facets), so there's just the enriched user directory and no separate customer page; the separate "Customers" page only appears in standalone where operators ≠ payers.

## Metadata

- Category: product
- Status: future
- Passes: false

## Goals

- Private OpenRails mode: an admin UI only. Users do not publicly register; authority is seeded through bootstrap/AuthKit.
- Public hosted mode: user registration, org/merchant onboarding, remote application creation, billing for OpenRails
  customers, and other SaaS features live in the separate private `openrails-saas` repo, not this source-available repo.
- Primary OpenRails UI goal: merchant operations and observability for already-provisioned merchants.
- Make OpenRails' operational state visible without SQL or ad hoc CLIs: subscriptions, checkout sessions, payments,
  entitlements, credits, catalog/provider status, webhook failures, rebills, refunds/disputes, and processor-specific
  lifecycle actions.
- Give merchants confidence in non-Stripe rails by showing processor configuration, test/live mode, last successful
  checks, catalog provider status, and known limitations.
- Use the unified merchant action auth model from #510. The UI is just a client using the same capability-gated merchant
  action routes as API keys, remote applications, delegated merchant admins, and user access tokens.

## Non-goals (initially)

- Public sign-up, org onboarding, checkout/pricing pages for hosted OpenRails, or marketing pages in this repo.
- The hosted OpenRails SaaS product. That belongs to `openrails-saas` and can be closed-source/private.
- Customer-facing self-serve dashboard for end users.
- Replacing core billing APIs.
- A marketing analytics site; this is an operational merchant console for private standalone OpenRails.

## Notes

- Keep the surface area minimal: read-heavy analytics and support visibility first, then a small set of high-confidence mutations.
- Every mutation must be gated + audited (who/what/when/why) through the same route/auth model as #510.
- Treat this as the productized surface for the backend roadmap items around processor capabilities, routing/fallback, provider certification, catalog-as-code status, dunning, and credits.
- Private mode should be useful with no public registration enabled. Admin users, API keys, remote applications,
  roles, and permissions come from bootstrap/AuthKit.
- Public mode is a hosted-platform concern: `openrails-saas` can enable AuthKit public registration, merchant creation,
  onboarding flows, plan/billing for OpenRails customers, marketing/product UX, and managed-hosting support workflows.
- OpenRails should expose the API primitives and private admin UI; `openrails-saas` composes those primitives into the
  public hosted product.

**Tasks:**
> TERMINOLOGY (read the "Three management domains" section above first): in the task list below, "user lookup" / the "User Lookup" page / `/v1/admin/users/*` mean **CUSTOMER** lookup (domain #2 — the merchant's payers, keyed by subject, OpenRails billing). They are NOT operator/staff management (domain #1 — that's AuthKit's user directory, #91). The two are separate pages.
- FOUNDATION:
- [ ] Choose private admin UI hosting path in this repo:
      serve static assets from the OpenRails binary, or ship a separate static app that calls the OpenRails API.
- [ ] Define private-mode entrypoint and deployment defaults:
      UI disabled unless configured, no public registration, AuthKit sign-in only for seeded users/roles.
- [ ] Align admin UI authorization with #510:
      no separate admin-only route/auth model; UI calls capability-gated merchant action routes with a user access token
      carrying the required permissions for that merchant.
- [ ] Define the normalized UI principal display:
      user, API key, service JWT, remote application, delegated merchant admin, merchant scope, permissions, and
      platform-superadmin state where applicable.
- [ ] Add structured audit log events for merchant action mutations:
      actor, credential type, merchant, action, target, before/after where safe, request_id, reason, source UI/API.
- [ ] Define `openrails-saas` boundary doc:
      public registration/onboarding and hosted-platform UX live in the separate private repo; OpenRails exposes reusable
      auth/provisioning/billing APIs and the private admin console.
- 
- ANALYTICS (READ):
- [ ] Define core metrics: gross sales, net sales, refunds, chargebacks, MRR, active subs, churn, failed attempts
- [ ] Implement time-series queries (today, 7d, 30d) with timezone handling
- [ ] Add breakdowns: processor (NMI/CCBill/Solana), product/tier, country (if available)
- [ ] Add payments funnel: attempts -> succeeded -> refunded/chargeback -> net
- [ ] Add dunning visibility: past_due count, retries, recovered vs churned
- 
- ADMIN OPS (MUTATIONS):
- [ ] User lookup + detail view (subscriptions, checkout sessions, payments, entitlements, credits, payment methods, processor IDs)
- [ ] User SEARCH/LIST/DETAIL for this console — **COMPOSE, don't rebuild** (absorbed the old #108; decided 2026-06-19 after reading ~/authkit + ~/doujins). The user DIRECTORY (list/search/sort/detail by email/username/role/org) is **AuthKit's**, never OpenRails'. This console is a BILLING/merchant console keyed by SUBJECT; customers are delegated subjects from the merchant's issuer, so OpenRails stores only `openrails.customers (issuer, subject, customer_id)` + billing — it has NO email/username. So:
    - When the merchant's issuer IS an AuthKit instance the console can reach → compose: call AuthKit's `GET /admin/users` (directory: search/list/detail) and enrich each row with OpenRails billing via the `BatchEntitlementsProvider` seam (exactly how doujins' embedded admin already works). Filter-by-entitlement ("premium users") flows through that seam, backed by OpenRails #535's reverse query — NOT a new OpenRails search endpoint.
    - When the merchant uses a NON-AuthKit identity → the console can only show billing keyed by opaque subject + whatever the issuer's OIDC userinfo exposes; rich identity stays in the merchant's own app.
    - So OpenRails builds NO `/v1/admin/users?q=` directory/search endpoint. The "sophisticated search" (by email / transaction_id / processor_subscription_id, sort, full-text) is the AuthKit directory's job (generalize AuthKit `AdminListUsers`) plus OpenRails billing-side filters (subscription_status / processor / transaction_id lookups) exposed as enrichment, never as a standalone user list. (Cross-repo: pairs with the AuthKit directory issue + OpenRails #535.)
- [ ] Add billing timeline per user/subscription that merges payments, subscription events, entitlement changes, credits, webhooks, and admin actions
- [ ] Support merchant actions through unified capability-gated routes:
      cancel subscription, pause/resume, extend access, comp/refund, grant/revoke entitlement, grant/revoke credits,
      retry rebill, replay webhook, reconcile/apply catalog item (only where safe).
- [ ] Add external processor links where available (Stripe objects, NMI transaction/plan identifiers, CCBill portal/admin references, Solana signatures/accounts)
- [ ] Guardrails: confirmation steps, rate limiting, and require a reason for every mutation
- [ ] Safe idempotency for all admin operations (avoid double-cancel/refund)
- 
- PROVIDER + CATALOG OPS:
- [ ] Provider health page: configured processors, sandbox/test/live mode, credential presence, last successful API call, supported capabilities, and known limitations
- [ ] Catalog/provider page: catalog-as-code apply status, provider object ids, NMI recurring plan ids, Stripe Product/Price ids, CCBill pending_manual_actions, Solana plan accounts, and open catalog drift events
- [ ] Bootstrap/provisioning page for private mode:
      show current merchant, owner org, roles, API keys (metadata only), remote applications, catalog manifest status,
      last apply result, and prune/archive-extra warnings.
- [ ] Catalog apply UI:
      upload/paste/select manifest, dry-run plan, apply, optional `--prune` equivalent with explicit warning that
      OpenRails-owned provider extras are archived and foreign objects are untouched.
- [ ] Remote application management UI:
      list registered AuthKit remote applications for the merchant, issuer/JWKS status, enabled state, role/permission
      assignments, last verification result, and safe disable/rotate workflows.

- PUBLIC/HOSTED OPENRAILS-SAAS:
- [ ] In this repo, document that public hosted registration/onboarding is out of scope and belongs to `openrails-saas`.
- [ ] In `openrails-saas`, plan a public registration flow:
      sign up, create org, create merchant, create/verify remote application, seed catalog, configure provider secrets,
      invite team members, and enter the private/admin console for that merchant.
- [ ] In `openrails-saas`, plan hosted-platform features:
      OpenRails customer billing, plan limits, managed support workflows, platform-superadmin operations, hosted status,
      onboarding checklists, and product/marketing pages.
- [ ] Keep `openrails-saas` private/non-source-available; it can depend on OpenRails APIs and SDKs without pushing hosted
      product concerns into this repo.
- [ ] Routing policy page when #288 exists: show preferred processors, fallback order, disabled rails, and dry-run/explain selected processor for a price/user/merchant
- [ ] Certification matrix page or linked view when #290 exists: show last verified date/environment/command for provider flows without exposing secrets
- 
- API + DATA LAYER:
- [ ] Add dedicated admin endpoints (e.g. /v1/admin/analytics/*, /v1/admin/users/*)
- [ ] Add indexes/materialized views if needed for dashboard performance
- [ ] Add caching strategy for expensive aggregates (optional)
- 
- UI:
- [ ] Minimal dashboard pages: Overview, Payments, Subscriptions, Checkout Sessions, Dunning, Credits, User Lookup, Providers, Catalog
- [ ] Tables: filter/sort/paginate; export CSV for key views
- [ ] Detail drilldowns from charts -> filtered tables
- [ ] Compact support workflow: search -> user/subscription/payment detail -> safe action -> audit result
- 
- VERIFICATION:
- [ ] Unit tests for analytics queries (edge dates/timezones) and authz checks
- [ ] Integration tests for a small set of admin mutations + audit logging
- [ ] Manual checklist for production access controls and least privilege

---

# #134: stripe-meter-connector (optional; Stripe-hosted metered invoices)

**Completed:** no

OPTIONAL connector for the narrow case where a customer specifically wants STRIPE to host their metered billing + invoice, instead of OpenRails' native usage metering (#289). Native metering (#289) is the DEFAULT: it works across all rails (NMI/CCBill/Solana) and feeds the credit ledger. This issue only adds a thin one-way sync so a Stripe-native customer's usage_events are mirrored to Stripe's Billing Meter API and Stripe generates the invoice.

This is niche, NOT the main path. DROPPED from the original plan (do not build): the internal Stripe meter / meter_events mirror tables, the metered_subscriptions table, and any native invoice generation.

Mechanism: POST usage_events to Stripe /v1/billing/meter_events; let Stripe aggregate + invoice; handle invoice.paid / invoice.payment_failed webhooks for status only.
Refs: https://docs.stripe.com/api/billing/meter , https://docs.stripe.com/api/billing/meter-event

**Tasks:**
- [ ] Forward usage_events (#289) to Stripe via POST /v1/billing/meter_events (event_name + stripe_customer_id + value + idempotency identifier).
- [ ] Map OpenRails event_type -> Stripe meter event_name; document the one-time Stripe Meter + metered Price setup.
- [ ] River retry/queue for failed meter-event syncs.
- [ ] Handle invoice.paid / invoice.payment_failed webhooks (status only; Stripe owns the invoice in this mode).
- [ ] Tests: usage_event -> meter_event sync + idempotency.
- DROPPED (do not build): internal meter/meter_events mirror tables, metered_subscriptions table, native invoice generation.

---

# #230: auto-topup hardening (core shipped #239; caps + disable-on-failure + extras)

**Completed:** no

Auto-topup so a customer's prepaid credit never hits $0 and gets shut off: when balance drops below a floor, charge their card a fixed amount and deposit it.

## CORE ALREADY SHIPPED (#239) — the user's exact ask
On CreditAccountSettings: low_balance_threshold_cents (the floor, e.g. $100), auto_topup_amount_cents (the charge, e.g. $50), auto_topup_enabled, auto_topup_payment_method_id, last_topup_at (cooldown). RunAutoTopups (internal/modules/credits/money_in.go) scans below-threshold accounts and charges the card OFF-SESSION via Charger + deposits credits; AutoTopupWorker runs it every 15min (River); LowBalanceAlertWorker (#240) alerts. Configured via the admin settings endpoint (service_credits.go). Tests: TestRunAutoTopups_ChargesAndDeposits/_Declined.
=> 'keep $100, charge $50 when it dips below' = set threshold=10000, amount=5000, enabled=true, a payment method. Both values are user-configurable. NOTHING TO BUILD for the basic feature.
NOTE: implemented as a fixed-amount charge + cooldown on CreditAccountSettings, NOT the separate auto_topups/auto_topup_events tables or price_id-package model this issue originally sketched (superseded).

## REMAINING (optional deltas only)
- Hard safety caps (max topups per day/week/month) beyond the single cooldown.
- Disable-after-N-consecutive-failures + notify the user.
- Optional 'top up TO a target' mode vs the current fixed-amount charge.
- Lago wallet cherry-picks: GRANTED_CREDITS (paid + bonus in one topup, 'buy $50 get $5 free' — needs Deposit to carry granted vs paid), and INTERVAL (scheduled) top-ups distinct from the threshold trigger, with a pending-transaction guard.

**Tasks:**
- [x] Threshold trigger + fixed-amount off-session charge + deposit + cooldown (SHIPPED #239: RunAutoTopups, AutoTopupWorker).
- [x] Settings surface (threshold, amount, payment method, enabled) + admin endpoint (SHIPPED: CreditAccountSettings + service_credits.go).
- [x] Low-balance alerts (SHIPPED #240: LowBalanceAlertWorker).
- [x] Integration tests for charge+deposit and decline (SHIPPED: money_in_integration_test.go).
- REMAINING (optional deltas):
- [ ] Hard safety caps (max topups per day/week/month) beyond the single cooldown.
- [ ] Disable-after-N-consecutive-failures + notify the user.
- [ ] OPTIONAL 'top up TO target' mode (vs the current fixed-amount charge).
- [ ] GRANTED_CREDITS: paid + granted in one topup ('buy $50 get $5 free') — needs Deposit to carry granted vs paid.
- [ ] INTERVAL top-ups: scheduled (e.g. monthly) trigger distinct from the low-balance threshold, with a pending-transaction guard.

---

# #202: admin-credit-blocks (manual grant + revoke by block)

**Completed:** no

Add a minimal admin surface for managing user credits as immutable "blocks":

- Admin can **create** a credit block (grant credits, optional expiry)
- Admin can **delete** a credit block (revoke the remaining credits from that specific grant)

This intentionally avoids a full taxonomy (refund/admin_adjust/etc.). The goal is a small, safe API that maps to support workflows.

## Metadata

- Category: admin
- Status: future
- Passes: false

## Goals

- Minimal mutation surface: create block, delete block.
- Revocation is scoped to a specific block/grant, not "take N credits from the user".
- Strong consistency under concurrency (holds, captures, withdrawals, and block revocation).
- Clear auditability: who/what/when/why for every admin mutation.

## Non-goals (initially)

- Editing blocks (no partial edits or amount changes).
- Automatically interpreting payment refunds as credit revocations.
- Allowing negative balances (clawbacks) unless explicitly added later.

## Design sketch

### Data model

Today, revocation-by-block is hard because only expiring deposits create `billing.credit_expiry_batches`, and withdrawals consume FIFO by `expires_at`.

Introduce an explicit "block" record so we can revoke remaining credits precisely:

- New table: `billing.credit_blocks`
  - {id, user_id, credit_type_id, original_amount, remaining_amount, expires_at NULL, source, source_id UUID NULL, created_by, revoke_reason, revoked_at, created_at, updated_at}
  - Unique/indexes: (user_id, credit_type_id, expires_at), (user_id, credit_type_id, revoked_at IS NULL)

Consumption rule:
- On withdrawal/capture, decrement `credit_blocks.remaining_amount` in deterministic order:
  - Prefer earliest `expires_at` first, then oldest created.
  - Treat NULL expires as "never", ordered last.

Balances rule:
- Keep `billing.user_credit_balances` as the fast denormalized read.
- All mutations that affect blocks also update `balance` in the same DB transaction.

### APIs

Private admin/service routes (X-API-KEY):
- `POST /v1/admin/credit-blocks` create a block
- `DELETE /v1/admin/credit-blocks/:id` revoke remaining credits from that block
- (Optional but likely needed) `GET /v1/admin/credit-blocks?user_id=&type=` list blocks for a user

### Semantics for delete/revoke

- Default: revoke only the block's `remaining_amount` at time of deletion.
- If the block has been partially spent, deletion still succeeds and just removes what remains.
- Guardrail: do not allow revocation that would make `balance < held_balance`.
  - Return 409 with guidance to release/expire holds first.

## Integration points

- `internal/services/credits_service.go`:
  - Add a block-backed accounting path for deposits/withdrawals/captures.
  - Keep existing ledger writes to `billing.credit_transactions` for observability.
- `internal/river/jobs_credit_expiry.go`:
  - If blocks have expiries, expiry worker should zero `remaining_amount` and reduce balance (analogous to existing batches).
  - Or keep expiry as a direct block operation without `credit_expiry_batches`.

## Exit Criteria

- Admin can grant credits with optional expiry as blocks.
- Admin can delete a block and the system removes only the remaining credits from that grant.
- All operations are race-safe with holds/capture/withdrawal.
- Tests cover concurrency and edge cases (partial spend + revoke, holds present, expiry).

**Tasks:**
- DATA MODEL:
- [ ] Add migration for billing.credit_blocks table + indexes
- [ ] Decide how to map existing credit_expiry_batches (keep, migrate, or deprecate)
- 
- SERVICE METHODS:
- [ ] Add CreateCreditBlock(user_id, type, amount, expires_at?, reason, source?)
- [ ] Add RevokeCreditBlock(block_id, reason) that removes remaining_amount and marks revoked_at
- [ ] Update withdrawal/capture logic to consume from blocks deterministically (expiry first)
- [ ] Enforce guardrail: prevent balance dropping below held_balance on revoke/expiry
- 
- HTTP API:
- [ ] Add private admin endpoints under /v1/admin (create/delete/list blocks)
- [ ] Return stable IDs for blocks and expose remaining/expiry info
- 
- WORKERS:
- [ ] Implement expiry worker for credit_blocks (or adapt existing credit expiry worker)
- 
- AUDITABILITY:
- [ ] Plumb through actor + reason fields from admin requests
- [ ] Add structured audit log event per mutation (optional table if not already present)
- 
- TESTS:
- [ ] Integration tests: create block -> spend -> revoke -> verify balances and remaining
- [ ] Integration tests: revoke blocked by holds (balance < held_balance)
- [ ] Concurrency test: withdrawal and revoke racing (no double spend, no negative remaining)

---

# #260: solana-swap-to-usdc-via-jupiter

**Completed:** no

Add a frontend UI + backend support that lets a user swap any token they hold (SOL, PYUSD, USDG, etc.) into USDC to top up their USDC balance for Solana payments, using the Jupiter aggregator. This is the SWAP-TO-USDC funding flow; it is adjacent to the existing Solana one-off + recurring (USDC) payment work (see internal/modules/solana/pay.go, support.go, and the supported-tokens API in internal/http/handlers/solana_supported_tokens.go) but is its own feature.

## User problem + flow

A user wants to pay or subscribe in USDC (the preferred stablecoin; solanatokens.PreferredStablecoin) but holds value in another token — e.g. $100 of SOL. Today the supported-tokens API (GET supported tokens) returns each token's balance + a fiat->token quote and a `preferred`/`recurring_eligible` flag, but there is no way to CONVERT a non-USDC holding into USDC. The flow we are adding: user opens a "Top up USDC" panel -> sees their held balances (from the existing wallet-balance fetch) -> picks a source token + an amount (or a target USDC output) -> we fetch a Jupiter quote -> show estimated USDC output, price impact, slippage, and minimum received -> user confirms -> we return the Jupiter swap transaction -> THE USER signs and submits it from their own wallet -> their on-chain USDC balance is topped up. The supported-tokens API re-fetches and the new USDC balance is reflected.

## Jupiter integration

Jupiter is Solana's standard swap aggregator. Two calls drive the flow: (1) the Quote API (GET /quote with inputMint, outputMint=USDC mint EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v, amount, slippageBps, and routing filters) which returns the best route, out amount, price impact, and per-leg market info; and (2) the Swap API (POST /swap with the quote response + the user's wallet pubkey) which returns a base64 serialized (versioned) transaction. CRITICAL — consistent with the rest of this project (see pay.go: the user signs and submits Solana Pay transfers; there is no fee-payer delegation): the USER signs and submits the Jupiter swap transaction and THE USER pays the gas/priority fee. We do NOT set ourselves as feePayer and do NOT co-sign. The backend is a thin authenticated proxy/helper around Jupiter (to inject routing/dex preferences, slippage guards, and our USDC mint) plus optional quote caching; it never holds keys or custody. We should reuse the existing token registry (internal/modules/solana/tokens) for the USDC mint + decimals and respect mainnet vs devnet (solanatokens.DefaultDevnetTokens) — note Jupiter routing liquidity is mainnet-centric, so devnet may be a stubbed/quote-only path.

## Orderbook-preference design (prefer orderbook venues over AMMs)

The product owner prefers orderbook-based execution (RFQ / limit- or market-order venues) over Uniswap-style constant-product AMM liquidity pools where possible. Jupiter can route across BOTH, so we express the preference via Jupiter's routing controls: use the `dexes` inclusion allowlist to bias toward true on-chain orderbook DEXs on Solana — notably Phoenix and OpenBook (OpenBook V2) — and consider Jupiter's RFQ / order-flow (Metis routing, Jupiter Z / RFQ) for market-maker fills. Other relevant knobs: `restrictIntermediateTokens=true` (route only through liquid intermediates, reduces exotic AMM hops), `onlyDirectRoutes` (single-hop, often lands on a single orderbook venue), and `excludeDexes` to drop specific AMM pools. The preference must be SOFT/configurable, not a hard requirement: if no orderbook route exists or an orderbook route is materially worse (price impact / output) than the best available route, we fall back to the aggregator's best route rather than failing the swap — surfacing which venue(s) were used. Expose this as backend config (an allowlisted-dexes list + an orderbook-preference toggle) so it can be tuned without code changes.

## Slippage / price-impact handling and limits

Slippage is expressed in basis points (slippageBps) and must be bounded server-side (e.g. a sane default like 50 bps and a hard max so the client cannot request an abusive value). The Quote response carries `priceImpactPct` and `otherAmountThreshold`/minimum-out; we surface price impact and the minimum-received USDC, and reject or warn when price impact exceeds a configured ceiling. The swap transaction must carry a concrete minimum-output so a worsening market causes the on-chain swap to revert rather than fill at a bad rate. Quotes are short-lived — stamp a `quoted_at`/`expires_at` (the existing flow uses ~15m for Pay quotes, but swap quotes should be much shorter, e.g. 30-60s) and refuse to build a swap tx from a stale quote.

## Failure handling + min-output

Handle: insufficient source balance (cross-check against the wallet balance the supported-tokens flow already fetches); no route found; price impact over ceiling; stale/expired quote; partial fills (some venues/RFQ can partially fill — decide whether to allow partial top-up or require full fill and reflect that in the min-output); and on-chain failure/revert (return a clear error and let the user retry with a fresh quote). The user pays gas, so a failed/reverted swap costs them only the network fee, never custody risk.

## Ties into existing supported-tokens API + quote flow

Reuse: the wallet-balance fetch and per-token balance/quote shape in solana_supported_tokens.go; TokenPriceProvider + CalculateTokenQuote (support.go) for a sanity USD reference on the swap (compare Jupiter's implied price against our Pyth price to flag suspicious quotes); solanatokens.PreferredStablecoin / IsStablecoin and the USDC mint from internal/modules/solana/tokens. The new swap endpoints live alongside the Solana module (internal/modules/solana) with their own handler(s), mirroring how GetSupportedTokens and GeneratePayment are structured.

## UX

A "Top up USDC" panel: lists the user's held token balances (excluding USDC itself), lets them pick a source token and enter either an input amount or a desired USDC output, then shows a live quote preview — estimated USDC received, price impact, slippage (bps, editable within bounds), minimum received, and the venue(s)/route used (highlighting when an orderbook venue is used). A single Confirm button triggers the wallet sign+submit. Show pending/confirmed/failed states and refresh balances on success.

## Security / edge cases

Slippage abuse (clamp bps server-side, reject excessive values); stale quotes (short TTL, server-side freshness check before building the tx, and bind the quote to the requesting user + wallet); partial fills (explicit min-out semantics); MEV / sandwiching (tight slippage, prefer orderbook/RFQ which is less MEV-exposed than AMM pools, and rely on the user's wallet for priority fees); quote tampering (the swap tx is built by Jupiter from the exact quote we proxied — re-validate inputMint/outputMint/amount/min-out before returning it); and never logging or storing wallet private keys (we never see them — user signs locally).

## Open questions

- Should we also support topping up OTHER stablecoins (USD1/PYUSD/USDG) or USDC only for v1? (USDC only recommended for v1.)
- Allow target-output mode (specify desired USDC out, solve for input) or input-only for v1?
- Do we run our own Jupiter API host / self-hosted Metis, or use the public Lite/Pro API (rate limits, key management)?
- Partial fill policy: allow partial top-up or require full fill?
- Devnet story: stub/quote-only vs a devnet swap path (Jupiter liquidity is mainnet-centric).
- Exact orderbook-preference fallback threshold (how much worse an AMM-inclusive route may be before we prefer it over an orderbook-only route).

**Tasks:**
- BACKEND:
- [ ] Add a Jupiter client in internal/integrations (or internal/modules/solana) wrapping the Quote API (GET /quote) and Swap API (POST /swap), with configurable base URL + optional API key
- [ ] Add backend config: USDC output mint (reuse internal/modules/solana/tokens), default slippageBps, max slippageBps, max price-impact ceiling, quote TTL, orderbook-preference toggle, and an allowlisted-dexes list (Phoenix, OpenBook/OpenBook V2)
- [ ] Implement routing/dex-preference logic: bias Jupiter toward orderbook venues via `dexes` allowlist + restrictIntermediateTokens/onlyDirectRoutes, consider RFQ/Metis order-flow, with soft fallback to best route when no/worse orderbook route exists
- [ ] Add POST quote endpoint (authenticated): inputs source token symbol/mint, amount, slippageBps; returns USDC out, price impact, min received, route/venues, quoted_at/expires_at
- [ ] Add POST swap endpoint (authenticated): takes a fresh quote + user wallet pubkey, returns the base64 serialized swap transaction for the USER to sign and submit (no fee-payer delegation, no co-sign)
- [ ] Implement slippage guards (clamp bps server-side), price-impact ceiling rejection, min-output enforcement, and stale-quote rejection (bind quote to user+wallet, short TTL)
- [ ] Cross-check requested input amount against the wallet's held balance; sanity-check Jupiter's implied price against TokenPriceProvider/CalculateTokenQuote (Pyth) and flag divergence
- FRONTEND:
- [ ] Build the "Top up USDC" panel that lists held token balances (excluding USDC) sourced from the supported-tokens API
- [ ] Add source-token selector + amount input (input-amount mode for v1), wired to the quote endpoint
- [ ] Render quote preview: estimated USDC received, price impact, editable slippage (within server bounds), minimum received, and route/venue(s) used (highlight orderbook venues)
- [ ] Add a Confirm button that requests the swap tx, has the user's wallet sign + submit it, and shows pending/confirmed/failed states
- [ ] Refresh held + USDC balances on success and surface clear errors (no route, price-impact too high, expired quote, insufficient balance, on-chain revert)
- INTEGRATION:
- [ ] Wire the panel to the existing supported-tokens API (balances + preferred/recurring_eligible) and reuse the USDC mint/decimals from internal/modules/solana/tokens
- [ ] After a successful swap, reflect the topped-up USDC balance in the supported-tokens / balance view used by the Solana payment flow
- [ ] Respect mainnet vs devnet token config (DefaultDevnetTokens); decide + implement the devnet behavior (stub/quote-only vs swap path)
- VERIFICATION:
- [ ] Unit tests for slippage clamping, price-impact ceiling, min-output, stale-quote rejection, and orderbook-preference routing/fallback selection
- [ ] Unit tests for the Jupiter client request building (dexes allowlist, restrictIntermediateTokens, onlyDirectRoutes) and response parsing (priceImpactPct, otherAmountThreshold)
- [ ] Handler tests for the quote + swap endpoints incl. auth, balance cross-check, and error paths (no route, expired quote, insufficient balance)
- [ ] Integration test that hits the real Jupiter quote API (devnet/mainnet) for a SOL->USDC quote and asserts a sane route + USDC out + min received
- [ ] Frontend test/dogfood of the Top-up-USDC panel: balances render, quote preview updates, confirm triggers sign+submit, balances refresh on success

---

# #276: solana-recurring-analytics

**Completed:** no

DEFERRED (analytics, work later). Analytics surfaces for recurring Solana (the remaining #258 analytics tasks): active recurring subs, MRR (USDC/USD1 at $1 peg), failed pulls, recovered-vs-churned, cranker gas spend. Admin endpoints + dashboard wiring, consistent with the existing Stripe/NMI metrics. Depends: #258.

**Tasks:**
- [ ] active recurring Solana subs + MRR per merchant (sum plan amounts at $1 peg)
- [ ] failed-pull / dunning metrics; recovered vs churned
- [ ] cranker gas spend per merchant (surface alongside the gas-alert #258)
- [ ] admin endpoints / dashboard wiring matching the card-processor metrics shape

---

# #292: openrails-helm-charts-k8s-deploy

**Completed:** no

Make OpenRails trivial to deploy on Kubernetes via OFFICIAL Helm charts, targeting BOTH self-hosted k8s (bare/on-prem clusters) AND managed k8s (DigitalOcean DOKS first, then GKE/EKS/AKS). Today deployment is docker-compose + a hand-rolled image; there is no first-class k8s story. A clean Helm chart is the substrate for both self-hosters and our own managed SaaS (see [[openrails-managed-saas]] / #293).

## Scope
Package the whole stack as a configurable chart: the OpenRails app (HTTP API + the embedded River workers for crank/dunning/reconcile), DB (in-cluster Postgres OR point at an external/managed DB), Redis/Garnet, ClickHouse (analytics, optional/toggleable), the migrations job, secrets (k8s Secrets / Vault / sealed-secrets), ingress + TLS, autoscaling, and config via values.yaml. Self-hosted vs managed differ mainly in whether DB/Redis/CH are in-cluster or external managed services — express both via values.

## Why DigitalOcean specifically
DOKS is cheap + popular with the indie/self-host audience; ship a DOKS quickstart (DO managed Postgres + Redis, DO load balancer + cert) and evaluate a DO Marketplace 1-click app. Keep the chart cloud-agnostic so GKE/EKS/AKS are just different values.

**Tasks:**
- [ ] Chart skeleton (deployment for the API, a separate deployment/args for the River workers, service, configmap, secret, HPA, PDB, serviceaccount); helm lint + a CI render test.
- [ ] values.yaml profiles: self-hosted (in-cluster Postgres/Redis/ClickHouse subcharts) vs managed (externalDatabase/externalRedis/externalClickHouse pointing at DO/GCP/etc managed services). ClickHouse/analytics fully optional (toggle).
- [ ] Migrations as a pre-install/pre-upgrade Job (helm hook) running the OpenRails migrator; gate the app start on it.
- [ ] Secrets: support k8s Secrets, Vault (the existing vault KV/transit adapter #251), and sealed-secrets/external-secrets; document the per-merchant DEK/secret-store wiring.
- [ ] Ingress + TLS via cert-manager (Let's Encrypt) + an nginx/traefik example; configurable host, CORS allowlist, and the public base URL the Solana Pay endpoints need.
- [ ] Resource requests/limits + HPA (CPU/RAM, and a worker-queue-depth custom metric later); readiness/liveness probes on the health endpoint.
- [ ] DigitalOcean DOKS quickstart: DO managed Postgres + managed Redis + DO LB; a values-do.yaml + a runbook; evaluate a DO Marketplace 1-click listing.
- [ ] Publish the chart to an OCI registry (ghcr) + a versioned helm repo; pin the OpenRails image tag; document upgrade/rollback.
- [ ] Docs: 'deploy OpenRails on k8s in 10 minutes' for self-hosted and for DOKS; production hardening notes (DB backups, Redis persistence for the Solana Pay pending set, secret rotation).

---

# #293: openrails-managed-saas

**Completed:** no

Stand up a HOSTED OpenRails SaaS where a user signs up and spins up their own OpenRails in a few clicks — no infra to run. THIS IS THE MAIN BUSINESS MODEL. Self-hosting stays free/OSS; the managed service is the commercial offering: we run, scale, monitor, and upgrade OpenRails for customers who just want billing-as-a-service.

## What it is
A control plane + dashboard on top of OpenRails' existing multi-merchant foundation: signup -> provision a merchant (or an isolated instance) -> the customer gets API keys + a dashboard to configure products/prices, connect their processors (Stripe/NMI/CCBill/Solana), and watch revenue. We lean on the multi-merchant work already built — RLS isolation + per-merchant secret store / DEK (#227/#259), federated/delegated tokens, the catalog-as-code apply, the de-embed/standalone deploy (#249). The Helm charts (#292 / [[openrails-helm-charts-k8s-deploy]]) are the deploy substrate for the managed pods (and keep self-host parity).

## Isolation model (decide early)
Two tiers: (a) SHARED multi-merchant cluster with RLS + per-merchant DEK (cheap, for small customers / free tier), and (b) DEDICATED per-merchant instance/namespace (for larger customers needing hard isolation / their own DB). Same image + chart, different provisioning.

## Meta-billing (the fun part)
We bill our SaaS customers... using OpenRails itself (dogfood): usage metering (API calls / active subscriptions / GMV %), pricing tiers, free tier, Stripe for the SaaS subscription. OpenRails-billing-OpenRails.

**Tasks:**
- [ ] Signup + onboarding: create an account -> provision a merchant (RLS row + per-merchant secret store/DEK) -> issue API keys + a delegated dashboard token. Self-serve, < 5 min to first test charge.
- [ ] Control-plane dashboard (web): manage products/prices (catalog-as-code #162 under the hood), connect processors (Stripe/NMI/CCBill/Solana keys into the per-merchant secret store), view subscriptions/revenue/dunning, manage webhooks.
- [ ] Isolation tiers: (a) shared cluster + RLS + per-merchant DEK for free/small; (b) dedicated instance/namespace via the Helm chart (#292) for enterprise. Provisioning automation for both.
- [ ] Meta-billing: dogfood OpenRails to bill SaaS customers — usage metering (active subs / API volume / optional GMV %), pricing tiers + free tier, Stripe for the SaaS plan, dunning on our own customers.
- [ ] Usage metering + quotas + rate limiting per merchant; surface usage in the dashboard; enforce plan limits.
- [ ] Scaling + ops: per-merchant/queue autoscaling, monitoring/alerting (cranker gas, failed pulls, webhook lag), backups, on-call runbooks, status page.
- [ ] Migration paths: self-host -> managed import, and managed -> self-host export (no lock-in; OSS core is the trust anchor).
- [ ] Security/compliance posture for handling processor keys + payment data at scale (secret rotation, audit log, SOC2 trajectory, PCI scope minimization since processors hold the PANs).
- [ ] Pricing + positioning: free tier (self-host parity), usage-based paid tiers, the 'Stripe-like billing infra you actually own, hosted for you' pitch.

---

# #297: deplatforming-resilient-card-vault

**Completed:** no

Make OpenRails' CARD billing survive a processor/vault de-platforming — the existential threat for the adult/high-risk merchants OpenRails serves (host apps). The card-side analog of the Solana rail (which is already un-de-platformable: no vault, no PCI, no acquirer).

SCOPE NOTE (2026-07-01): keep this issue about DEPLATFORMING RESILIENCE only. The Link/Shop-Pay-style
cross-merchant wallet is a DIFFERENT motivation with a different chosen path — Stripe-Connect saved-PM
reuse on the platform account (see ~/openrails-saas/payments-multi-merchant-wallet-and-payfac.md, Rank 1:
rented PCI, no MTL) — not this vault. If this neutral vault is ever shared across merchants, the
wallet-integrity constraint applies (openrails-saas #23: shared vault and shared subscription state must
be inseparable — no merchant-controlled state machine may charge shared-vault instruments). Under the
two-party platform, vault entries here would be customer-scoped, not (customer, merchant)-scoped
(Roadmap v0.2), which makes that constraint load-bearing.

## Strategy (decided 2026-06, after researching the vault market)
Keep cards in a NEUTRAL, EXPORTABLE third-party vault; OpenRails stays the orchestration/billing brain and NEVER sees the PAN. Architecture:
  Browser --(vault SDK/iframe, PAN bypasses OpenRails)--> VAULT (holds card + network token)
  OpenRails --("charge token X, $Y, off-session")--> VAULT --> high-risk acquirer (CCBill / NMI-high-risk / Epoch / Verotel ...)
PCI: because the PAN never touches OpenRails' servers, OpenRails/the merchant stays at SAQ A (a checklist), NOT PCI Level 1 (audits). The vault vendor carries PCI L1 + the breach liability. If OpenRails ever RECEIVES the raw PAN (even unstored) it jumps to SAQ D; if it STORES PANs itself it's PCI Level 1 — both to be avoided.

## Vendor decision: Spreedly (with caveats)
Researched Spreedly vs VGS vs Hyperswitch Cloud on (adult/de-platform trigger, export-on-exit, self-host):
- SPREEDLY = best fit: neutral infra (not judging content), most high-risk-tolerant, and the STRONGEST contractual export — documented self-service SFTP/JSON/PGP export to a PCI-DSS-compliant destination + a defined 30-DAY post-termination window. Cloud-only (no self-host), but the export right offsets lock-in. Caveat: broad 'unacceptable level of risk' suspension clause -> confirm adult stance + the export SLA contractually.
- VGS = content-neutral too, BUT its standard T&C grants NO export right ('may, but not obligated to, delete') -> export must be negotiated in. Weaker for this use case.
- HYPERSWITCH CLOUD = WORST for adult: ToS lets Juspay terminate IMMEDIATELY if the business is 'restricted or cautioned by card networks/acquirers' (adult IS network-restricted) -> a trapdoor for adult; and its export is a TEAM-ASSISTED process dependent on Juspay's cooperation (no clear for-cause survival). Hyperswitch's only real value here is that it's OSS + vault-agnostic (bring-your-own-vault: connect VGS/TokenEx) -> use it (if at all) as a router over an external vault, not as the vault. But OpenRails IS already the orchestration, so Hyperswitch's router is redundant.

## Why Spreedly + OpenRails (not Hyperswitch + Chargebee)
OpenRails already owns the billing brain (subscriptions, dunning, entitlements, proration) + the charge across processors. So the only missing piece for card portability is the vault. Spreedly slots in as 'the vault OpenRails commands'; no second billing vendor (Chargebee) or redundant router (Hyperswitch) needed.

## Phasing (decided 2026-06)
- NOW: processor-direct — Mobius/NMI (cards) + CCBill + Solana, OpenRails as orchestration. Simple, low-PCI (SAQ A), works today. INTERIM RISK to accept knowingly: card tokens are vaulted AT the processor and are NON-PORTABLE, so if a card processor de-platforms us TODAY the saved cards are STRANDED (we'd have to re-collect from every user). Only Solana is de-platform-resilient in this phase; the card side is not.
- LATER: slot Spreedly in as the neutral card vault -> cards become portable -> full card-side de-platform resilience, still at SAQ A, OpenRails unchanged as the brain. This issue is that 'later' work.
- TRIGGER to prioritize this issue: if any of Mobius/NMI/CCBill becomes shaky or signals de-platform risk, pull this forward (the interim card-lock-in is the exposure). Absent that, it can wait behind the usage-billing engine work.
## Precision (so the model is exact)
Spreedly sits IN FRONT OF the gateways — it holds the card + presents it to whichever gateway OpenRails picks; it is NOT itself a money-mover and does NOT replace the gateway/acquirer relationships. The win is gateway FLEXIBILITY: because the card lives in Spreedly (not in Mobius), we can swap/add gateways and route the SAME saved card to them without re-collecting from users.

## Export destinations if a vault de-platforms us (decided 2026-06)
Any export target must be PCI-DSS-compliant and provide a PCI AoC (the migration is vault-to-vault; we never touch PANs). Two tiers:
- STAY SAQ A: migrate to another managed neutral vault — TokenEx / VGS / Basis Theory (all neutral, portability-oriented; the new vault holds the PCI scope). Pre-provision one (TokenEx or VGS) as the standing failover and test the migration.
- PCI LEVEL 1 break-glass: self-hosted Hyperswitch vault (or Basis Theory on-prem) — only if we make it PCI-L1 with an AoC; this takes on the heavy regime, so it's the fallback-to-the-fallback, used only if we want to OWN the vault.

**Tasks:**
- [ ] Integrate a vault (Spreedly) via its CLIENT SDK/iframe so the PAN goes browser->vault and NEVER reaches OpenRails -> keep OpenRails at SAQ A. OpenRails stores only the vault token reference (mirror the existing token-reference posture).
- [ ] OpenRails charge path: 'charge vault token X, amount Y, off-session/MIT' -> vault de-tokenizes + routes to the configured high-risk acquirer. A new processor type ('vaulted-card via Spreedly') behind the planned narrow-processor-interface.
- [ ] Use NETWORK TOKENS (Visa VTS / MC MDES via Spreedly) for portability + higher auth rates + surviving card re-issuance (recurring doesn't break).
- [ ] CONTRACT: negotiate an export SLA that explicitly SURVIVES for-cause termination (not just voluntary exit); confirm Spreedly's adult-content stance in writing.
- [ ] FAILOVER PREP + DESTINATIONS: pre-provision a SECOND managed neutral vault account NOW (idle standby) as the export destination, and TEST the vault-to-vault migration once. EXPORT TARGETS (all must provide a PCI AoC to receive the migration):
      - PREFERRED (stay SAQ A): another managed neutral vault — TokenEx, VGS, or Basis Theory. The new vault carries PCI; OpenRails just re-points token refs (token-remap). The portable ecosystem is Spreedly <-> VGS <-> TokenEx <-> Basis Theory.
      - BREAK-GLASS (accept PCI Level 1): a SELF-HOSTED vault (self-hosted Hyperswitch locker, or Basis Theory on-prem) — only works if we make it PCI-DSS Level 1 compliant WITH an AoC (Spreedly only exports to a PCI-compliant destination), which flips us into PCI L1. Continuity option of last resort, not the default.
- [ ] TOKEN-REMAP design: a vault swap issues new token IDs -> OpenRails' vault/processor layer must consume an old->new token mapping and re-point references. Design the layer so 'swap vaults' = a remap step, not a re-architecture.
- [ ] Maintain standing relationships with multiple ADULT/HIGH-RISK acquirers (CCBill, NMI-via-high-risk-bank, Epoch, Verotel/Vendo) so the vault can route A->B->C if one acquirer drops you.
- [ ] Document the two-layer resilience posture for ops + sales: card payers ride a portable/exportable vault (move on 30 days' notice); crypto payers ride Solana (no vault, no PCI, no acquirer, nothing to de-platform).
- [ ] NOT to do (record the rationale): do NOT receive raw PANs in OpenRails (->SAQ D), do NOT self-host a PAN vault (->PCI Level 1 + breach liability), do NOT depend on Hyperswitch Cloud's vault for adult (termination trapdoor + cooperation-dependent export).
- [ ] TRIGGER/sequencing: prioritize this work if any card processor (Mobius/NMI/CCBill) signals de-platform risk (the interim phase leaves card tokens processor-locked = stranded-on-ban). Otherwise sequence behind the usage-billing engine (#289).

---

# #305: fast-eventual-consistency-admission

**Completed:** no

OPTIONAL future optimization: a FAST (eventual-consistency) admission mode for extreme per-payer throughput. TODAY the money axis is STRONG consistency: admission.Admitter calls credits.AuthorizeAndHold, a Postgres tx with SELECT ... FOR UPDATE on the payer balance row, so requests for one payer serialize on the lock and read the exact committed balance (zero overspend, but throughput bounded by lock-serialized txns + a DB round-trip per request). The THROUGHPUT axis is already Redis-fast in both modes.

FAST mode (this issue): per-request money decision against a Redis/Garnet HEADROOM counter (= available balance + remaining credit line), O(1), no lock, no DB round-trip; the durable Postgres debit is WRITE-BEHIND (async) and the Redis counter is RECONCILED from the authoritative PG balance periodically. Sub-ms at scale. BOUNDED OVERSPEND: atomic Redis decrement (no concurrent oversell) + cap = reconcile-lag x spend-rate, hard-capped by the credit limit. Per-host CONFIGURABLE strict|fast. Phase-2 host-side LEASE: the host reserves N units from OpenRails and spends locally, removing the per-request network hop; overspend bounded by lease size.

WHEN TO BUILD: only when a single payer needs hundreds+ req/s where the per-payer FOR UPDATE serialization or the admission round-trip becomes the bottleneck. Until then strict is plenty. Source: internal/modules/admission + the #298 latency design.

**Tasks:**
- [ ] CONFIGURABLE consistency per host/merchant (optionally per endpoint/credit_type): STRICT (sync AuthorizeAndHold, exact, zero overspend, higher latency) vs FAST (Redis headroom, eventual, bounded overspend, sub-ms). Default STRICT; opt into FAST for high-QPS.
- [ ] FAST PATH: admission = one Redis/Garnet op (throughput windows + cached money-headroom counter, atomic decrement); NO Postgres lock/round-trip per request. Keep strict FOR-UPDATE AuthorizeAndHold for low-QPS/exact callers.
- [ ] WRITE-BEHIND + RECONCILE: durable PG debit + usage_event async via host RecordUsage (#289); periodically resync the Redis headroom from the authoritative PG balance.
- [ ] BOUNDED OVERSPEND: atomic Redis decrement (no concurrent oversell) + credit_limit cap + reconcile-lag bound (throughput caps spend-rate); make the tolerance tier-tunable.
- [ ] OPTIONAL phase 2: host-side lease (reserve N units, spend locally, reconcile) to remove the per-request network hop; overspend bounded by lease size.

---

# #306: billing-admission-future-refinements

**Completed:** no

Deferred refinements to the usage-billing/admission system (core shipped + live-validated). None blocking; each is a small, well-scoped follow-up.

- MERCHANT->OWNER limit level (#304): admission enforces owner->actor budgets/throughput today; add the second level so a MERCHANT can cap each OWNER (tensorhub caps cozy), via the SAME admitter logic at a merchant-scoped key + merchant-level tier policy (owner sentinel). The budgets/limiter engines already support arbitrary scope keys.
- BUDGET CAPTURE tie-to-ledger (#304): on ledger CaptureHold, also budgets.Capture the matching reservation so reserved->used converts immediately. NOT required for correctness (the rolling window self-heals: an un-captured reservation ages out of the window), so it's an accuracy nicety.
- /v1/me delegated budget introspection (#304): a browser-token variant of merchant budget reads. Redundant while hosts proxy the service endpoint; add if browsers should read budgets directly.
- usage_events MONTH PARTITIONING (#289): partition billing.usage_events by month for scale. Wrinkle: the idempotency unique index (merchant,owner,event_type,source,source_id) must include the partition key, weakening cross-month dedup (acceptable since source_ids are request/time-scoped). Premature until event volume demands it.
- INVOICE admin list + CSV export (#303): an admin (cross-owner) invoice list + CSV download. Self endpoints (GET /v1/me/invoices[/:id]) cover the customer need; admin/CSV on demand. (PDF rendering intentionally NOT planned.)
- tensorhub->OpenRails platform_policies ownership migration (#304): cross-repo move of tensorhub's tier policies onto OpenRails' tier_policies; do when consolidating.

**Tasks:**
- [ ] Tier policy carries money caps (credit_limit/monthly) + graduation applies them to account settings (auto credit-limit by tier).
- [ ] Arrears authorize gate: combine prepaid balance + remaining credit line as headroom (currently line-only; conservative).
- [ ] MERCHANT->OWNER admission level (tensorhub caps cozy) via merchant-scoped tier policy + admitter check.
- [ ] Tie budgets.Capture/Release to ledger CaptureHold/ReleaseHold (accuracy; window self-heals without it).
- [ ] /v1/me delegated budget introspection variant.
- [ ] usage_events month partitioning (include partition key in the idempotency index).
- [ ] Invoice admin (cross-owner) list + CSV export.
- [ ] Migrate tensorhub platform_policies -> OpenRails tier_policies (cross-repo).

---

# #129: direct-card-processing

**Completed:** no

Accept credit card details directly on our server instead of proxying through Mobius/NMI's hosted tokenization

## Metadata

- Category: architecture
- Priority: low
- Status: not_started
- Passes: false

## Details

- context: {"current_flow":["1. Frontend collects card info in Mobius-hosted iframe/form","2. Card data sent directly to Mobius/NMI (never touches our server)","3. Mobius returns a payment token or customer_vault_id","4. Our server uses token to charge via NMI API"],"proposed_flow":["1. Frontend collects card info in our own form","2. Card data POSTed to our server","3. Our server sends card data directly to NMI API","4. Store customer_vault_id for future charges"],"why_consider":["Full control over checkout UX (no iframe styling limitations)","Faster checkout (no redirect to third-party)","Can implement custom validation and error handling","Reduce dependency on Mobius-specific integration"]}
- nmi_api_parameters: {"direct_card_fields":["ccnumber - Credit card number","ccexp - Expiration date (MMYY format)","cvv - Card security code"],"with_customer_vault":["customer_vault=add_customer - Store card in vault","Returns customer_vault_id for future charges"]}
- pci_compliance_requirements: {"current_level":"SAQ A or SAQ A-EP (card data never touches our servers)","required_level":"SAQ D or full PCI DSS assessment","key_requirements":["Encrypt card data in transit (TLS 1.2+) - already done","Never store full card numbers (only last 4 + token)","Never log card data","Quarterly vulnerability scans by ASV (Approved Scanning Vendor)","Annual penetration testing","Secure coding practices audit","Network segmentation for cardholder data environment","Access controls and audit logging","Incident response plan"],"cost_estimate":{"asv_quarterly_scans":"$100-500/quarter","annual_pentest":"$5,000-20,000/year","pci_assessment":"$15,000-50,000/year for SAQ D","ongoing_compliance":"Significant engineering time for documentation and controls"}}
- recommendation: {"verdict":"Probably not worth it for current scale","reasoning":["PCI SAQ D compliance is expensive and time-consuming","Hosted tokenization (current approach) is industry standard","Mobius iframe can be styled reasonably well","Security risk/liability not worth UX improvement"],"reconsider_when":["Processing >$1M/month and UX is measurably hurting conversion","Mobius hosted form has significant technical limitations","We hire dedicated security/compliance staff"]}
- risks: ["PCI compliance burden and ongoing costs","Security liability if breached (card data exposure)","Mobius may not support direct integration (contractual requirement for hosted form)","More attack surface on our servers"]

**Tasks:**
- IMPLEMENTATION:
- [ ] Determine if Mobius Pay allows direct API integration (vs requiring their hosted form)
- [ ] Review NMI direct card submission API parameters (ccnumber, ccexp, cvv)
- [ ] Implement server-side card validation (Luhn check, expiry, CVV format)
- [ ] Add endpoint: POST /v1/checkout/card with {card_number, exp_month, exp_year, cvv, ...}
- [ ] Ensure card data is never logged (scrub from request logs)
- [ ] Ensure card data is never stored (only vault token after successful charge)
- [ ] Pass PCI compliance assessment
- [ ] Update frontend to use direct form instead of Mobius iframe

---

# #127: durable-workflow-execution

**Completed:** no

Evaluate and potentially adopt embedded durable workflow execution for critical payment flows

## Metadata

- Category: architecture
- Status: on_hold
- Passes: false

## Details

- status_update: {"date":"2025-12-14","decision":"On hold - focus on simpler solutions first","reasoning":["Ad-hoc durable workflow system was built and then reverted","Durable workflows don't compensate for faulty code - unit tests do","Idempotency keys on NMI calls solve the double-charge problem more simply","If we need durable workflows later, use a proper library like go-workflows"],"prerequisite":"Implement wrap-nmi-calls-with-idempotency first, then reassess if durable workflows are still needed"}
- candidate_library: {"go_workflows":{"repo":"github.com/cschleiden/go-workflows","maturity":"Production-ready, supports PostgreSQL","when_to_adopt":["Multi-step flows that genuinely need crash recovery","Workflows with long-running waits (e.g., approval flows)","Complex saga patterns with compensating transactions"]}}
- candidate_workflows: ["Checkout flow (charge → record → grant entitlements)","Subscription upgrade (charge proration → cancel old → create new)","Dunning/rebill (charge → renew or → escalate failure)","Refund flow (refund at processor → revoke entitlements → update records)"]
- when_to_reconsider: ["After idempotency keys are implemented and we still see issues","When we add complex multi-step flows (e.g., multi-party payouts)","If reconciliation reports show frequent partial-completion issues"]

**Tasks:**
- ON HOLD - Reassess after implementing idempotency keys
- Previous ad-hoc implementation was reverted on 2025-12-14
- If needed later:
- [ ] Evaluate go-workflows with a simple PoC (non-critical path first)
- [ ] Create PoC: Wrap a simple workflow (e.g., refund) with go-workflows
- [ ] Benchmark: Measure overhead of event persistence
- [ ] Decide: Adopt go-workflows based on PoC results

---
# #351: optional managed cloudflared: expose a local OpenRails instance externally

**Completed:** no

Optionally RUN cloudflared (not just document it): a local/dev OpenRails instance gets a stable public hostname so external systems can reach it — primarily processor webhooks (NMI/Mobius, Stripe, CCBill) hitting a laptop or CI box, and host apps integrating against a dev instance. Context: the doc-only `cloudflared` config block (tunnel_token/tunnel_name/public_hostname) was removed in the #350 knob diet because OpenRails never ran it; if we ever DO run it, the config earns its way back. Filed per Paul 2026-06-11: "we presumably want cloudflared to run, so that a local instance can be made available externally by another system. Kind of optional."

Sketch: a `cloudflared` sidecar in the dev compose stack (image exists upstream; needs tunnel token) OR an opt-in supervisor goroutine that execs a bundled/system cloudflared with the configured token and logs the public hostname at boot. Compose-sidecar is likely the right shape — keeps the binary out of OpenRails and the knob count at zero (token lives in .env for compose only). docs/cloudflared-webhooks.md already documents the manual flow.

**Tasks:**
- [ ] Decide shape: compose sidecar (preferred) vs supervised child process
- [ ] Wire dev compose service + .env token passthrough; print the public webhook URL on boot
- [ ] Update docs/cloudflared-webhooks.md from manual steps to the supported flow

---

# #493: fold the product catalog into the platform-policy / platform-config umbrella

**Completed:** no

One declarative, CLI-refreshable config per platform. Today the OpenRails product
catalog (`internal/modules/catalog`: ProductService/PriceService, models.Product/
models.Price — what's sold + prices) is managed separately from the host's
platform policy, and tensorhub's `platform-config apply` only pushes the #486
capacity ladder. Goal: fold the catalog into the same umbrella so one config doc
(per platform) drives both, pushed idempotently (full-upsert + diff) the same way
the tier ladder is.

Constraint (Paul, 2026-06-14): doujins, hentai0, cozy-art are NOT API platforms —
they need NO api-spend platform policy (tiers / wasted-spend / in-flight caps /
allowed-endpoints / arrears). They need ONLY the product catalog (products +
prices for subscriptions/checkout). tensorhub IS the API platform and needs both
halves. So the unified per-platform doc has two parts, the governance half OPTIONAL:
  - `catalog:` (products + prices) — every platform.
  - `api_governance:` (capacity ladder #486 + tier/budget policy + allowed
    endpoints + arrears) — API platforms only (tensorhub).

This supersedes the earlier vague "per-stack config CLI for sibling stacks" idea:
non-API stacks just run the same `platform-config apply` with a catalog-only doc.

Design questions to resolve before building:
- Does a generic SDK/client setter exist for hosts to push products + prices
  (SetCatalog / UpsertProducts)? The catalog module is internal — confirm; if
  absent, add one mirroring SetTierSchedule/SetTierPolicy (idempotent full-upsert).
  Keep it generic — products/prices only, no host meaning.
- Unified config schema (one YAML: `catalog:` + optional `api_governance:`).
- Push targets: catalog -> OpenRails catalog; governance -> OpenRails admission +
  tensorhub.platform_policies. Extend `platform-config apply` to push both.

**Tasks:**
- [ ] Audit the catalog module for an existing host-facing setter; design one if missing
- [ ] Design the unified per-platform config schema (catalog + optional api_governance)
- [ ] Extend tensorhub `platform-config apply` to push the catalog section
- [ ] Author catalog-only configs for doujins/hentai0/cozy-art; full config for tensorhub

---

# #515: cross-currency settlement (convert / spend across a customer's currency wallets)

**Completed:** no — future direction only. Extracted from #512 Phase E (CLOSED there as not-needed under the current "each currency is its own wallet" model). Build only when a concrete use case appears.

## What this is (and is NOT)
Multi-currency **wallets already work**: a customer holds an independent balance per currency (USD/USDC/EUR/JPY/SOL), and **FX rate conversion for comparison already exists** (`internal/integrations/fx` `ExchangeAPIProvider`, live + free + no-key, used by admission/checkout/solana to evaluate a charge priced in currency A against a cap/budget denominated in currency B). This issue is NEITHER of those.

This issue is **settlement** = *moving actual money between a customer's wallets*: debit their EUR balance to pay a USD charge (or convert one wallet into another), recorded as ledger transfers. The difference from what exists: converting a *number* for a comparison (done) vs moving *money* between wallets (this).

## When we'd actually do it (triggers, increasing likelihood)
1. **Explicit convert** (user-initiated): customer holds $50 USD, hits "convert", we reprice into €X at spot, they now have a EUR balance.
2. **Auto cross-wallet spend**: customer has only a EUR balance, a USD charge arrives, we auto-draw EUR at spot to cover it.
3. **Crypto deposit → fiat credits** (the nearest real case): customer deposits SOL/USDC, we credit a USD balance at the deposit-time rate. The Solana pay path already quotes via `fx.ConvertAmount`, so this is the most plausible first consumer.

## Design — keep it minimal (NOT money-transmitter grade)
Spreads / FX gain-loss machinery exist for businesses that **hold a currency position and settle on a market later**. OpenRails balances are **prepaid credit we already hold** — when a customer converts $50→€44 and later spends it, we just *deliver €44 of service*; we never bought euros on a market. So for one-way fiat↔fiat conversion: **no spread needed for risk, no FX P&L to track.** A spread is only a *product choice* (charge a conversion fee), not a necessity.

**The one irreducible mechanic** (a ledger transfer can't cross currencies — the currency-guard trigger forbids it): a conversion is **two transfers through a per-merchant `fx_liquidity` account**:
```
DR customer EUR-wallet  €30  /  CR fx_liquidity (EUR)  €30
DR fx_liquidity (USD)   $33  /  CR customer USD-wallet $33
```
Each currency ledger still conserves on its own (EUR −30/+30; USD +33/−33). Sub-cent **rounding dust** (€30 at the real rate ≈ $33.0094, rounded to $33) lands in `fx_liquidity` — that's all the account is for in the simple case; it's where pennies go so the books balance, not a risk buffer. No P&L reporting unless we want it.

## Where real FX risk DOES appear (so we know the boundary)
- **Volatile assets held over time** (crypto deposit→fiat, #3): if we credit USD value at deposit and the held SOL drops before external conversion, that loss is real. Mitigation is usually "convert immediately at deposit," not a spread.
- **Round-trips** (€→$→€): rounding + any spread compounds, can leak value.
- **Delayed external settlement** (repatriating balances to a bank at a later rate).
None of these apply to "one-way convert at spot, each currency its own wallet."

## Tasks (when built)
- [ ] Decide explicit user-initiated convert (simpler, auditable — RECOMMENDED) vs implicit cross-currency spend at spot.
- [ ] Add an `fx_liquidity` account type; model conversion as two linked transfers through it (atomic), reusing the existing live `fx.Provider` for the rate; round the output to integer minor units.
- [ ] Record the quote (rate, AsOf, provider) on the conversion transfers for audit.
- [ ] Only if charging a conversion fee or holding volatile positions: add spread + FX gain/loss reporting. Otherwise skip.
- [ ] Custom credits remain no-FX by design (#475).
- [ ] **Persist the crypto FMV-at-receipt basis snapshot on the durable payment** (small; standalone-able). Today a confirmed SOL payment records only the fiat product amount + on-chain signature; the receipt quote (`token_amount`, `token_price_usd`, `quoted_at`, oracle source) lives only on the ephemeral Redis pending and is dropped at confirmation. Stamp it onto the `payments.metadata` jsonb (already exists) at confirmation so the merchant has a stored cost-basis record instead of reconstructing it from chain + a historical oracle. **Two distinct rates — store the ones that apply:** `token_price_usd` = SOL→USD (the crypto price; this alone gives the USD FMV/basis = `token_amount × token_price_usd`); `fx_rate` = USD→billing-currency, the *second* leg, only non-trivial when the product is priced in a non-USD fiat (EUR/JPY) — for a USD-billed merchant it's always 1.0 and redundant. Store `token_price_usd` always; store `fx_rate`/billing-currency only when billing ≠ USD (so the merchant can also record income in their books' currency). The durable form of this basis record is the #516 holdings-ledger lot.

## Relationship
- Closes the loop on **#512 Phase E** (which is marked CLOSED-as-not-needed there, pointing here).
- Reuses the **already-shipped** `internal/integrations/fx` provider (no new rate-source work).
- Downstream consumer: **#577 (coupons)** fixed-amount coupons are per-currency until this ships; once cross-currency conversion exists, a fixed-amount coupon defined in currency A could convert to charge currency B at spot.

---

# #516: crypto treasury policy — auto-convert / stop-loss / hold+stake on received crypto

**Completed:** no — future direction only.

Today a Solana payment lands in the recipient ("pull") wallet, the **fiat product price** is recorded as income (a `payments` row, with the on-chain signature as the audit anchor), and that's it — what happens to the received SOL afterward is entirely manual / off-platform. That leaves the merchant holding a volatile asset with no managed disposal, and it leaves the *capital* side of crypto tax (basis → disposal → gain/loss, the "Event 2" from the tax discussion) completely unmodeled. This issue adds a configurable **treasury policy** that automates the disposal decision per merchant (and per token), plus the holdings sub-ledger needed to track basis and realized gain/loss.

## Why
- **Risk:** volatile crypto held with no policy = uncontrolled FX/price exposure. The cleanest mitigation ("convert at receipt") and the smarter ones (stop-loss, stake-for-yield) should be a config knob, not a manual chore.
- **Tax/accounting:** receipt records income at FMV (≈ fiat price); the later disposal is a separate capital event. A holdings ledger that stamps **basis at receipt** and **proceeds at conversion** produces the merchant's realized gain/loss records automatically (see the tax reasoning in #515 / the FMV-at-receipt note).
- **Custody:** holding/staking means a second key. Receiving and long-term holding should NOT share one hot key.

## The three policies (per-merchant, ideally per-token)
1. **Immediate convert** — on confirmed receipt, swap SOL→USDC (and optionally off-ramp USDC→USD). Basis ≈ proceeds → ~zero capital gain/loss, no held position, no price risk. The "convert at deposit" mitigation; the simplest + safest default for risk-averse merchants.
2. **Stop-loss hold** — hold the SOL but anchor its **receipt-time price**; a background monitor auto-converts if the price falls ≥ X% below the anchor (e.g., 10%) to cap downside. Otherwise keep holding. (Optional symmetric take-profit ceiling — out of scope unless wanted.)
3. **Hold + stake** — sweep to a cold wallet and natively stake the SOL (stake account → validator) for yield; long-term hold. Manual or policy-driven unstake/convert later.

## Infrastructure this needs
- **Two-tier wallet model.** Keep the existing hot/receiving wallet (`recipient_wallet`, the "pull key"); add a **cold wallet** key for held/staked funds. Sweep hot→cold for anything not immediately converted. The cold key is high-value — store via the existing `internal/modules/vault`, and strongly prefer never holding it in app memory: offline/HSM/multisig signing for sweeps + stake ops. **This custody surface is the riskiest part of the issue — design it first.**
- **Holdings sub-ledger (treasury lots).** One row per received-and-held lot: token, amount, **basis (FMV/fiat at receipt)**, receipt signature/time, policy, anchor price (for stop-loss), status (`held | converting | converted | staked | unstaking`), and on disposal: proceeds, realized gain/loss, holding period (short/long). This is the durable record the payment row doesn't keep today (the receipt quote currently lives only on the ephemeral Redis pending — see #515 note). Fills the "Event 2" gap.
- **Swap execution.** SOL→USDC on-chain via Jupiter (**depends on / subsumes #260 solana-swap-to-usdc-via-jupiter**). USDC→USD bank off-ramp (if wanted) needs a CEX/off-ramp (Coinbase) — separate, custodial, KYC.
- **Price monitor.** Reuse the shipped Pyth price provider + `internal/integrations/fx` for the stop-loss trigger; a background poller compares live price vs each lot's anchor.
- **Staking ops** (policy 3 only): stake-account create / delegate / deactivate / withdraw; validator selection config. Heaviest, lowest-priority sub-part — could ship policies 1+2 first and defer 3.

## Decisions to make
- [ ] Swap venue: on-chain DEX (Jupiter — non-custodial, but slippage/MEV) vs CEX (better liquidity + fiat off-ramp, but custodial/KYC). Likely Jupiter for SOL→USDC, CEX only if USD off-ramp is needed.
- [ ] Anchor price granularity: per-lot (cleaner for tax + precise stop-loss) vs weighted-average across holdings. Recommend per-lot.
- [ ] Stop-loss execution realities: oracle-trigger vs actual fill price gap, slippage caps, partial fills, re-trigger debounce.
- [ ] Which tokens: USDC received needs no conversion (already a stable); target = SOL + other volatile tokens. Custom credits N/A.
- [ ] Finality: wait for Solana confirmation/finality before sweeping hot→cold.
- [ ] Policy scope: per-merchant default + per-token override? Where configured (catalog/treasury config)?

## Tasks (rough, sequence policies 1→2→3)
- [ ] Cold-wallet key model + Vault storage + secure sweep signing (design custody first).
- [ ] Treasury holdings sub-ledger (lots: basis, anchor, status, disposal → realized gain/loss).
- [ ] Policy config surface (per-merchant / per-token) + wiring into the confirmed-payment path.
- [ ] Policy 1 (immediate convert) via Jupiter swap (#260).
- [ ] Policy 2 (stop-loss): price-monitor poller off Pyth/fx + anchor + auto-convert.
- [ ] Policy 3 (hold + stake): cold-wallet staking ops.
- [ ] Surface realized gain/loss + holdings as a merchant report (ties to tax basis records).

## Relationship
- Builds on the Solana payment path (`internal/modules/solana` pay/poller/transaction) + the FMV-at-receipt income record.
- **Depends on / subsumes #260** (solana-swap-to-usdc-via-jupiter) for the swap primitive.
- Reuses the shipped Pyth price provider + `internal/integrations/fx`.
- Distinct from **#515** (that's converting between a *customer's* currency wallets; this is *merchant* treasury management of received crypto) but shares the swap/rate machinery.
- The holdings ledger's basis-at-receipt is the durable form of the "persist the quote snapshot on the payment" idea noted in #515.

---

# #517: Provider-absence tombstones for local-only provider mirror rows

**Completed:** no — future direction only.

Active #511 deliberately does **not** emit or repair `pull.*.excess`. A local payment/subscription/refund/dispute/vault
row that is not returned by a provider pull is not reliably wrong:

- the provider may not expose a complete all-time history;
- report/export endpoints may have retention windows or date-window limits;
- the pull may have failed pagination or used a partial current-state endpoint;
- the local row may be attributed to provider/account A while the pull checked provider/account B;
- old imports may have incomplete provider identifiers or copied/off-platform records.

This issue captures the later, stricter design if OpenRails ever wants to handle confirmed provider absence. Until then,
the reconcile/pull command should materialize missing provider facts and overwrite mismatched provider-owned fields, but
should ignore local-only provider mirror rows.

## Target Model

Provider absence is a verifier result, not a normal list-diff result. Only create `pull.*.excess` when a provider-specific
absence contract proves the local mirror cannot correspond to a real provider object.

If that bar is met, do not hard-delete and do not add generic `deleted_on` / `deleted_at` columns to provider mirrors.
Provider mirror rows are audit evidence. Add explicit tombstones instead:

- `invalidated_at timestamptz`
- `invalidated_reason text`
- `invalidated_by_finding_id uuid REFERENCES openrails.reconciliation_findings(id)`
- `provider_absence_confirmed_at timestamptz`
- `provider_absence_evidence jsonb`

For payments, an invalidated row is no longer a live source event for grants, entitlements, credits, invoice settlement,
or consistency math. DERIVE/LIFE/CON then clean up consequences through append-only paths.

## Questions To Resolve

- Which provider APIs can prove absence strongly enough?
  Stripe direct object lookup may be stronger than list absence; NMI reports may be weaker.
- What is the minimum evidence contract per provider/resource?
  Account fingerprint, provider id, endpoint, pagination, date window, request id, and direct lookup result likely matter.
- Should `pull.*.excess` exist at all, or should provider-absence verification only write tombstones plus operator notes?
- Which tables need tombstones first?
  Likely `payments`, then `payment_methods`; refund/dispute storage should wait until the concrete mirror tables are settled.
- How should historical imports with missing provider attribution be excluded from absence checks?
- Should this ever auto-apply, or should it always be operator-reviewed?

## Tasks

- [ ] Inventory provider capabilities for direct object lookup vs list/report absence:
      Stripe charges/refunds/disputes/payment methods/subscriptions, NMI/Mobius transactions/subscriptions/vault,
      CCBill transactions/subscriptions, and Solana signatures/references.
- [ ] Define a provider-specific absence authority contract.
- [ ] Add provider account fingerprint requirements to absence checks.
- [ ] Add tombstone columns to `payments` first if `pull.charge.excess` is approved.
- [ ] Add equivalent tombstone columns to `payment_methods` only if vault absence can be proven usefully.
- [ ] Defer refund/dispute tombstones until refund/dispute mirror storage is concrete.
- [ ] Ensure invalidated payments are excluded from live source-event queries used by grants, entitlements, credits,
      invoice settlement, and consistency math.
- [ ] Ensure invalidation never mutates balances directly; money corrections stay append-only.
- [ ] Add operator-facing evidence rendering for any provider-absence finding or tombstone.
- [ ] Add tests proving list/report absence alone does not tombstone a local row.
- [ ] Add tests proving wrong-provider/wrong-account pulls cannot tombstone a row.
- [ ] Add tests proving an invalidated payment stops acting as a live grant source while preserving audit history.

## Acceptance

- Active reconcile remains conservative: local-only provider mirrors are ignored unless a provider-specific verifier proves
  absence.
- No generic `deleted_on` / `deleted_at` columns are added for provider mirror rows.
- Tombstoned rows remain queryable as audit evidence.
- Invalidated payments no longer produce grants, entitlements, credits, invoice settlement, or consistency math inputs.
- All destructive consequences are produced by DERIVE/LIFE/CON through append-only repair paths, not by hard deletion.

---

# #523: Convergence Engine follow-ups (lower-priority tails split from #511)

**Completed:** no — follow-up tails de-scoped from #511 when its core landed (2026-06-18).

#511 (the unified Convergence Engine) is complete: all four planes (PULL/DERIVE/LIFE/CON) built + integration-tested, runs in-server (inline hooks on high-value mutation paths + a 15-min sweep), provider pull account-bound, audit + PS-* retired, grant ledger the single source of truth for creation AND revocation. These remaining items are genuine but lower-priority enhancements — NOT correctness gaps in the shipped engine. (Items deliberately NOT built because they are structurally unnecessary — the `*.mismatch` checks and webhook/checkout inline hooks — are documented in #511's completed entry and are NOT part of this issue. `derive.grant.missing` IS built, spec-aware.)

## Scope

1. **CON `consistency.amount_mismatch.*` subtypes.** The CON plane is bootstrapped (`consistency.reference.source_reference` + `consistency.duplicate.provider_charge`). Remaining qualified subtypes from `docs/consistency-invariants.md` §5:
   - `amount_mismatch.provider_catalog` (provider plan/price drift vs OpenRails catalog) and `amount_mismatch.refund_math` (refund totals exceed original charge) — both need **provider-observed data** from the PULL plane, so wire them onto the pull snapshot.
   - `amount_mismatch.invoice_math` — invoices use a DUAL representation (arrears `money_movements`/`owed_paid` + a separate Stripe `invoice_items`/`invoice_payments` flow). A cross-table check needs both flows mapped first to avoid false positives; the safe intra-row invariant (`amount_due = total − paid`) is already maintained by construction (≈zero value). Map the flows, then decide which (if any) cross-table check is reliable.
   - Extra `consistency.duplicate.*` (`invoice_usage`, `invoice_payment`) — same invoice-flow-mapping prerequisite.

2. **LIFE `grace_exhausted` → provider-cancel ACTION emission.** Today the LIFE pass terminates a grace-exhausted subscription LOCALLY (status flip + #264 Solana cranker cascade + as-of entitlement revoke, via the shared `ApplyLocalCancellation` core). The linked REMOTE provider-cancel (enqueue a `provider_intents` row to cancel the still-live remote subscription at NMI/Stripe) is not yet emitted — so a converged terminal cancel relies on the existing deferred-delete / caller paths for the remote side. Wire the provider-action emission into the grace_exhausted repair so the remote subscription is durably cancelled too.

3. **Pull "pure-overwrite" framing (cosmetic).** `pull-provider` already syncs the local mirror (missing→insert, mismatch→overwrite provider-owned fields) and emits four-plane `pull.*` slugs, but internally it still runs the legacy diff+selectively-apply machinery rather than a literal "pull head → overwrite mirror" rewrite. Purely cosmetic (behavior + taxonomy are already correct); reframe only if the engine is otherwise touched. The PS-8/9/10 checks also still physically live in the pull engine (they emit the correct plane's slug) rather than inside the Converge CON/DERIVE/LIFE passes — physical relocation only, no behavior change.

## Notes

- None of these block the engine's correctness: access is read off entitlement windows, money off the ledger, and the sweep + inline hooks already converge all drift. These improve coverage breadth (CON), durability of the remote-cancel side-effect (LIFE action), and code tidiness (pull framing).
- Design reference: `docs/consistency-invariants.md` §5 (the check catalogue) + §9 (construction-guaranteed invariants that are intentionally NOT runtime checks).

---

# #576: pluggable tax / VAT (host-supplied TaxCalculator + invoice tax lines + VAT-ID capture)

**Completed:** no — planned.

OpenRails has **zero** tax support today (grep: the only `tax` references are passive parsing of provider webhook payload fields in `internal/modules/webhooks/types.go`; no tax rates, no calculation, no tax line items, no VAT-ID handling). For our headline verticals this is a **legal** gap, not just a feature gap: EU VAT-MOSS on digital/adult goods is mandatory, and high-risk merchants are precisely the ones who cannot quietly skip it. Origin: billing-system design comparison vs Lago + UniBee (2026-06-24).

## Design principle

Do **not** build Avalara/Anrok/Stripe-Tax in core. Follow OpenRails' established "bring your own X via interface" philosophy (`billingauth.Authenticator`, `DeliverEmail`): add a host-supplied **`TaxCalculator`** seam, keep core thin, let the host wire a real engine or native rate tables. Lago integrates Anrok/Avalara + VIES exactly this way; UniBee has a pluggable `VATGateway` (VatSense/Vatstack). Default = no calculator wired → no tax (today's behavior is unchanged; tax is opt-in).

## Target Model

- A framework-neutral `TaxCalculator` interface (alongside `pkg/billingauth`): input = customer location/country, VAT-ID (+ validation status), line items, amounts, currency, merchant, B2B/B2C; output = tax lines (jurisdiction, rate, amount, tax code, reverse-charge flag) or an explicit "not configured / zero" result.
- Called at **two** points: checkout/quote (estimate, may be advisory) and **invoice finalization** (authoritative, snapshotted).
- Tax is computed **per-line in the row/invoice currency** — never cross-currency (consistent with the FX-forbidden double-entry ledger; no conversion).
- **VAT-ID capture + validation**: store the customer VAT number; validate via a host-supplied validator (VIES for EU); snapshot the validation result + reverse-charge determination onto the invoice (mirrors UniBee's `VatVerifyData` snapshot). B2B cross-border EU → reverse-charge zero-rate with a note line.
- **Versioned document math**: snapshot rate + amount onto the invoice at finalization; historical documents are never recomputed when the calculator or rates change.
- Consider an invoice `tax_status` (`pending|succeeded|failed`) gating finalization when the calculator is async (Lago does this); fail-closed.

## Tasks

- [ ] Define the `TaxCalculator` interface + a built-in no-op (zero-tax) default and an embedded/standalone wiring point on the same seam as `Authenticator`/`DeliverEmail`.
- [ ] Add tax breakdown to `invoice_items` / `invoices.line_items` jsonb + a `tax_total` aggregate column; enforce `total = subtotal − discounts + tax_total`.
- [ ] Add customer VAT-ID storage + a host-supplied validation hook; snapshot the verify result + reverse-charge flag onto the invoice.
- [ ] Decide tax-inclusive vs tax-exclusive pricing (prices are ex-tax minor-unit amounts today) and a per-merchant default tax behavior.
- [ ] Wire tax into checkout quote + invoice finalization; leave the admission/spendgate hot path untouched (admission is credit-headroom, not invoicing).
- [ ] Integration tests: EU B2C standard-rate, EU B2B reverse-charge, non-EU zero, calculator-not-wired = no tax.

## Open Questions

- Native rate table in core, or fully host-side? (lean fully host-side; ship no rates.)
- Tax-inclusive pricing support, or exclusive-only initially?
- Async-tax finalization gating — needed now, or only once a real async provider is wired?

## Stripe sync note (ties to #586 catalog mirror)

The `TaxCalculator` seam is the engine and stays rail-neutral; this is only the
Stripe-rail mirror angle. When mirroring catalog to Stripe (#586 sibling), also
push tax metadata so the Stripe-hosted side is consistent: set `tax_behavior`
(`inclusive|exclusive`) on mirrored Prices and a `tax_code` on mirrored Products.
For Stripe-hosted Checkout, a merchant can opt into `automatic_tax[enabled]=true`
so Stripe Tax computes for THAT rail — treat Stripe Tax as ONE possible
host-supplied `TaxCalculator` implementation (its result snapshotted onto the
OpenRails invoice at finalization), never as core. Non-Stripe rails keep using the
host calculator. One-way OpenRails → Stripe; OpenRails invoice tax lines remain the
source of truth.

## Acceptance

- A host can supply a `TaxCalculator`; invoices carry correct tax line items + totals; VAT-ID + reverse-charge are captured and snapshotted; with no calculator wired, behavior is identical to today.

---

# #577: coupons / discounts — first-class object (definition + redemption ledger, checkout & renewal modifier)

**Completed:** no — planned.

Today the only discount mechanism is three free-text columns on `payments` (`discount_code` / `discount_reason` / `discount_metadata`) — a manual price-override annotation set at purchase time (`internal/modules/checkout/purchase_service.go`, `payments/register_purchase.go`). There is no coupon object, no validation, no redemption tracking, no percentage/recurring logic. Promo codes / first-month-free / %-off are conversion table-stakes for the SaaS, adult-subscription, and storefront verticals. Origin: design comparison vs Lago + UniBee (2026-06-24).

## Design principle

Steal **UniBee's** model specifically (it's richer than Lago's and maps cleanly onto our immutable-price design): a coupon **definition** table + a per-user **redemption ledger** (`merchant_discount_code` + `merchant_user_discount_code`). The coupon is a **checkout-time + renewal-time modifier** — the canonical `prices` row is untouched; the coupon adjusts the charged amount and writes a redemption row. It is NOT a price mutation and NOT a new price object. The engine lives OpenRails-side so it works uniformly across NMI/CCBill/Solana (which have no native coupon concept), consistent with "OpenRails owns the billing semantics across rails"; optionally map to Stripe Coupon/promotion_code for Stripe-hosted checkout.

## Target Model

- **Definition**: percentage vs fixed-amount; `once | recurring | forever` (+ duration); validity window; active flag; per-merchant.
- **Scoping / gates** (UniBee-rich): user-scope (`all | new-customers-only | renewals-only`), upgrade-only / upgrade-longer-only, plan allow/exclude lists, per-subscription / per-cycle / per-user redemption limits, total-quantity-limited code pools.
- **Fixed-amount + currency**: percentage coupons are currency-agnostic. For **fixed-amount** coupons, the simplest first cut is to **define them per-currency** (or single-currency-scoped) and not cross-convert — the ledger's currency-guard trigger forbids cross-currency transfers *today*. This is **not a permanent rule**: cross-currency conversion at spot is already planned in **#515 (cross-currency settlement)**, which adds the `fx_liquidity` two-transfer mechanic + reuses the shipped `internal/integrations/fx` rate provider. So once #515 lands, a fixed-amount coupon defined in currency A *could* optionally convert to charge currency B at spot (UniBee-style cross-convert) — until then, per-currency. Decide whether to ship per-currency-only and revisit after #515, or gate cross-convert behind #515 from the start.
- **Application points**: checkout/first charge (`internal/modules/checkout/service.go`); `RenewMembership` (`internal/modules/subscriptions/lifecycle_service.go`) for recurring coupons; tier-change checkout for upgrade-only gates.
- **Redemption ledger**: one row per application (coupon, customer, subscription, invoice/payment, amount discounted, cycle index). Supersedes/back-fills the free-text `discount_*` columns; powers "remaining redemptions", "applied N of M cycles", and history-discount-aware proration.
- **Proration interplay**: a discounted sub changing tier must credit back the **discounted** historical amount, not list price (UniBee's `ComputeHistoryDiscountAmount`); wire into the existing checkout proration.
- **Invoice/versioning**: discount as a negative line item + `discount_total`; snapshot the computation; version the application logic so historical documents stay stable. Ordering vs tax (#576): decide discount-before-tax vs after (Lago has a `before_taxes` flag — pick one default, make it explicit).

## Tasks

- [ ] Schema: coupon-definition table + redemption-ledger table (both `(merchant_id, …)`-scoped, RLS).
- [ ] Validation/redeem service: gate checks (user-scope, plan allow/exclude, limits, window, quantity pool) → applicable discount amount.
- [ ] Apply at checkout + `RenewMembership` + tier-change; write redemption rows; make proration history-discount-aware.
- [ ] Add `discount_total` + discount line items to invoices; define discount-vs-tax ordering with #576.
- [ ] Migrate/retire the free-text `discount_*` columns (keep as denormalized annotation or drop).
- [ ] Integration tests: %-off recurring, fixed-amount per-currency, new-customers-only rejection on renewal, upgrade-only gate, redemption-limit exhaustion, discounted-tier-change proration.

## Stripe sync note (ties to #586 catalog mirror)

Expands the "optionally map to Stripe Coupon/promotion_code" line above. Same
one-way mirror pattern as products/prices/entitlements (#586): mirror each coupon
DEFINITION → a Stripe Coupon (+ a Promotion Code for the user-facing code),
idempotent and `openrails_*`-marked for ownership; on Stripe-hosted Checkout
sessions pass the resolved discount via `discounts[]` (or `allow_promotion_codes=true`
to let Stripe-native promo codes apply) so Stripe's invoice/receipt reflects the
discount. OpenRails' redemption ledger stays the cross-rail source of truth — never
read redemption state back from Stripe (it would be empty for NMI/CCBill/Solana).
Cheap interim before the full object lands: set `allow_promotion_codes=true` on the
Stripe Checkout session so Stripe-managed promo codes work on the Stripe rail today.

## Open Questions

- Stacking (multiple coupons per sub)? — recommend **no stacking** initially.
- Interaction with existing promo-credit/wallet discounting — coupon vs credit precedence.
- First-month-free modeled as a coupon vs a trial?
- Discount before or after tax (#576) as the default.

## Acceptance

- Merchants can define coupons with UniBee-class scoping; redemptions are ledgered and limit-enforced; discounts apply at checkout and on recurring renewals across all rails; proration credits the discounted amount; historical documents are stable.

---

# #578: credit notes — projection over refunded/reversed invoices (no new document engine)

**Completed:** no — planned.

OpenRails has refunds (negative `payments` rows referencing `refunded_payment_id`) and `voided`/`uncollectible` invoices, but **no credit-note document**. Many jurisdictions require a credit note (not merely a refund record) to reverse VAT — so this pairs with #576. Origin: design comparison vs UniBee (2026-06-24).

## Design principle

Steal **UniBee's** trick: **no separate table** — a credit note is a **projection/view** over already-reversed/refunded invoices (their `ConvertInvoiceToCreditNoteDetail`, rendered with a "Reversed" status). OpenRails' invoicing is already arrears/statement-shaped, so this is a presentation layer over data we already hold (`invoices` + `invoice_items` + refund payment rows). The **only** genuinely new state is a per-merchant credit-note number sequence (compliance needs its own numbering).

## Target Model

- Derive a credit-note document from: original invoice ref, credited line items, refunded amount, reason, issued-at, sequential credit-note number.
- **Issued when**: (a) a payment is refunded against an invoice; (b) an invoice is voided **after** being (partially) paid; (c) an explicit credit/adjustment is applied. A voided **unpaid** invoice needs no credit note (no money moved).
- **Tax reversal**: when #576 lands, the credit note must reverse the tax lines too (snapshot the reversed tax).
- **Numbering**: dedicated per-merchant credit-note sequence (mirror invoice numbering); the one new piece of durable state.
- **Surface**: `/v1/me/credit-notes[/:id]` (delegated read) + merchant admin list; JSON/statement projection consistent with the invoice statement shape. PDF is out of scope here (OpenRails has no PDF gen; route document artifacts through #512 S3 report-artifacts if/when needed).
- Postgres invoice/payment data stays the source of truth; the credit note is derived except for its number.

## Tasks

- [ ] Add a per-merchant credit-note number sequence.
- [ ] Projection/service deriving credit-note detail from invoice + refund/void data.
- [ ] `/v1/me/credit-notes` delegated read + merchant admin list.
- [ ] Tie tax reversal to #576 when present.
- [ ] Integration tests: full refund, partial refund, void-after-paid, void-unpaid (no credit note).

## Open Questions

- Line-level (partial) credit notes, or whole-invoice only initially?
- Numbering reset cadence (annual vs continuous)?

## Acceptance

- A refund or paid-invoice void yields a numbered credit-note document (derived, not a new ledger), readable on the self-service + admin surfaces, with tax reversed once #576 exists.

---

# #579: client-facing `Idempotency-Key` header on public mutating routes

**Completed:** no — planned.

OpenRails' **internal** idempotency is strong (`IdempotencyService` op-keys, natural-key dedup, idempotent intents + spendgate, 90-day TTL — well ahead of UniBee, which has none). What's missing is the **Stripe-style client-facing `Idempotency-Key` HTTP header** on public POSTs so integrators can safely retry without duplicate side-effects — the contract API clients expect. Origin: design comparison vs Lago (2026-06-24).

## Target Model

- Middleware on mutating public routes: read `Idempotency-Key`, store `(merchant[, subject], key, request fingerprint) → captured response` and **replay** the stored response on retry within a TTL.
- **Conflict** (same key, different request body) → 409/422; **in-flight** duplicate → 409 "request in progress".
- Back it with the existing `IdempotencyService` (it already does op-key + dedup + TTL); the new work is the HTTP-header layer + response capture/replay.
- Header is **optional** (Stripe-style) — absent key = today's behavior. Where a natural idempotency key already exists (e.g. admissions on `(type, source, id)`), the header layers on top / both are honored.
- Applies in embedded mode to mounted routes too.

## Tasks

- [ ] Idempotency middleware (header parse, fingerprint, store, replay, conflict/in-flight handling) over `IdempotencyService`.
- [ ] Apply to checkout, charge, subscription create/cancel, payment-method, tier-change routes; document which routes honor it.
- [ ] Define TTL (Stripe uses 24h), key scoping (merchant + optionally subject), and response-size limits.
- [ ] Integration tests: retry replays identical response, same-key-different-body conflict, concurrent in-flight, key-scoped to merchant.

## Open Questions

- TTL length (24h default?).
- Response-replay storage size cap + what to do when exceeded.
- Precedence/interaction where a natural idempotency key already protects the route.

## Acceptance

- Public mutating routes accept an optional `Idempotency-Key`; a retried request with the same key replays the original response and causes no duplicate side-effect; mismatched reuse is rejected; absent header preserves current behavior.

---

# #580: SPECULATIVE — event-handler subscriptions for async, non-user-initiated billing occurrences ("notify me when X, so I can do Y")

**Completed:** no — **speculative; not needed now.** File only; do not build until a concrete merchant need appears.

## Context / why this is deliberately deferred

OpenRails is a **pull-from-source-of-truth** system by design: consumer apps query OpenRails as the authority for state (entitlements, balance, "is a rebill due"), and the delegated-token path even lets the app's **frontend** query directly without its own backend in the loop. This is the right spine, and an outbound event/webhook feed must **not** erode it — pushing *state* invites apps to keep a local replica and get it wrong, which is exactly what the entitlement-timeline-as-truth design avoids. Decided against a general outbound-webhook feature on these grounds (2026-06-24 design comparison vs Lago/UniBee).

This issue captures the **one narrow seam** pull does not cover, named so it's dismissed deliberately rather than by omission.

## The seam (and ONLY this seam)

**App-domain side-effects that must fire promptly and exactly-once in reaction to an *async, non-user-initiated* billing occurrence.** Examples:
- deprovision a Discord role the moment a 3am rebill **fails**,
- kick off a fulfillment workflow on a one-time purchase,
- post to the ops Slack channel on a **chargeback**.

The distinction is **occurrence** (something happened at an instant; a side-effect must run) vs **state** (what is true now). Pull serves state perfectly; it does not proactively serve occurrences.

## Explicitly NOT in scope (covered already)

- **State / access checks** — pull is correct; push would be an anti-pattern.
- **User-initiated changes** — the frontend pulls on checkout-return and sees new state instantly; no event needed.
- **Transactional email** — OpenRails already owns this via `DeliverEmail` / `notification_queue`.
- **Correctness of access decisions on async changes** — already fine: the *next* pull reads truth. The only thing missing is the app reacting *at the instant* of the event instead of at next pull, and even that has a **pull fallback**: a periodic reconcile poll. So this is a latency/convenience improvement, **not** a correctness gap.

## Possible shapes (if ever built)

- **Embedded apps**: in-process **event-handler registration** — a Go subscriber/callback on the normalized event stream OpenRails already produces. The taxonomy exists today (`internal/modules/analytics/event_log_service.go` already mints `subscription_created/cancelled/past_due`, `payment_*`, refund, chargeback…); these events are generated and logged but currently have nowhere to go.
- **Remote / standalone apps**: a per-app **outbound signed event delivery** with an event-type allowlist + durable delivery queue — reuse the **intent-ledger pattern** (durable outbox, leased claims, retries, replay). If built: sign with a **per-endpoint secret, never the API key** (the mistake UniBee made), and scope strictly to occurrence events, never state snapshots.

## Why wait

- Building it risks tempting integrators back into stale local mirrors unless carefully scoped to occurrences-not-state.
- The pull spine + periodic reconcile poll already keeps everything correct; only sub-minute *reaction* to async events is improved.
- Worth building only when a concrete merchant needs prompt exactly-once reaction to async, non-user-initiated events.

## Tasks (only if revived)

- [ ] Confirm a concrete merchant use case requiring sub-minute reaction to an async occurrence (not satisfiable by poll+reconcile).
- [ ] Decide embedded in-process handler vs remote outbound delivery (or both).
- [ ] Define the occurrence event catalog exposed (subset of `event_log_service` types) — occurrences only, no state payloads.
- [ ] For remote: durable delivery queue on the intent-ledger pattern, per-endpoint secret signing, event-type allowlist, replay.
- [ ] Exactly-once / at-least-once + idempotent-handler contract; delivery ordering guarantees.

## Open Questions

- Does any real merchant need this, or does poll+reconcile suffice indefinitely?
- Delivery semantics (at-least-once + idempotency key on the event) and ordering.
- How to keep the surface occurrence-only so it can't be abused as a state-replication channel.

---

# #581: Authorize.Net gateway adapter (second card rail after NMI)

**Completed:** no — planned. Likely the **next** processor adapter to build.

## Why

Authorize.Net is the **#2 card gateway in high-risk after NMI**, and it's the one merchants land on when they can't or won't use NMI: they already have an Authorize.Net account, or their acquiring bank/ISO provisions Auth.Net instead. The high-risk ISOs we'd onboard through (PayKings, PaymentCloud, HighRiskPay, Host Merchant Services) all resell **NMI _or_ Authorize.Net** on top of the merchant account — so today, "ask for NMI and you're covered," but any merchant stuck on Auth.Net is unservable. Supporting Auth.Net widens the set of acquirers/ISOs OpenRails can take without forcing a gateway switch. Origin: adult/high-risk acquirer research (2026-06-24).

**The big lever:** NMI was built to speak **Authorize.Net's transaction format** (AIM / Direct Post), so this is the *closest* adapter we'll ever add to an existing rail. Much of the NMI rail's shape — vaulted cards + OpenRails-driven dunning + direct-sale rebills — ports directly. This is an *adapter*, not a new rail architecture.

## Target Model (map onto the existing decomposed abstraction)

OpenRails has **no single `PaymentProcessor` interface** — a gateway is implemented across four seams. Auth.Net plugs into each, using the NMI rail (`internal/integrations/nmi`, `internal/modules/payments/processors/nmi.go`, `internal/modules/vault`, `internal/modules/subscriptions/dunning.go`) as the template:

- **Processor enum**: add `authorize_net` (a.k.a. `authnet`) to `models.Processor`. It is **NMI-format-adjacent but NOT NMI-backed** — keep `processors.IsNMIBacked` false for it; decide whether it shares NMI rail helpers or gets a sibling package (recommend a sibling `internal/integrations/authnet` that reuses shared card/dunning lifecycle code, not the NMI direct-post client).
- **Gateway client** (`internal/integrations/authnet`): target Auth.Net's **current API** (`createTransactionRequest` for auth/capture/sale/refund/void), not the legacy AIM — NMI's Auth.Net-format compatibility helps us *read* the model, but we still call Auth.Net's own endpoints (`api.authorize.net` / `apitest.authorize.net`).
- **Card vault**: today `internal/modules/vault` is NMI-only (`CreateCustomerVault → vault_id`). Add the Auth.Net equivalent — **CIM** (Customer Information Manager): `createCustomerProfileRequest` + payment profile, store the profile/payment-profile IDs as the vault ref. Reuse the compensating-delete + "refuse delete with active sub" safety from the NMI path.
- **Subscription lifecycle**: prefer **OpenRails-driven charges against the CIM profile** (the direct-sale rebill path we already use for NMI dunning via `intents/manual_rebill.go`) over Auth.Net's native **ARB** (Automated Recurring Billing). ARB is limited and would pull lifecycle control back to the provider; OpenRails-driven keeps proration/dunning/grace uniform with NMI. **Decision to lock:** ARB vs OpenRails-driven-against-CIM (recommend the latter; revisit only if a merchant needs provider-native recurring).
- **Inbound webhooks** (`webhooks.WebhookHandler` + `internal/shared/sigverify`): add an Auth.Net verifier. Auth.Net signs webhooks with **HMAC-SHA512** (`X-ANET-Signature` header) over the body using the webhook signature key — distinct from NMI's `timestamp.body` HMAC. Also account for the legacy **Silent Post URL** if any target account only supports that. Normalize to the existing event taxonomy.
- **Outbound mutations** (`intents.Handler`): add `authnet_refund` / `authnet_void` / `authnet_delete_subscription` (if ARB) intent types through the durable intent ledger, with the account-fingerprint guard.
- **Reconcile / convergence PULL**: Auth.Net has read-only detail APIs (`getTransactionDetailsRequest`, `getSettledBatchListRequest`, `getUnsettledTransactionListRequest`) — wire them into the `ProcessorFetcher` so the four-plane engine and the intent verifier get "detail-call-as-truth" parity with NMI/Stripe.
- **Catalog provider mapping**: with OpenRails-driven charges (no ARB), Auth.Net is **amount-driven like NMI** — deterministic local plan id, no remote catalog object, so no `pending_manual_link`. (Only ARB would require provider-side subscription objects.)
- **Modes**: enforce the `readonly` wire-gate on the Auth.Net transport (mirroring the NMI direct-post gate / Stripe transport gate / Solana submission gate). `limited` defers system-origin intents as usual.
- **`test_mode`**: Auth.Net sandbox is a **separate host** (`apitest.authorize.net`) and separate sandbox accounts, so the credential guarantee is **structural (URL-based)** — much simpler than NMI's boot-time test-card probe. A live key pointed at the sandbox URL (or vice-versa) is rejectable without a probe; reuse the `billing.probe_verdicts`/refingerprint plumbing only if useful.

## Open Questions / Decisions

- **ARB vs OpenRails-driven-against-CIM** for recurring (recommend OpenRails-driven).
- **Share the NMI rail vs sibling package** — how much of `internal/integrations/nmi` + the NMI dunning/vault paths can be factored into shared card-rail helpers without leaking NMI specifics.
- Webhooks vs legacy **Silent Post** — support both, or webhooks-only and require accounts that have webhooks enabled?
- Does the existing `IsNMIBacked` branch logic need a broader `IsCardDirectSale`-style predicate so Auth.Net inherits the right dunning/vault behavior without being mislabeled NMI?

## Tasks

- [ ] Add `authorize_net` to `models.Processor` + routing; introduce an `internal/integrations/authnet` client (current JSON API: sale/auth/capture/refund/void).
- [ ] CIM-based vaulting in `internal/modules/vault` (profile + payment profile), with compensating delete + active-sub guard.
- [ ] Wire the OpenRails-driven rebill/dunning path to charge the CIM profile (reuse `manual_rebill` + `dunning.go` cadence).
- [ ] Inbound webhook handler + `sigverify` HMAC-SHA512 (`X-ANET-Signature`) verifier; normalize to the existing event taxonomy; decide Silent-Post fallback.
- [ ] Intent handlers: `authnet_refund` / `authnet_void` (+ ARB delete only if ARB chosen).
- [ ] Reconcile PULL fetcher + intent verifier using Auth.Net transaction/batch detail APIs.
- [ ] Mode gating on the Auth.Net transport (`readonly` wire-gate, `limited` system-intent defer).
- [ ] `test_mode` structural URL guarantee (`apitest.authorize.net`) + live-key-refuses-sandbox/boot check.
- [ ] Integration tests against the Auth.Net sandbox: sale, vault+rebill, refund/void, webhook signature verify, reconcile pull, readonly-blocks-write.

## Acceptance

- A merchant on an Authorize.Net account can run the full lifecycle (checkout, vaulted rebill, OpenRails-driven dunning, refund/void, reconcile) through OpenRails, with the same mode/test-mode/convergence guarantees as the NMI rail — so an ISO that provisions Auth.Net instead of NMI is fully servable without a gateway switch.

---

# #680: USDC funding into user Solana wallets via Robinhood / Coinbase

**Completed:** no — FUTURE (2026-07-01). Successor to #328 (closed): the half-implemented v1 (Coinbase onramp
sessions, `usdc_funding_sessions` table, Hook0 webhook ingestion, `ufs_` ids, USDCFundingConfig) was DELETED in
the #666 dead-code purge — sessions could never be created (no route ever called Service.Create), so it was
dead weight. Recover the code from git history if useful; #328's section in completed.md has the full design
notes (contract, statuses, provider adapters, wallet-balance verification).

## Metadata
- Category: feature
- Status: future
- Passes: false

## Intent (Paul, 2026-07-01)
Make it easy for clients' users to fund USDC into their own (self-custody) Solana wallets so they can complete
Solana wallet checkouts — using Robinhood and Coinbase as the funding providers. OpenRails never custodies
funds; the provider handles login/KYC/buy/transfer; completion is verified by Solana wallet balance (return
from provider is never proof of funding — the v1 got this right, keep it).

## What survives today (kept, do not rebuild)
- The checkout insufficient-USDC error metadata (`error.metadata.usdc_funding`: network, asset, wallet,
  amount/balance/shortfall) — doujins' current UX drives manual Robinhood/Coinbase links off this plus
  connected-wallet balance checks. That's the working v0.
- Solana USDC balance verification code in internal/modules/solana.

## When picked up
- Blockers that killed v1 still apply: Robinhood has no partner API docs/access (was never implementable);
  Coinbase onramp needs CDP keys + webhook subscription. Don't rebuild until a provider path is actually
  usable end to end.
- Route creation THROUGH the checkout flow this time (v1's create/get session routes were never wired — build
  the wiring first, provider adapters second).
- Provider writes go through the intents log per #674's design decision.

---

# #687: revision counters (optimistic CAS) on OpenRails-authoritative records

**Completed:** no — FUTURE (2026-07-01, Paul). From the payload-apply/versioning design discussion: per-object
monotonic version numbers are the clean ordering mechanism (k8s resourceVersion / DynamoDB sequence numbers /
Solana slots), and Solana is the one rail where OpenRails already uses exactly this (minContextSlot /
ReadUntilConsistent). Stripe/NMI expose no object versions, so this issue applies the mechanism ONLY where the
counter can actually mean something: records WE own and control.

## Metadata
- Category: architecture
- Status: future
- Passes: false

## Scope rule (the whole point — Paul 2026-07-01)
Version counters go on records where OPENRAILS IS THE STATE MACHINE driving the record forward. They must NOT
be treated as a freshness signal on records that merely MIRROR a provider's state machine (Stripe/NMI-driven
subscriptions): our revision orders OUR writes, not the provider's mutations — revision 7 of a mirror can carry
older information than a later fetch would; only a fetch (#684) learns newness for mirrors. (On mirrors, CAS
could still guard our own concurrent writers against each other, but #675's row locks already do that and #684
makes it moot — so mirrors are OUT of scope.)

## Why (Paul, 2026-07-01) — consumers first, consistency second
This exists primarily for OPENRAILS' OWN CONSUMERS: when openrails is more widely used and emits its own
events/webhooks (and on every API response), the object's revision rides along — so hosts get the ordering
primitive Stripe never gave us (order, dedupe, reject-stale trivially; no consumer ever rebuilds the #675
watermark machinery against US). Secondarily it keeps our own state consistent under concurrent writers.
Be the provider Stripe wasn't: expose the counter.

## Mechanism
- `revision BIGINT NOT NULL DEFAULT 1` on each in-scope table; MONOTONIC-ONLY is a hard invariant — every
  UPDATE goes `SET revision = revision + 1 ... WHERE id = $1 AND revision = $expected`; zero rows ⇒
  concurrent-modification ⇒ reload-and-retry or surface a conflict error (per call site). Backstop the
  monotonicity structurally (the CAS pattern guarantees it; a BEFORE UPDATE trigger asserting new.revision =
  old.revision + 1 is the belt-and-suspenders option if any non-CAS write path survives).
- Readers carry the revision they read; writers CAS against it. Optimistic — replaces/augments FOR UPDATE where
  retry loops beat lock waits; KEEP pessimistic locks on hot money paths where a retry loop is worse
  (LockCustomerForSpend stays).
- CONSUMER SURFACE (the point): revision appears on API responses for in-scope objects and on any future
  OpenRails-emitted events for them; document the contract "never apply revision N after N+1" for hosts.
  ETag/If-Match on admin/config mutation endpoints falls out of the same field (concurrent admin edits
  conflict visibly instead of last-write-wins).
- Vector clocks (considered, REJECTED): vector clocks solve multi-master causality — concurrent independent
  writers on partitioned replicas (Dynamo-style). OpenRails has ONE writer authority per record (single
  Postgres, serialized by locks/CAS), so causality is already total-ordered; a scalar monotonic counter is
  sufficient and strictly simpler. Revisit only if openrails ever goes multi-master (it should not).

## Candidate record classes (verify authority per table when picked up)
- OpenRails-driven subscription state: solana recurring subs (we crank), NMI vaulted-dunning lifecycle fields
  (retry counts, schedules) — careful: the NMI REMOTE recurring record stays a mirror.
- checkout_sessions (our state machine end-to-end).
- provider_intents (has its own status machine; CAS may already be implicit in claim/lease — verify, don't
  double-guard).
- merchant configuration / credit-account settings / tier schedules (admin last-write-wins today).
- invoices (status transitions).
- NOT: grants + ledger transfers (append-only — immutability is stronger than versioning); NOT provider
  mirrors (rule above); NOT entitlements (derived projection — re-derivable from grants).

## Acceptance
Every in-scope table's UPDATE path is CAS-guarded (no silent lost updates under concurrent writers, proven by
a concurrency test per class); mirrors carry NO revision-as-freshness semantics anywhere; hot money paths keep
their existing lock discipline unchanged.
