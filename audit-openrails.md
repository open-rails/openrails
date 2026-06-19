# Architecture Review — Payments & Authentication

*2026-06-18 · follow-up to `API_DESIGN_AUDIT_2026-06-18.md`*

---

## Purpose

The June 18 API design audit catalogued route-shape and middleware issues across `openrails` and the embedded `authkit` library — the consequences of a single-tenant codebase being stretched toward multi-merchant SaaS. This document is the next layer down: implementation-level findings in the two highest-stakes subsystems, **payments** and **authentication**, with concrete remediation plans for the three highest-leverage issues.

The multi-merchant migration is treated as in-flight context, not the subject of this review. Audit findings OR-API-C1 (single-merchant boot config), OR-API-C2 (global webhook surface), and the API-hygiene items (duplicate routes, REST verb consistency, error-shape consistency) are deferred to the migration owners and a separate hygiene pass.

## Findings — at a glance

| ID | Surface | Severity | What |
|----|---------|----------|------|
| OR-IMPL-1 | Payments / inbound | High | Webhook processing has correctness gaps under takeover + zero operator visibility into failed events |
| OR-IMPL-2 | Payments / outbound | High | Refund flow swallows a reachable DB error; `Idempotency-Key` not honored on user POSTs |
| OR-IMPL-3 | Auth / delegated | Medium-High | Delegated-token permissions, subject status, and merchant status are not re-validated per request |
| OR-IMPL-4 | Payments / abstraction | Medium | Processor abstraction is half-finished — webhook verify/normalize is unified, but credential resolution, outbound mutations, and event identity remain per-processor |

Each is described below with diagnosis, recommended approach, critical files, and verification. A backlog of secondary findings sits at the end.

---

## OR-IMPL-1 — Webhook processing safety

### Diagnosis

`internal/modules/webhooks/deduplication.go` has the right *shape* for idempotency: `idem.Begin` records the event as `pending` (line 123) before `processingFunc` runs, then transitions it to `success` or `failed`. Stale-pending records can be taken over after a 2-minute lease (line 136-148). Three problems live underneath that shape:

**1. CCBill has no event identity.** CCBill posts form-encoded transaction notifications with no provider-assigned event_id. `IsDuplicate` short-circuits when the event_id is empty (deduplication.go:71-74), so every CCBill webhook bypasses dedup. Network retries by CCBill, or any redelivery, will re-apply: a CCBill webhook that creates a `payment` row via `payments.PaymentService.Create` is *not* idempotent at the DB layer — there is no `ON CONFLICT (processor, transaction_id) DO NOTHING` discipline in the processor code.

