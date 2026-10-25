# Move the hosted account product out of core

Status: **reviewed removal plan; an isolated implementation draft may proceed, while merge and release remain gated on destination qualification**. This inventory was read against signed candidate `5dccddefec0a7c3fdcd9ed9484eb3a2125733a03` (generic mapped routes, customer proof resolution, projection removal and proposed v0.264.0 dependency pins), based on fetched master `31cb304e72b30eedc0deab5d72cca54ee07345a6`.

The destination is the SaaS account-policy, proof, linking, lifecycle and UI composition. Core continues to own merchant billing customers, ordinary merchant-scoped authentication, orders, subscriptions, payment authority and generic `CustomerRoutes` / `ResolveCustomer`. The original planning checkpoint removed no source. The separate extraction draft now implements the production removal described below; shipping master and the old critical/browser scenarios remain intact pending destination qualification.

## Current isolated implementation

The draft removes the old service, implicit configuration/hooks, River reader,
network HTTP/client/DTO/query owners, platform sign-in on the generic payment
page, and old retention/archive ownership. Migration0116 preserves released SQL
and refuses populated or conflicting untransferred state. No queued job or live
database was changed. Notification migration0115 remains a separate lane and
must be integrated before the final release ordering is fixed.

The new standalone `CustomerAdmissionPolicy` hook is customer-only and runs
before ordinary admission. Explicit host OAuth resource scopes survive server
composition; canonical billing scope ceilings remain authoritative. Host
consent/resources/group-client declarations are explicit; see
[the host guide](customer-admission-policy.md).

Production root/server builds, SQLC generation/live query preparation and
contract generation pass. Two narrow real-service journeys cover migration
refusal/continuation and actual AuthKit/customer policy behavior, including
OAuth scope isolation and retained old-event reads. SDK and payment-page
production builds pass. These checks do not replace the destination browser,
full combined suite, restored-data rehearsal or release gates below. The narrow
server tests must be integrated into the existing critical fixture/ledger before
merge; they are not intended to become an extra parallel CI suite.

## Gate before merging the removal patch

- Qualify controlled dual-proof linking, explicit isolated-to-shared conversion, replay/conflict handling and original credential liveness in SaaS. Merely creating a platform account or seeing equal IDs/contacts is not proof.
- Qualify unlink, consent withdrawal, account purge, merchant retirement, original AuthKit cursor continuation and bounded pruning. Keep live read authorization independent of eventual event consumption.
- Prove the in-use guard through public billing operations: every non-canceled, non-deleted customer subscription blocks, including unknown future statuses. The old read-before-revoke guard is not atomic against a concurrent new subscription; do not claim a stronger guarantee without an explicit billing contract.
- Qualify SaaS account routes, all notification list/mutation admission, shared checkout/portal call sites and the relevant browser/client behavior. Do not simply switch off old mounts while leaving a hidden old service active.
- Check every native self-admission entry. Removing `AdmitNative` must retain ordinary OSS customer access, while SaaS must not leave a platform-native `/v1/me` path that allocates UUID-equal merchant customers outside its shared/isolated policy. New mapped routes alone do not prove there is no bypass.
- Complete stopped-writer transfer and fresh/restored migration rehearsal, including archive/retirement behavior with the preserved SaaS-to-billing customer FK. Root owns combined release/adoption/rollout.

Shared wallet/provider execution, customer-facing checkout and production qualification remain separate product completion requirements; a schema transfer does not prove those capabilities.

## One coordinated core patch

