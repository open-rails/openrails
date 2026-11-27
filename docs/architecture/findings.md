# Findings and alerts

The staff review queue and how merchants hear about it. Reconciliation, converge, configuration checks, webhook parks and pay URL failures record what needs a person as findings; a provider read's finding that carries a change is a proposal a person approves or dismisses; a live provider subscription converge cannot land is parked with a finding until its cause is fixed. Findings that need review alert the merchant by email and alert webhook. Staff work the queue in the console ([billing-ui](billing-ui.md)), with the Go client or with the `openrails` CLI. Integration guides: [proposals and findings](../client.md#proposals-and-findings), [alerts](../client.md#alerts).

## Concepts

- **Finding**: a row of `reconciliation_findings`, one per merchant, type, PSP and subject (`subject_key`), so a re-detection updates it instead of adding one. Types are namespaced by plane (`pull`, from provider reads; `derive`, `life`, `consistency`, `notify`, `catalog`, `books`, `mrr`: `reconciliation_findings_type_check`); severity is `low` to `critical`; status is `reconcile_required`, `requires_review` (the queue), `auto_fixed`, `fixed` or `ignored`.
- **Proposal**: a `pull.*` finding carrying a `change` and the basis it was computed on; only `proposals.Executor` applies it.
- **Approval run**: one approve call's applied changes, reversible by `openrails undo-run`.
- **Park**: a `parked_provider_subscriptions` row: a live provider subscription converge cannot land (`billing.ParkReason`: `price_unmapped`, `customer_unlinked`, `product_held`).
- **Alert**: one notification episode of a finding, or a destructive hold, sent to the alert email and alert webhooks.

## How it works

**Findings.** A provider read records its findings with `reconcile.PGStore.UpsertFinding` and resolves the open ones it no longer sees (`AutoResolveVanished`); other engines raise one with `RaiseFinding` and close it with `ResolveRaisedFinding` when the subject recovers. `ListFindings` answers open findings unless a status is named, most severe first, then oldest. `ResolveFinding` resolves one at a time; there is no bulk route. `approve` runs the finding's structured recommendation (`recommend.FromEvidence`, `override_params` merged) and marks it `fixed`; a failed run leaves it open with the error in its notes (`AppendFindingNotes`). Approving a recommendation that refunds, cancels, or grants or revokes a product also needs the permissions of the staff routes whose effect it runs (`handlers.FindingActionRoutes`). `ignore` needs notes and silences the subject for good. A finding with no recommendation answers `finding_not_actionable`, a `pull.*` finding `finding_requires_merchant_config` (it is a proposal), and `life.destructive.hold` `permission_required` (it clears with its hold).

**Proposals.** `ListProposals` answers open proposals unless a status is named, most severe first. `ApproveProposals` takes 1 to 100 ids (`billing.MaxBatchItems`) with an `Idempotency-Key` that replays the first answer and refuses another body (`idempotency_key_reused`); an unknown id or one that is not a `pull.*` finding refuses the whole call before anything runs. Each id runs in its own transaction: a basis that moved or evidence older than seven days resolves it `superseded`, applying nothing; disarmed destructive actions hold a change that ends access or writes to a provider (`destructive_actions_held`), and readonly writes one that moves money or writes to a provider (`provider_writes_held`), both left open (`held`); a failure leaves it open with the error in its notes (`failed`). A cancellation is admitted through the destructive policy before it applies. The first change applied opens the call's approval run, so one call is at most one run. `DismissProposals` closes proposals with required notes; a dismissed one is raised again only on other evidence. `openrails proposals foreign` (`proposals.Executor.Foreign`) closes a provider schedule nothing local names as not the merchant's. A PSP import's corrections are approved as one set when a person applies the import (`ApproveSet`).

**Parking.** Converge parks a live provider subscription it cannot land (`parking.Park`) and raises its finding (`catalog.price.unmapped` or `life.subscription.unattributed`), moving no money and canceling nothing. Fixing the cause (mapping the price, linking the customer) wakes converge in the fixing transaction (`parking.Wake`); a trigger on `billing.subscriptions` closes the park when the subscription lands by any path (`landed`), and converge closes it when no longer needed (`converged`) or when the provider ended it (`gone`). Park and wake take the same advisory lock per cause, so the subscription lands once. While the PSP's import owns the subscription (`parking.ImportOwns`) the park raises no finding. `ListParkedSubscriptions` pages them newest first, by PSP, reason and release.

