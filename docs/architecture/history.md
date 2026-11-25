# History

Append-only history of what changed, who changed it and how, in one envelope: configuration history (`change_log`), every change to the catalog, to a merchant's configuration documents and to its controls, and operations history (`operation_log`), each phase of each provider operation, written before the call. A subscription's history reads the operations history beside its own transitions. What each system records is on its page: [catalog](catalog.md), [merchant configuration](merchant-configuration.md), [payment providers](payment-providers.md). Integration guide: [`ListChanges`](../client.md#merchant-configuration-and-psps).

## Concepts

- **Entry** (`billing.LogEntry`, id `log_`): when (`occurred_at`), the actor (`billing.LogActor`: `staff:<invoker>` as the route gate admitted it, `customer:<invoker>`, `provider:<rail>`, or `system`, `file` and `vault` where no person acted), the source (`billing.LogSource`: `api`, `console`, `customer`, `catalog_apply`, `cli`, `reload`, `job`, `intent`, `webhook`, `provider_read`), the object (`billing.LogObjectType` and its id), the action (`billing.LogAction`), a versioned payload (`history.Version`), and an optional reason, request id and operation id.
- **Configuration history** (`change_log`): products, price keys, meters and limits (`create`, `update`, `archive`, `unarchive`, `delete`, `key_add`, `key_remove`, `key_rename`, `move`), the merchant, messages, PSP and custodian documents, each changed field before and after, and the merchant's controls (`controls`: writes, rebills, destructive arming; `controls.Set`).
- **Operations history** (`operation_log`): each phase of a provider operation (attempting before the call, then its result or a hold), with its address: rail, PSP or custodian, and subscription.

## How it works

**Writing.** `history.Record` writes entries in the caller's transaction, so an entry commits and rolls back with the change it records; the catalog's commit (`catalogHistory`) and configuration writes (`RecordConfigWrite`) use it. `history.WriteAhead` writes on the pool, each entry its own statement outside any business transaction: an operation's `attempting` entry is written before the provider call and is kept whatever follows; recovery only appends. Every entry names its actor and source, and an entry names an address if and only if it is in the operations history. A credential never enters the history: its field reads `"changed"` (`history.Changed`). An entry over the log's bounds keeps the start of its reason and evidence and says `truncated` (`Operation.Fit`).

A price change that moves its key adds `move: {price_id, archived}` to its fields payload. A movement with no other price change has a standalone `move` entry with `version`, `revision`, `price_id` and `archived`. `ListPriceHistory` reads both payloads.

**Reading.** `ListChanges` (`GET /v1/merchant/changes`, `history.List`) pages one merchant's configuration history newest first, by object and time (`billing.LogListParams`). `ListPriceHistory` (`GET /v1/catalog/prices/{id}/history`) reads a price key's moves (`billing.PriceKeyMovement`). `GetSubscriptionHistory` (`GET /v1/admin/subscriptions/{id}/history`) merges a subscription's transitions, payment attempts, billing changes and provider operations. `openrails intents-log --merchant M` reads the operations history by rail, operation, PSP and phase.

## Interfaces

- Go: `Client.ListChanges`, `ListPriceHistory`, `GetSubscriptionHistory`; `billing.LogEntry`, `LogListParams`, `LogActor`, `LogSource`, `LogObjectType`, `LogAction`.
- HTTP: `GET /v1/merchant/changes` (`merchant:config:read`), `GET /v1/catalog/prices/{id}/history` (`merchant:catalog:read`), `GET /v1/admin/subscriptions/{id}/history` (`merchant:customer:read`).
- CLI: `openrails intents-log`.

## Guarantees

### 1. Exactly-once provider calls
The operations history is part of how [payment providers](payment-providers.md#1-exactly-once-provider-calls) keep this: an attempt's `attempting` entry is written ahead of the call, so a process that dies mid-call leaves the evidence that it may have been sent.

Checks:
- OR-J43-S08: the `attempting` entry outlives the process dying mid-call, and recovery only appends; OR-J43-S11 and S13: the breaker's sent mark and its phases.

### 2. Agreement with providers
Does not apply: the history records; the systems that agree with providers decide.

### 3. Exact money
Does not apply: entries carry field values and evidence, not balances.

### 4. Access control and tenant isolation
Each read route has its permission; every query in `change_log.sql` and `operation_log.sql` names `merchant_id`, and an entry's actor is the route gate's invoker, never a value the request names.

Checks:
- OR-J43-S05: configuration history lists one merchant's entries, to merchant configuration readers only.
- OR-J43-S03: a change through the API is recorded with its actor, and a credential only as changed.

### 5. Contracts match the code
The read routes are catalog entries that generate their `Client` methods, OpenAPI and TypeScript types; `billing.LogEntry` and its enums are in `api/go.txt`, and payloads carry `history.Version`.

Checks:
- `go run ./scripts/contracts` shows no removed or changed line here.

### 6. Data lifecycle
`change_log` is permanent and append-only (`immutable_change_log`, `immutable_change_log_truncate`): a merchant purge keeps it, the merchant archive moves it, and a restore adds the archive's entries to the destination's own, once per merchant. `operation_log` rows are deleted 25 months (761 days) after `occurred_at`, in the hourly cleanup's bounded batches; no update reaches one (`immutable_operation_log`). Neither is partitioned.

Checks:
- `internal/retention/tables.go` classes `change_log` `Permanent` and `operation_log` `Rows`; both are in `internal/merchantarchive/owned_tables.go`.

### 7. Work scales with activity
Entries are written by the change or the operation they record; nothing scans the history but the retention sweep, on `occurred_at`.

### 8. Abuse protection
Does not apply: the history has no entry point of its own; its read routes are staff routes.

### 9. Destructive-action safeguards
Nothing deletes configuration history, and `openrails undo-run` restores neither log (`NeverRollbackableTables`).

Checks:
- `NeverRollbackableTables` lists `change_log` and `operation_log`.

## Tests

- OR-J43-S01 to S07: catalog and configuration changes recorded with their transaction, document, actor and revision; outside writes recorded once across replicas; the list route.
- OR-J43-S08, S11, S13: operations written ahead, the breaker's sent mark, a parked or stale claim.
- OR-J43-S09: a subscription's history reads its provider operations once each.
- OR-J43-S10, S12: a price key's moves, and a price migration's source archive recorded once.