| Area | Exact source actions |
|---|---|
| Application service | Remove `server/internal/network/{network,accounts,events,config}.go`, `server/network.go`, `server/notification_events.go`, and `internal/customernetwork/network.go` after their SaaS replacements pass. This removes challenge storage, native enrollment, link mutation, terms admission, unlink, lifecycle hooks and the periodic account reader. |
| Construction and hooks | Remove the `network.Service` field/import, `network.Enabled` / `Configure` / `New` / `RiverJobs` wiring and `rt.Network` assignment from `server/server.go`; remove only the network-specific branch in `server/merchants.go`. Retain generic merchant naming, ordinary AuthKit configuration, host hooks and merchant deletion itself. |
| Runtime and customer admission | Remove `Runtime.Network` and `PaymentPageNetwork` from `internal/app/runtime.go`. Remove the network branch from `admitCustomer` in `internal/http/routes/mount.go`, preserving `requireCustomerTenant` and `db.AdmitCustomer`. Keep generic issuer, tenant, credential and customer-existence checks. |
| HTTP product surface | Remove `internal/http/routes/customer_network.go`, `network_host.go`, `internal/http/handlers/customer_network.go`, and `network.go`. Remove `AuthNetwork`, network route group/feature metadata, network mount registration and `networkGates` / `networkCheck` from `route.go`, `mount.go`, and `gate.go`. Remove registration in `server/internal/http/routes_self.go` and `server/internal/http/server.go`. Keep ordinary `/v1/me` operations and generic customer-action sensitivity classification. |
| Public Go contract | Remove the network/link product DTOs and errors from `billing/customer_links.go`, `billing/network.go`, `billing/error_codes.go`, plus `Client.ListNetworkCustomers` in `customers.go`. Remove `PublicConfig.Network` / `PublicNetwork` from `billing/public_config.go` and its assignment in `internal/http/handlers/public_config.go`. Generic customer projection is already absent in this candidate. |
| Payment page | Remove `web/pay/src/network.ts`, `sign-in.tsx`, `SaveWithNetwork` and their imports/dispatch in `app.tsx` / `main.tsx`. Remove the network configuration and issuer origins in `server/internal/http/routes_payment_page.go`, the `/pay/sign-in` mount in `internal/http/paymentpagehttp/paymentpage.go`, and its opener/header/path exception in `internal/paymentpage/page.go`. Preserve ordinary checkout hosting, frame policy and provider/Solana payment controls. |
| SDK | Remove only `createNetworkLinkChallenge`, `createNetworkLink`, their imported schemas/types and exports from `sdk/billing-ui/src/client/{client,index,types}.ts`. Regenerate `generated/{wire,routes,error-codes}.ts` and the Go/OpenAPI/compatibility contracts; do not hand-edit generated definitions. |
| SQL and generated bindings | Remove `internal/db/queries/network.sql`, `network_events.sql`, and product-specific `customer_links.sql` queries when no callers remain. Review `CustomerHasLiveSubscriptions` and `NativeCustomerExists` separately before deleting; do not delete a query needed by a generic caller. Regenerate `internal/db/gen` with the new forward migration included in `sqlc.yaml`. Keep all historical migration files and their bytes. |
| Retention | Remove the customer-link union and cutoff argument from `internal/db/queries/retention.sql`; remove `CleanupResult.CustomerLinks`, its logging and link-deletion sweep in `internal/river/jobs_cleanup.go`; remove only network-link/cursor entries and the obsolete constant from `internal/retention/{tables,retention}.go`. SaaS must supply bounded revoked-link/account-log pruning through its necessary lifecycle pass or explicit host hook; no such wiring can be assumed merely from existing dormancy/notification workers. |
| Archive ownership | Remove only `customer_links` and `network_event_cursors` from `internal/merchantarchive/owned_tables.go`, exclusions and reviewed-column inventory in `checks.go`. Replace the current `billing.guard_billing_restore_receipt` function in a new forward migration without its old link-table entry. Its current definition is in migration0113; do not edit that released file. Qualify restore guards and merchant/customer retirement with SaaS history still referencing billing customers. |
| Existing event log | Stop producing `customer.network_linked` / `customer.network_unlinked`; move product payload construction to SaaS account events. Existing committed core log rows must remain readable for normal retention after public producer constants/DTOs are removed. Audit generic decoder/filter behavior before changing `billing/host_events.go` or `internal/testevents/events.go`. Never rewrite events, splice old/new counters or add source consumers/ACKs. |
| Documentation/fixtures | Update `docs/customer-network.md`, event-log/operations/embedding docs, `config.example.yaml`, `internal/contract/docs.go`, and network-only fixture comments/configuration. Keep historical changelog/release evidence accurate rather than rewriting it as if the feature never shipped. |

Do not remove code merely because a symbol contains “network”: Solana networks, card network tokens, ordinary HTTP transport retry/cancellation and SSRF controls remain core functionality.

## Forward migration and queued work

The candidate ends at migration0114. A separate notification lane already owns `0115_notification_issue_revisions.up.sql`. Allocate the next available number only after coordinating that lane; this plan reserves no core migration number.

Keep the procedural preflight separate from ordinary `DROP TABLE IF EXISTS` statements so SQLC sees the final absent tables. Use no `CASCADE`. The removal migration needs explicit state handling under locks:

1. Intact legacy pair with any link row **or any cursor row**: refuse with an actionable transfer/export requirement. An empty-string cursor row is still reader state. Do not copy, delete or reset populated data.
2. Intact empty legacy pair and no SaaS destination pair: drop the two old tables normally, without `CASCADE`; this is the fresh self-hosted path through the unchanged historical migration chain.
3. Neither old table and a complete, qualified SaaS destination pair: leave SaaS storage and its FK untouched. This is the explicit offline-transfer path. Preserve the exact issuer/tenant/cursor/filter boundary and old retained event-log source.
4. Partial/conflicting source/destination locations or unexpected dependencies: refuse. Do not resolve conflicting owners by deleting a supposedly empty table. If both source tables and both destinations are missing before this migration is first recorded, refuse rather than infer that historical data was empty. Normal already-migrated standalone restarts skip the recorded migration and remain valid.

SaaS transfer PR133 at `8a5768e12927d3a0d9c633637043953776acad4b` qualifies the explicit transactional two-table move. Storage PR135 at `1993da326a6c308a8c530d1227da7377e30dbf73` qualifies fresh destination schema parity and repeated ownership checks. They do not qualify this future core removal migration.

Stop old HTTP writers and River readers across replicas before transfer and prevent old binaries restarting. An exclusive table lock detects an active transaction, not an idle old process. Inventory/cancel only the exact retired job kind `openrails.network_read_account_events` in its configured River schema after stopping its owner; preserve unrelated jobs and financial work. Do not add a compatibility worker that continues reading or writing old network state.

Fresh SaaS initializes core before host storage to preserve the customer FK. Legacy SaaS prepares host migrations without constructing the new engine, explicitly transfers links/cursors, then applies the new core migration. A rejected transaction rolls back; a lost commit reply requires inspecting both schemas. Reverse transfer after new SaaS writes is not qualified by a source revert.

## Coverage handoff before deleting old scenarios

| Existing obligation | Destination or retained core obligation |
|---|---|
| `OR-J02-S01` — both real identities link through consented one-use proof | SaaS controlled-link real AuthKit/HTTP/storage journey, with preserved merchant customer/financial identity and actual client/nonce/terms metadata. |
| `OR-J02-S02` — foreign nonce/customer and expired challenge have no effect | SaaS wrong-client/merchant/customer, expired/replayed challenge, foreign realm, stale/deleted original credential, conflict and unavailable-authority tests. |
| `OR-J02-S03` — account alias is only its linked owned customer | SaaS account-route tests plus retained generic `CustomerRoutes` tests, including recipient selection before notification pagination and mutation authorization. |
| `OR-J02-S04` — stale session cannot unlink without fresh sign-in | SaaS lifecycle/HTTP recent-auth tests; keep generic sensitive-action classification and identity-forwarding coverage in core. |
| `OR-J02-S05` — live provider agreement blocks unlink; terminal cancellation preserves billing history | SaaS lifecycle uses real public billing import/subscription/cancellation operations, preserving payment/history records while access is revoked. |
| `OR-UI-06` in `sdk/billing-ui/critical/client.spec.ts` | Move the real consented OAuth/browser linking path to the SaaS client/UI suite before removing the generic SDK methods and their browser fixture. Go-only service checks are not a browser qualification claim. |
| Network-link calls inside `OR-UI-03` | Remove or move only those calls/assertions; retain its unrelated typed checkout-error checks. |
| `OR-J04-S03` and the rest of public SDK/browser coverage | Retain: “network retry” here means transport, not the removed account product. Remove only account-network fixture wiring and `ListNetworkCustomers` assertions in `critical/public_sdk_test.go`. |
| `OR-J01-S09` mapped standalone no-implicit-enrollment | Retain, adapting the absence assertion to removed routes/tables rather than deleting the scenario. |
| `OR-J07-S05` customer projection independence | Retain the customer identity/contact/balance/credit and foreign-access behavior; adapt its old-table-rename fixture to the already-absent storage. |

`critical/network_contract_test.go` currently owns the five J02 paths. Update `critical/{coverage,executables,http_contract}.json`, browser `critical/{methods.tsv,check-results.mjs}` and generated HTTP/SDK inventories only after the corresponding destination paths are qualified. Do not replace real security/money assertions with route-existence checks or merely reduce expected counts.