**Merchant alerts.** A finding's engine calls `alerting.Service.NotifyFinding` after persisting it (reconcile and converge engines, configuration evaluation, webhook parks, pay URL failures), and `alerting.RecordLedgerRepair`, `alerting.RecordCrankerGas`, `alerting.RecordMessageRateLimit` and `alerting.RecordMessageSenderRefused` notify the findings they raise, and so does the lifecycle for `life.terminal_outcome.held`, through `alertjob.Notify`, the claim and queue the others share. Only `requires_review` findings notify. In one transaction `ClaimReconciliationFindingNotification` stamps `notified_at` and `notified_severity` (first time, or a strict escalation) and an `openrails.operator_alert` job (`DeliveryArgs`) is queued; a low finding stays in the console. A destructive hold queues its alert in the hold's own transaction (`projectDestructiveHold`). `OperatorAlertWorker` snapshots destinations (`Service.Channels`: alert email for high, critical and holds; enabled alert webhooks for medium and above and holds) and queues one job per channel. `Service.Deliver` re-reads the finding and the destination and sends nothing for an ended episode or a removed or disabled destination. Email goes through `Deps.Email` (`alerting.MailSender`), from the merchant profile's `from_email`. Webhook bodies come from `shapeWebhookBody`: generic `Alert` JSON, Slack `{text}`, Discord `{content}`. An undeclared account's finding (`merchantdocs.UndeclaredAccountFindingType`) alerts once per episode; a load of the merchant's configuration that finds the account declared again, or holding no card and no subscription, resolves it (`recordRefusedDocuments`): a configuration change, or the first load after a restart.

Alert webhooks live in the merchant document (`AlertWebhooks`), not PostgreSQL; their writes mount only where Vault holds merchant configuration (`FeatureMerchantConfigManages`). The API returns `destination_host`, never the URL. The console's bell counts `open_findings` with `finding_status: requires_review` through the metrics API every 30 seconds.

## Interfaces

- Go: `Client.ListFindings`, `GetFinding`, `ResolveFinding`, `ListProposals`, `ApproveProposals`, `DismissProposals`, `ListParkedSubscriptions`, `ListAlertWebhooks`, `CreateAlertWebhook`, `UpdateAlertWebhook`, `DeleteAlertWebhook`.
- HTTP, all `merchant:config:read` to read and `merchant:config:manage` to change: `GET /v1/merchant/findings`, `GET /v1/merchant/findings/{id}`, `POST /v1/merchant/findings/{id}/resolve`; `GET /v1/merchant/proposals`, `POST /v1/merchant/proposals/approve` and `POST /v1/merchant/proposals/dismiss` (a person only); `GET /v1/merchant/parked-subscriptions`; `GET|POST /v1/merchant/alert-webhooks`, `PATCH|DELETE /v1/merchant/alert-webhooks/{id}`.
- CLI: `openrails proposals` (`list`, `show`, `approve`, `dismiss`, `foreign`), `openrails undo-run`.
- Configuration: the merchant's `alert_email` and alert webhooks.
- Jobs: `openrails.operator_alert`.

## Guarantees

### 1. Exactly-once provider calls
A proposal's provider writes go through `provider_intents`: a refund is queued with a key derived from the payment and the proposal (`intents.RefundIdempotencyKey`), a schedule cancel through `ScheduleProviderCancel`. Alerts call no payment provider; an alert job is unique by its arguments.

Checks:
- `refundCharge` and `cancelSchedule` in `internal/proposals/execute.go` only queue intents.
- `DeliveryArgs.InsertOpts` deduplicates by args across states, completed included.
- OR-J06-S64: a finding alert whose first delivery fails is delivered by its job's retry, once.