**2. Non-idempotent processing under takeover.** The 2-minute pending lease means: if `processingFunc` commits its DB writes (payment row, subscription state change) but the worker dies before `idem.Complete`, after 2 minutes another worker takes over, re-runs `processingFunc`, and unless the underlying SQL is idempotent (it isn't, consistently), state is duplicated. The dedup layer cannot fix this alone — the processor SQL must be idempotent for the takeover lease to be safe.

**3. No operator visibility into failed webhooks.** `GetFailedWebhooks` (deduplication.go:221) returns `[]any{}` unconditionally with the comment "Not supported with TTL-based storage." `CleanupOldWebhooks` (line 225) is a no-op log line. The 90-day `WebhookIdempotencyTTL` (line 18) implies a durable store, but the surface for inspecting failures is dead. Operators cannot answer "which Stripe events failed yesterday and why" without log-grepping. The `repair_alerts` and `manual_rebill_attempts` tables referenced in `migrations/postgres/001_schema.up.sql` comments are never written to.

### Recommended approach

**1a. Synthesize a stable CCBill event_id and enforce DB-level dedup.**

CCBill webhooks carry a `transactionId` for one-off events, and `subscriptionId + eventType + timestamp` for subscription events. Synthesize a stable event_id at the webhook handler boundary (`ccbill:{transactionId}:{eventType}` for one-offs; `ccbill:{subscriptionId}:{eventType}:{timestamp}` for subscriptions) and pass it into `ProcessWebhook`. Document the synthesis rule in `internal/integrations/ccbill/` so it is canonical.

In parallel: ensure `payments` has `UNIQUE (processor, transaction_id)` and wrap `PaymentService.Create` with `ON CONFLICT (processor, transaction_id) DO NOTHING RETURNING *` semantics. Defense in depth — even if dedup fails, the DB rejects the duplicate.

**1b. Make `processingFunc` idempotent at the SQL layer.**

Walk each processor handler (`internal/modules/webhooks/stripe.go`, `nmi.go`, `ccbill.go`, `coinbase.go` if present) and ensure every mutation is idempotent: payment creation by `(processor, transaction_id)`, subscription state transitions guarded by `WHERE status != target`, entitlement grants `ON CONFLICT DO NOTHING`. The 2-minute takeover lease is only safe when `processingFunc` is re-runnable.

Where idempotency cannot be made cheap (e.g., notification side-effects), gate the side-effect on a status transition that the SQL layer atomically enforces, so the second run sees the new state and skips.

**1c. Resurrect operator visibility for failed webhooks.**

The `idempotency` module already persists records with status. Replace the stub `GetFailedWebhooks` with a real query against the idempotency store, scoped by `op LIKE 'webhook.%'` and `status = 'failed'`, paginated. Surface it through a new `/v1/admin/webhook-failures` route (or fold into `/v1/admin/repair-alerts`) so operators can list, inspect payload, and trigger a manual reprocess. The `repair_alerts` and `manual_rebill_attempts` tables should either get populated here or be dropped.

### Critical files

- `internal/modules/webhooks/deduplication.go` — implement `GetFailedWebhooks`; document semantics
- `internal/modules/webhooks/ccbill.go` — event_id synthesis at handler boundary
- `internal/modules/webhooks/stripe.go`, `nmi.go` — audit processingFunc idempotency
- `internal/modules/payments/payment.go` — `PaymentService.Create` → upsert by `(processor, transaction_id)`
- `internal/modules/idempotency/` — add `ListFailed(ctx, opPrefix, limit, cursor)` query
- `internal/http/handlers/admin_webhooks.go` (new) or fold into existing `admin_repair.go` — operator surface
- `migrations/postgres/` — new migration: `UNIQUE (processor, transaction_id)` on `payments` if missing; populate or drop `repair_alerts` / `manual_rebill_attempts`

### Verification

- Unit: `DeduplicationService.ProcessWebhook` with a `processingFunc` that commits then panics; second invocation under takeover lease must not produce a duplicate payment row (requires the SQL upsert).
- Unit: CCBill event_id synthesis is stable for a given (transactionId, eventType, timestamp); re-delivery short-circuits at dedup.
- Integration: replay a captured Stripe webhook twice; assert one payment row and one entitlement grant.
- Manual: send a duplicate CCBill subscription notification via test fixture; assert second is silently ignored at both dedup and SQL level.
- Operator UX: hit `/v1/admin/webhook-failures` after a deliberately failed webhook; expect to see it listed with payload visible.

---

## OR-IMPL-2 — Payment-mutation safety

### Diagnosis

Two related cracks in the outbound payment-mutation surface.

**2a. Admin refund flow swallows a reachable error and carries redundant locking.**

`internal/http/handlers/admin_payments.go`:

- **Line 285** is the real problem. After `intents.RefundIntentFor` returns an error (reachable path — e.g. provider target missing or malformed), the code calls `_ = paymentService.MarkFailed(ctx, prepared.reservation.ID)`. If `MarkFailed` itself errors (DB hiccup, connection pool exhaustion), the reservation row stays in `pending` forever. The next refund attempt with the same idempotency key then hits line 180-181 in `prepareAdminRefund` and is rejected as "refund request is already pending" — a transient DB error permanently blocks refund retries on that payment.
- Line 279 sits on an unreachable default branch in the processor switch (`prepareAdminRefund` at line 195-213 already filtered unsupported processors). Sloppy, not critical; either delete or convert to a log-and-500.
- `adminRefundLocks sync.Map` (line 47-54) is process-local. The `pg_advisory_xact_lock` at line 124 is the actual cross-replica safety. The sync.Map adds no safety in a multi-replica deployment and confuses readers about which lock provides the contract.

**2b. `Idempotency-Key` is not honored on user-facing money POSTs.**

The admin refund endpoint honors `X-Idempotency-Key` correctly (admin_payments.go:95). User-facing money POSTs do not:

- `POST /v1/checkout` — uses internal session fingerprinting at `CheckoutSessionService.CreateSession` but does not honor a client-supplied `Idempotency-Key`. A browser retry produces a second session.
- `POST /v1/me/payment-methods` — no `Idempotency-Key`; double-tap creates two payment-method rows.
- `POST /v1/me/subscriptions/:id/change-tier|cancel|resume` — no `Idempotency-Key`.
- `POST /v1/self/*` mutation surface — same.

Stripe-style `Idempotency-Key` is a baseline expectation for every money POST. Its absence forces every client to implement defensive retry-deduplication client-side, and clients that don't (mobile, server-to-server self-service, third-party integrations) will produce duplicates under network failure.

### Recommended approach

**2a. Refund hardening.**

- Replace `_ = paymentService.MarkFailed(ctx, prepared.reservation.ID)` on line 285 with: log the original error, attempt `MarkFailed`, and *if `MarkFailed` errors, return a 500 with a `reservation_release_failed` code*. Surfacing the inconsistency is better than silently stranding a `pending` reservation.
- Add a sweeper job that finds reservations stuck `pending` for > N minutes with no matching intent and releases them. This is the fallback for the 500 path above.
- Drop `adminRefundLocks sync.Map`. The advisory lock is the contract.
- Line 279 unreachable branch: delete it (preferred — let exhaustive-switch lint catch processor additions) or replace with a `log.Errorf` + 500.

**2b. `Idempotency-Key` as a first-class primitive.**

The `idempotency` module already exists and is used by webhook dedup and the admin refund flow's bespoke check. Promote it to a request-scoped middleware:

- New `ginmw.IdempotencyKey(scope string)`:
  - Reads `Idempotency-Key` header (require for chosen routes; 400 if missing on POSTs that opt in).
  - On first request: records `pending` for `(merchant_id, user_id, scope, key)` → invokes handler → buffers response body and status → on success records the response and transitions to `success`; on handler error transitions to `failed`.
  - On subsequent matching request: replays the buffered response — *only* if request body fingerprint matches; mismatched body with same key returns 409 `idempotency_key_mismatch`.
- Mount on `POST /v1/checkout`, `POST /v1/me/payment-methods`, `POST /v1/me/subscriptions/:id/{change-tier,cancel,resume,payment-method}`, and the equivalent `/v1/self/*` mutations. The `/v1/self/*` surface is browser-direct; idempotency matters most there because retries are common on flaky mobile networks.
- Document the requirement in the OpenAPI surface so SDK clients pick it up.

The admin refund handler can then drop its bespoke idempotency check and rely on the middleware, reducing duplicated surface area.

### Critical files

- `internal/http/handlers/admin_payments.go:279, 285` — fix error suppression
- `internal/http/handlers/admin_payments.go:47-54` — remove `sync.Map` lock
- `internal/http/middleware/ginmw/idempotency.go` (new) — generic `Idempotency-Key` middleware
- `internal/modules/idempotency/idempotency.go` — extend with response-replay storage if not present
- `internal/http/routes/` — mount middleware on user POSTs (checkout, me/*, self/*)
- `internal/modules/payments/payment.go` — sweeper for stranded `pending` reservations
- `internal/http/handlers/checkout_session.go`, `me_subscriptions.go`, `me_payment_methods.go`, `self_*.go` — drop bespoke idempotency where it exists, rely on middleware

### Verification

- Unit: `MarkFailed` returning an error on the line-285 path produces a 500 with `reservation_release_failed`, not a silent 400.
- Unit: `IdempotencyKey` middleware on `POST /v1/checkout` — same key + same body → replays first response; same key + different body → 409.
- Integration: simulate a duplicated `POST /v1/me/payment-methods`; expect one payment-method row.
- Integration: two concurrent admin refunds on the same payment; advisory lock serializes; second returns 201 replay.
- Manual: kill the process mid-refund-intent-enqueue; on restart the sweeper releases the orphaned reservation; a fresh idempotency key succeeds.

---

## OR-IMPL-3 — Delegated token revocation freshness

### Diagnosis

`internal/http/middleware/ginmw/delegated.go:227-249` — `RequireDelegatedPermission` checks the permission against `resolved.Permissions`, which were embedded in the JWT at issue time. The middleware docstring confirms this: delegated tokens carry `permissions: ["openrails:self:*"]` or `openrails:merchant:*` as claims.

Service tokens are immune to this drift — they are opaque (no JWT), looked up server-side per request, so a revocation propagates immediately (modulo resolver caching).

Delegated tokens are NOT looked up per request beyond signature, issuer, audience, and expiry validation. Consequences:

- A merchant admin revokes a role's `openrails:self:billing:write` permission. Any browser session holding a delegated token issued before the revocation continues to write billing until the token expires.
- A delegated subject is offboarded (employee leaves, end-user account suspended). The delegated token remains valid until expiry; the suspension does not invalidate it.
- The merchant whose `merchant_id` is pinned in the token can be suspended on the openrails side; the delegated token continues to override `ResolveMerchant` and pin the now-suspended merchant on the request.

The exposure window equals the delegated token TTL. The structural issue is that delegated permissions, subject status, and merchant status are not re-validated per request.

### Recommended approach

**3a. Shorten delegated-token TTL to 5 minutes and add silent refresh.**

Trades operational complexity (more `/token` round-trips on the host frontend, refresh-failure handling) for a 5-minute exposure window. Worth doing regardless of the other steps below — it caps the worst case.

**3b. Per-request liveness check on three small predicates (recommended baseline).**

After JWT verification in `ResolveDelegated`, look up:

1. **Token revocation:** include a `jti` (JWT ID) claim at issue time. On each request, check `(jti, status)` in a revocation table or Redis set. Issue-time writes the row as `active`; explicit revocation flips it to `revoked`. One Redis GET per delegated request.
2. **Merchant status:** verify the pinned `merchant_id` is still active. The merchant directory cache already used for service tokens can be reused.
3. **Delegated subject status:**
   - For `DelegatedPrincipalRequired`, the host's `DelegatedAuthenticator` should reject — push validation upstream.
   - For `DelegatedSelfRequired`, check against AuthKit's user store with the same caching discipline.

Existing error vocabulary (`ErrAccessTokenRevoked`, `ErrServiceTokenMerchantUnresolved`) already covers the response codes the middleware switch at delegated.go:78-94 needs — no new error types required.

**3c. (Reject for now.)** Switch delegated tokens to opaque, like service tokens, with full per-request lookup. Cleanest semantic, highest per-request cost, incompatible with the browser-direct goal that the token can be validated offline by the host backend. Revisit only if 3b proves insufficient.

**Permission-set freshness:** even with 3b, the permission set in `resolved.Permissions` remains stale until the next token refresh. Accept this for v1 since 3a (5-minute TTL) bounds it. A `role_version` claim with per-request comparison is the next step if a security review demands it.

### Critical files

- `internal/controlplane/delegated.go` and the AuthKit-side issuer — add `jti` claim; configurable shorter TTL
- `internal/controlplane/` — `ResolveDelegated` implementation gains per-request liveness checks (jti revocation, merchant active, subject active)
- `internal/http/middleware/ginmw/delegated.go:75` — no middleware change required; the resolver returns the new failures via existing error returns
- `migrations/postgres/` — new table `delegated_token_revocations(jti, revoked_at, reason)` or a Redis schema doc
- `pkg/embedded/authkit/authkit.go` — token-issue path: bake `jti`, configure short TTL

### Verification

- Unit: revoke a delegated token's `jti`; next request returns 401 `delegated_token_revoked`.
- Unit: suspend a merchant; next request with a delegated token pinned to it returns 403 `delegated_tenant_unresolved`.
- Unit: suspend a delegated subject; next request returns 401.
- Integration: full lifecycle — issue → request OK → revoke → request 401, all within the token's TTL window.
- Performance: per-request liveness check overhead < 5ms p99.

---

## OR-IMPL-4 — Finish the processor abstraction

### Diagnosis

The processor surface is **half-abstracted**, not unabstracted. Reading the partial state matters because the recommendation is "complete the contracts that already exist," not "design from scratch."

**Already abstracted (good shape, leave alone):**

- **Inbound webhook normalization.** `internal/modules/webhooks/webhook_handler.go:60` defines `WebhookHandler { Processor(), Verify(msg), Normalize(msg), Apply(ctx, dispatcher, msg) }`. `StripeWebhookHandler`, `NMIWebhookHandler`, `CCBillWebhookHandler` all implement it; `WebhookHandlerRegistry` resolves by processor name with NMI-alias fallback. Issue #296 in the codebase notes "adding a processor is implement-the-interface + register, not add-a-branch." A unified `WebhookEvent` shape and `WebhookEventType` enum normalize the event vocabulary across processors.
- **Reconciliation reads.** `internal/reconcile/reconcile.go:175` defines `ProcessorFetcher { Name(), Capabilities(), Fetch(ctx, params) }`. `ProviderKeyer` handles the NMI-alias-vs-gateway-key concern.

**Not abstracted (the actual gaps):**

**4a. Credential resolution is per-processor with three different shapes.**

`internal/modules/checkout/merchant_processor_secrets.go`:
- NMI: `resolveNMIClient(ctx, provider)` (line 57) — per-provider secret name lookup keyed by gateway alias (`SecretNMIMobiusProductionKey`), clones static config, overlays the security key, constructs an `nmi.NMIClient`.
- CCBill: `resolveCCBillConfig(ctx)` (line 100) — single JSON-blob secret (`SecretCCBillAccountConfig`), parsed with field aliasing (`client_acc_num` / `clientAccNum` / `ClientAccNum`), merged with a static base.
- Stripe: webhook secret lookup lives in `WebhookDispatcher`; customer/API key lookup is yet another path scattered through `service.go:1282` (`resolveStripeCustomer`).
- Solana: separate again, through the solana config module.

Four processors, four secret-store shapes, four lookup call sites. Adding a fifth processor today means:
1. Add a `SecretFooXxx` constant somewhere.
2. Write a new `resolveFooClient(ctx)` method on `CheckoutService`.
3. Hand-wire it into every place that needs a Foo client.

The `merchantSecretGetter` interface (merchant_processor_secrets.go:18) is the right primitive but stops one level too low — it gives raw secrets, not constructed processor clients.

**4b. Outbound mutations (charge, refund, void) are routed via the intent ledger but the per-processor logic isn't behind a uniform interface.**

`intents.RefundIntentFor(payment, target, amount, reason)` (used in admin_payments.go:283) returns `(intentType, provider, intentKey, error)` — a content-addressed intent. The actual provider-side mutation runs inside processor-specific intent handlers, each implemented bespoke. The intent system gives uniform *queueing*, *retry semantics*, and *content-addressed idempotency*; it does NOT give a uniform `PaymentProcessor` contract that the rest of the system can program against.

Consequence: code that needs to issue a refund must route through the intent runner. Code that needs to know "can this processor be refunded" (admin_payments.go:195-213 — the processor switch in `prepareAdminRefund`) still hard-codes the answer per processor. A new processor adds a switch case in every such gate.

**4c. Webhook event identity is not part of the `WebhookHandler` contract.**

`Normalize()` populates `WebhookEvent.ProcessorRef` (the processor's event ID), but the deduplication layer (`DeduplicationService.ProcessWebhook` in deduplication.go) takes the event_id as a separate string parameter from the caller, not from the handler. This is why CCBill's missing event_id (covered in OR-IMPL-1) can't be patched purely at the dedup layer — the responsibility for *deriving* an event_id is not in the handler interface. Pushing event_id derivation onto `WebhookHandler` makes the OR-IMPL-1 fix per-processor uniform and forces each new handler to answer the question.

**4d. Subscription lifecycle side-effects are duplicated across processor webhook handlers.**

When a subscription renewal fails, when a subscription is canceled, when a subscription is reactivated — each processor's `Apply()` reaches into `SubscriptionLifecycleService`, `MoneyService`, `NotificationService` with bespoke calls. The `WebhookEventType` enum normalizes the event but the per-event side-effect chain is duplicated. This is a *coupling* problem, not a missing-interface problem; the fix is to move the side-effect chain behind a single `WebhookEventApplier(ctx, evt WebhookEvent) error` that handlers call after `Normalize()`. Whether that's worth the refactor depends on how often new processors land; flag it but don't gate the other steps on it.

### Recommended approach

Layered, in order of leverage. Each step is independently shippable.

**Step 1: `ProcessorCredentialResolver` — unify credential lookup.**

```go
type ProcessorCredentialResolver interface {
    ResolveStripeAPIKey(ctx context.Context) (string, error)
    ResolveStripeWebhookSecret(ctx context.Context) (string, error)
    ResolveNMIClient(ctx context.Context, gateway string) (*nmi.NMIClient, error)
    ResolveCCBillConfig(ctx context.Context) (*config.CCBillConfig, error)
    ResolveSolanaConfig(ctx context.Context) (*config.SolanaConfig, error)
}
```

Implemented once in `internal/modules/payments/credentials/` (or `internal/merchants/credentials/`); reads from `merchantSecretGetter` with static-config fallback. `CheckoutService` and `WebhookDispatcher` both depend on this interface, not on the concrete merchant-secret store. The bespoke `resolveNMIClient` / `resolveCCBillConfig` methods on `CheckoutService` collapse into thin call-throughs and then disappear.

This is the smallest, highest-leverage step: it directly addresses the per-merchant credential plane work that the multi-merchant migration needs, and unblocks moving credential management out of static config (audit OR-API-H3).

**Step 2: Extend `WebhookHandler` with `EventID(msg) (string, error)`.**

```go
type WebhookHandler interface {
    Processor() string
    EventID(msg *WebhookMessage) (string, error)  // new
    Verify(msg *WebhookMessage) error
    Normalize(msg *WebhookMessage) (WebhookEvent, error)
    Apply(ctx context.Context, d *WebhookDispatcher, msg *WebhookMessage) error
}
```

Forces every handler to answer "what is the dedup key for this message." Stripe/NMI return their native event_id; CCBill synthesizes one per the rule defined in OR-IMPL-1a. The webhook ingress code stops passing event_id as a separate parameter; the registry resolves the handler, calls `EventID()`, then `Verify()`, then dispatches through `DeduplicationService.ProcessWebhook`. This locks in OR-IMPL-1's CCBill fix and makes the next processor's dedup story explicit at the type level.

**Step 3: `PaymentProcessor` outbound interface — `Charge`, `Refund`, `Void`, `QueryPayment`, `Capabilities`.**

```go
type PaymentProcessor interface {
    Name() string
    Capabilities() ProcessorCapabilities  // refundable, voidable, supports recurring, supports off-channel, ...
    Charge(ctx context.Context, req ChargeRequest) (*ChargeResult, error)
    Refund(ctx context.Context, req RefundRequest) (*RefundResult, error)
    Void(ctx context.Context, req VoidRequest) (*VoidResult, error)
    QueryPayment(ctx context.Context, ref string) (*PaymentSnapshot, error)
}
```

This sits *between* the intent runner and the processor SDK clients. Intent handlers become 3-5 lines: unmarshal payload, resolve `PaymentProcessor`, call the right method, marshal result. The hard-coded processor switches in `prepareAdminRefund` (admin_payments.go:195-213) become a single `processor.Capabilities().Refundable` check. CCBill's "refunds must be processed through CCBill's admin portal" becomes `Capabilities{Refundable: false}` returned by `CCBillProcessor.Capabilities()`.

`Capabilities()` is the under-rated half of this interface — it's the structured answer to "what can I do with this processor" that the audit's "duplicate processor switch statements everywhere" implies but doesn't articulate.

**Defer Step 4 (subscription-lifecycle applier) unless processor count grows.** Hold the per-processor `Apply()` shape until adding a 5th or 6th processor proves the duplication is real and not a one-time copy.

### Critical files

**Step 1 (credentials):**
- `internal/modules/payments/credentials/resolver.go` (new) — `ProcessorCredentialResolver` interface + default implementation
- `internal/modules/checkout/merchant_processor_secrets.go` — collapse `resolveNMIClient`, `resolveCCBillConfig` into the new resolver
- `internal/modules/checkout/service.go:98` — replace `MerchantSecrets merchantSecretGetter` with the higher-level resolver
- `internal/modules/webhooks/dispatcher.go` — `WebhookDispatcher` consumes resolver for secret lookups, not raw `NMIClients map`

**Step 2 (event identity):**
- `internal/modules/webhooks/webhook_handler.go` — add `EventID(msg)` to the interface
- `internal/modules/webhooks/webhook_handler.go:115, 219, 305` — implement `EventID` on Stripe/NMI/CCBill handlers (CCBill per the OR-IMPL-1a rule)
- `internal/modules/webhooks/dispatcher.go` — webhook ingress calls `handler.EventID()` instead of taking event_id as a parameter
- `internal/modules/webhooks/deduplication.go` — no signature change; the parameter source moves from caller to handler

**Step 3 (outbound):**
- `internal/modules/payments/processors/processor.go` (new) — `PaymentProcessor` interface, `Capabilities`, request/response types
- `internal/modules/payments/processors/stripe.go`, `nmi.go`, `ccbill.go`, `solana.go` (new files; `processors/nmi.go` exists but only carries the gateway-set helpers) — concrete implementations wrapping existing SDK clients
- `internal/intents/refund_handler.go` (and friends) — collapse processor branches behind `PaymentProcessor.Refund`
- `internal/http/handlers/admin_payments.go:195-213` — replace the processor switch with a `Capabilities().Refundable` check
- `internal/modules/checkout/` — `Charge` calls collapse behind the new interface

### Verification

- Unit: contract tests for `PaymentProcessor` and `WebhookHandler` — each concrete implementation must pass the same shared test suite (idempotent refund, charge, query consistency, event_id stability). Adding a new processor means making the contract tests green for it.
- Integration: end-to-end checkout + refund for each of Stripe, NMI/Mobius, CCBill (where supported), Solana through the new interfaces. Existing E2E tests should pass unchanged (the interface is internal).
- Migration test: confirm the static-config fallback in `ProcessorCredentialResolver` still serves embedded/self-hosted single-merchant installs without merchant secrets configured.
- Audit: grep the codebase post-refactor for `case models.ProcessorStripe:` / `case models.ProcessorCCBill:` style switches; the count should drop substantially. The remaining ones should be in places where processor-specific semantics genuinely diverge (Solana on-chain confirmation is a likely holdout).
- Performance: per-request credential resolution latency should not regress (cached resolver state with the same 15-minute TTL as today's secrets cache — see Backlog about pushing toward invalidation).

### Caveats

- This is a *refactor*, not a feature. Land it on a stable base — after OR-IMPL-1 (webhook safety) and OR-IMPL-2 (mutation safety) ship, so the abstraction shape is informed by the post-fix code rather than the pre-fix code.
- The intent-ledger layer is doing real work and should not be replaced. `PaymentProcessor` sits *behind* the intent runner — intents are the durable, retry-safe queue; processors are the bare provider clients. Keep that separation.
- NMI's gateway-alias model (mobius vs. other NMI MIDs) means `PaymentProcessor.Name()` is "nmi" but the underlying gateway key varies. Mirror the existing `ProviderKeyer` pattern from reconcile.

---

## Sequencing

| Step | Issue | Estimate | Rationale |
|------|-------|----------|-----------|
| 1 | OR-IMPL-2a (refund hardening) | ~1 day | Smallest, immediate bug fix |
| 2 | OR-IMPL-1 (webhook safety) | 1-2 weeks | Money-correctness; unblocks operator visibility for multi-merchant rollout |
| 3 | OR-IMPL-2b (`Idempotency-Key` middleware) | ~1 week | Build the middleware as #1 lands; refund handler is its first migrated consumer |
| 4 | OR-IMPL-3 (delegated token freshness) | 1-2 weeks | Different code area — can run in parallel with #2/#3 |
| 5 | OR-IMPL-4 step 1 (`ProcessorCredentialResolver`) | ~1 week | Lands after #1-3 settle; directly enables per-merchant credential UI for the multi-merchant pivot |
| 6 | OR-IMPL-4 step 2 (`EventID` on `WebhookHandler`) | ~2 days | Locks in OR-IMPL-1a structurally |
| 7 | OR-IMPL-4 step 3 (`PaymentProcessor` outbound) | 2-3 weeks | Largest, lowest urgency; do once the post-fix code shape is stable |

Total: 7-9 weeks of focused work to close all four. The first four (~4-5 weeks) close the correctness gaps; the last three (~3-4 weeks) close the architectural gap that will otherwise compound as processors are added.

---

## Backlog — secondary findings, deferred

Surfaced during exploration; not in the top-4 deep-dive but worth tracking.

- **Reconciliation is on-demand only.** `/v1/admin/reconcile/runs` triggers synchronous reconciliation; no scheduled job, no background drift detection. Couple this with the `repair_alerts` / `manual_rebill_attempts` cleanup in OR-IMPL-1c.
- **`merchant_secrets` cache is in-process with 15-minute TTL.** Secret rotations propagate slowly across replicas; a webhook signing secret rotated on Node A causes silent webhook rejection on Node B until the TTL expires. Push invalidation (LISTEN/NOTIFY or Redis pub/sub) would close this — needed if the team scales horizontally.
- **Solana confirm flows lack `SELECT ... FOR UPDATE`.** `solana-tier-change/confirm` and `solana-cancel` mutate DB state without row locks; concurrent confirmations could race. Audit individually.
- **Owner_org_id → merchant binding is implicit.** A service token's authkit org resolves to a merchant via `owner_org_id`; if multiple merchants share an owner_org, or if `owner_org_id` changes, the binding is brittle. Add an explicit binding check at token resolution time.
- **API hygiene from the existing audit** — duplicate service routes, REST verb consistency, error response shape — should be a single focused hygiene PR rather than scattered cleanup.

## Deferred to other planning sessions

- **OR-API-C1 / C2** (single-merchant default; legacy global webhook surface). Coordinate with the multi-merchant migration owners; design the merchant-disambiguation strategy (URL prefix, JWT `org` claim, or subdomain). This is the architectural prerequisite for the per-merchant `StoreConfig` and CORS-from-DB work (audit OR-API-M1, H3).
- **authkit-side issues** (AK-API-* in the audit). Separate repo, separate session.