Keep the shared standalone fixture machinery in `critical/standalone_fixture_test.go` and `critical/server-fixture/server_test.go` needed by generic authority, mapped-route, proof and billing tests. Remove only network-specific operations after mapping all callers. Preserve focused identifier, signature, monetary and parsing vectors.

## Qualification order

1. Freeze destination controlled-link/lifecycle/schema/route commits and their actual executed coverage map.
2. Apply the bounded removal patch with a new forward migration; regenerate SQLC, public contracts, SDK and required test inventories.
3. Run selected local negative/security, fresh/legacy/refusal/restore and ordinary self-hosted checkout checks. Run the retained core financial suite once on the combined ready PR, with the required conservative CI gate.
4. Publish the tested generic core and paired server/adapters. Qualify SaaS against those public versions, with no implicit old-owner activation or policy bypass.
5. Rehearse the owned restored-data cutover/rollback, then build and deploy the same qualified artifact through the root release lane. Provider shared-wallet execution remains its own evidence boundary.

The original planning branch performed no source deletion. This separate implementation draft is now active; no migration application, source activation, hosted CI, tag or deployment has occurred. Shipping master remains unchanged.

## Existing admission controls and the remaining seam

`server/internal/http/server.go` always calls `registerSelfServiceRoutes`. `LocalSignIn=false` changes AuthKit sign-in/registration/console exposure; it does not disable `/v1/me`. `RouteGroups` deliberately controls only staff/programmatic groups. Public `server.Config` and `Deps` contain no separate customer Auth override; the old internal comment mentioning `DelegatedAuthenticator` is not an available public option. Embedded `Engine.Routes` likewise always adds customers, while a standalone engine refuses that method and directs hosts to `Server.Routes`.

The automatic path is `registerSelfServiceRoutes → RegisterCustomerRoutes → customerCheck → customerIssuer → admitCustomer → db.AdmitCustomer`. Today its platform-account branch goes through `rt.Network.AdmitNative`. Simply removing that branch preserves automatic native customer allocation, so SaaS needs an explicit customer-only policy before this admission step. Do not use URL-prefix heuristics, reauthenticate the request, alter its original verified identity, or deny native staff at the whole server. Generic mapped routes already take a separate path and must remain separate.

The approved seam is `server.Deps.CustomerAdmissionPolicy(context.Context, auth.Verified, openrails.CustomerBinding) error`, implemented in the isolated draft. It must preserve the default OSS behavior and let SaaS require its explicit policy/link resolution for native platform and isolated accounts while retaining legitimate merchant-issued customer APIs.

| HTTP entry and original credential | Acceptance requirement |
|---|---|
| Default OSS `/v1/me`, valid self-acting merchant-bound native credential | Preserve ordinary merchant customer behavior and issuer/tenant checks. |
| SaaS automatic `/v1/me`, platform-native credential with valid merchant binding and self scope | Refuse before allocating or reading a UUID-equal customer outside the account policy. Test both missing and pre-existing matching customer rows. |
| SaaS automatic `/v1/me`, native isolated-realm credential | Require its explicit mapped/account-policy path; no fallback to direct subject UUID admission. |
| SaaS automatic `/v1/me`, legitimate merchant assertion or trusted merchant customer issuer | Preserve merchant-specific access and normal first admission; create no platform relationship. Unsupported/unknown credential metadata fails closed. |
| Native staff credential on merchant/admin routes | Preserve normal permission-checked staff behavior; customer policy does not blanket-deny staff. |
| AuthKit local sign-in endpoints | Remain available for the hosted platform; do not misuse `LocalSignIn=false` as customer policy. |
| SaaS mapped platform customer route | Require the exact current owned active link; preserve original liveness and sensitive-action recent-auth checks. No auto-enrollment. |
| SaaS mapped isolated customer route | Require the exact issuer/tenant/subject and merchant relationship; no shared wallet or portal authority. |
| Explicit shared/isolated checkout enrollment | Apply current policy/revision and terms before fixing a billing customer and admitting its order. |
| Existing pay-URL payment surface | Remain scoped to the immutable order's payer; it grants no platform history/link authority and does not enter native auto-admission. |

The SaaS migration fixtures also need a version boundary. Current `platform_link_reader_integration_test.go` creates legacy proof through core network HTTP and compiles against `Server.CustomerLinks` / `UnlinkCustomer`. Removing those public methods requires a separate legacy fixture module/process pinned to the last qualified old-owner release, while destination runtime/tests compile against the removal candidate. Do not keep product APIs in core solely for historical fixture setup.