### 2. Agreement with providers
A finding records a disagreement; a proposal applies only while what it was computed on still holds (`basis`: the subscription's row version, and evidence at most seven days old), else it is `superseded`. A provider read resolves the findings it no longer sees; a park closes when its subscription lands by any path.

Checks:
- OR-J38-S32: a stale approval is superseded; OR-J38-S24: a row that moved after the roster read gets no state proposal.
- OR-J15-S09 to OR-J15-S14: a parked subscription lands exactly once when its cause is fixed or an import lands it.

### 3. Exact money
Does not apply: findings, parks and alerts carry evidence, not balances; a proposal that records or refunds a charge does so through the systems that own the money ([refunds](refunds.md), [payment providers](payment-providers.md)).

### 4. Access control and tenant isolation
Every route here is a merchant-configuration route (`merchant:config:read` to read, `merchant:config:manage` to change); approving a finding's customer action also asks for its routes' permissions (`merchant:customer:manage`), and approving and dismissing proposals is `Person: true`. `proposals.Executor` and `reconcile.PGStore` act on the merchant on the context (`merchant.Require`). Alert webhook URLs are SSRF-guarded.

Checks:
- Every query in `reconciliation.sql` and `parking.sql` names `merchant_id`, but `ListActiveMerchantIDs` and `ConvergeObservedFrom`.
- OR-J15-S16: parked subscriptions are listed for merchant configuration readers only; OR-J01-S21: a configuration-only key is refused a refund finding and still resolves an acknowledgement.
- OR-J06-S05 refuses private and metadata alert webhook URLs.

### 5. Contracts match the code
Each route is one catalog entry (`ops.go` and `merchant.go` in `internal/http/routes`) that generates its `Client` method, OpenAPI and TypeScript types; the CLI's `proposals` commands run the same `proposals.Executor` (`internal/hosttools`). `billing.Finding`, `ProposalResults`, `ParkedSubscription` and `AlertWebhook` are in `api/go.txt`.

Checks:
- `go run ./scripts/contracts` finds `client_gen.go`, `api/openapi.json`, `api/go.txt` and the route tables current.
- `TestClientProbe` sends each catalog route's `Client` method and checks its method, path and query.

### 6. Data lifecycle
`internal/retention/tables.go` classes `reconciliation_findings` as rows (resolved findings deleted 366 days after they were resolved and last seen) and `parked_provider_subscriptions` as rows (released parks deleted 90 days after `released_at`); each table comment states its class. Neither is partitioned. Alert webhooks are merchant configuration, not rows.

Checks:
- `CleanupExpiredDataWorker` deletes resolved findings and released parks past `retention.ResolvedFindings` and `retention.ReleasedParks` in bounded batches (`DeleteResolvedFindingsBefore`, `DeleteReleasedParksBefore`).

### 7. Work scales with activity
Alerts are queued by the claim that opens or escalates an episode; nothing re-alerts an open one. A fix wakes its parks in its own transaction (`parking.Wake`); nothing sweeps parks or findings.

Checks:
- `ClaimReconciliationFindingNotification` claims only a first notification or a strict escalation.

### 8. Abuse protection
No customer-facing entry point. Outbound alert webhooks are SSRF-guarded: `httpx.Policy` validates each URL on write, before each delivery, and at the dialer on every redirect hop.

Checks:
- `CreateWebhook` and `UpdateWebhook` answer `webhook_invalid` for a non-public host.
- `deliverOnce` re-validates the URL before posting.

### 9. Destructive-action safeguards
Approving proposals is a person's act, at most 100 ids a call; each cancellation is admitted through the destructive policy before it applies, disarmed destructive actions and readonly writes hold what they cover, and each call's changes are one approval run `openrails undo-run` can reverse. A destructive hold's finding, alert job and host event commit with the hold, alert at critical, go to every channel, and cannot be resolved around the hold.

Checks:
- `holdFor` holds access-ending and provider-writing changes while destructive actions are disarmed, and money-moving and provider-writing ones while writes are readonly (OR-J38-S32).
- OR-J38-S71: proposal cancellations spend the actor allowance per affected subscription and a replay spends nothing.
- `projectDestructiveHold` inserts the finding, the alert job and the host event in one transaction; `Service.Channels` selects email and webhooks for a hold regardless of severity.
- OR-J34-S04: a hold's finding cannot be resolved around the hold.

## Tests

- Findings: OR-J37-S09 (a cranker low-gas finding raised and resolved), OR-J06-S65 (silent webhooks), OR-J29-S37 and OR-J29-S73 (stuck and abandoned operations), OR-J34-S04 (a hold's finding).
- Proposals: OR-J38-S24, OR-J38-S29, OR-J38-S32, OR-J38-S71.
- Parking: OR-J15-S08 to OR-J15-S17, OR-J15-S20, OR-J15-S21, OR-J38-S33 (mapping a parked signup's price from the console).
- Alerts: OR-J06-S87 (an undeclared account alerts once), OR-J06-S64 (failed email retried, sent once), OR-J06-S05 (webhook URL refusal), OR-J06-S70 (a held terminal outcome), OR-J06-S71 (distinct repairs of one transaction).
- Gaps: no test delivers an alert webhook (generic, Slack or Discord body, `Idempotency-Key` reuse) or an escalation; `internal/proposals` has no tests of its own and `internal/modules/alerting` only `channels_test.go`.
