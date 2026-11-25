# Customer messages

OpenRails emails a merchant's customers about their billing: receipts, payment problems, cancellations and price changes. Each message comes from a fact in the [event log](events.md); a merchant turns an event type off to send that message itself, and can replace the built-in templates. With an AuthKit account inbox wired, selected notices also go there. Integration guides: [customer messages](../guide.md#customer-messages), [the client](../client.md#customer-messages).

## Concepts

- **Customer message**: a row of `customer_messages`: one fact's message on one channel (`email`) and what became of it (`billing.MessageStatus`). It keeps no body.
- **Messages document**: a merchant's `messages` document (`billing.MessageSettings`): which event types send, and templates that replace the built-in ones.
- **Issue key**: the exact inbox identity of a notice: `subscription:<id>:<kind>`, `order:<id>:<kind>` or `event:<id>`, with a revision.

## How it works

**Queueing and sending.** The `AFTER INSERT` trigger `notification_event_customer_message` runs `billing.queue_customer_message`: in the fact's own transaction it inserts one `pending` `customer_messages` row per channel (`ON CONFLICT (merchant_id, event_id, channel) DO NOTHING`) and `pg_notify`s `openrails_customer_messages` with the schema; it inserts nothing while `billing_restore_active`. `messages.Service.Run` (started by `Runtime.StartWorkers` on worker processes) listens and calls `Drain` on every wake and reconnect; `openrails.customer_messages_dispatch` (`DispatchArgs`, every `DispatchInterval`, five minutes) drains what no listener saw. `Drain` claims pending rows oldest first through `customer_messages_pending_idx` (`ClaimPendingCustomerMessages`, `FOR UPDATE SKIP LOCKED`, 100 per transaction), marks them `queued` and inserts each one's `openrails.customer_message` job (`SendArgs`, unique by args, completed included; `SendAttempts`, ten) in the same transaction.

`Service.Send` settles a message in order, each stop where it ends: `Enabled` (the document's `enabled`, else the default: `defaults`, refined by `byDefault`, which turns `premium_ended` on for `confirmedEndings` and off a `payment_method_failed` whose `recovery_action` is `resubscribe` and a payment issue from `checkoutSources`) or `disabled`; `CustomerMessageOnTestPSP` (the fact's `psp_id`, or any PSP of its payment, order or subscription, with `psps.live` false) or `test_environment`; no `Deps.Email`, `failed`/`no_sender`; the directory's contact (`identity.Directory.Contacts`) or `no_contact`; an address `net/mail` parses within 320 bytes or `failed`/`invalid_recipient`. It renders (`Deps.MessageRenderer` when it takes the message, else `Render`), then `claim` takes `LockCustomerMessages` (a per-customer transaction advisory lock), counts `CountCustomerMessagesSince` and either marks it `sending` with the recipient or, past `RateCap` (ten an hour), `rate_limited` with `alerting.RecordMessageRateLimit`'s `life.customer.messages_rate_limited` finding. `config.EmailSender.Send` follows; `SettleCustomerMessage` marks `sent`, back to `queued` for a retry, or `failed`: `refused` on a 5xx to `RCPT TO` (`refusalOf` reads helpers/smtp's step text), `send_failed` or `unavailable` on the last attempt. A 5xx to `AUTH` or `MAIL FROM` retries and raises `alerting.RecordMessageSenderRefused`'s `life.customer.messages_sender_refused`, which `alerting.ResolveMessageSenderRefused` closes on the next send or test the sender accepts. `settle` runs on `context.WithoutCancel` with its own timeout, so a timed-out last attempt still ends. From is the profile's `from_email` and display name, else the sender's own.

**Templates.** Built-in templates (`templates/en/<type>.subject|.txt|.html`, one `layout`) are a `billing.MessageSettings` the merchant's document overlays per event type and language. Subject and text are `text/template`, HTML `html/template`, over `billing.MessageData` (version `MessageDataVersion`), with `missingkey=error` and `functions` (comparisons, logic, `len`, `default`, `upper`, `lower`); amounts are `moneyutil.FormatAmount`. `compile` parses each part alone and `vet` refuses `range`, `define`/`block`, variables, a `template` call but the layout's one body, and every other function; a part's source is at most `maxSource` (64 KiB) and renders through `capped` (256 KiB). `typecheck` walks every branch's field chains against `billing.MessageData` by reflection, refusing arguments to a field and comparisons of unlike kinds. A document is checked by `Check` (every event type against its `Sample` in every language it names) where it loads: `checkMessages` in the app's merchant configuration sync and `merchantbootstrap.ScopeDeclaration`; a refused one is `Rejected` under `merchantdocs.MessagesDoc` and the version served before keeps serving. `Service.Preview` renders over a sample fact with the merchant's own profile and an optional candidate template; `Service.Test` sends that rendering to the merchant's `alert_email` only, `TestCap` (ten) an hour per merchant, counted in the abuse state (`Service.Abuse`).

**What the facts carry.** Endings record their cause for the confirmation: `CancelMembershipTx` (`CancelMembershipParams.EndedReason`: `admin` for staff, `refund` for a refund), `FailMembership` (`expired` or `non_recoverable` when a decline ends the subscription) and `ExpireAwaitingMethod` (`expired`); the converge `notifyPass` records `access_ended`. `refund_completed` is written by `PaymentService.queueRefundReceipt` where a refund settles (one the provider reported only within `RefundReceiptWindow`, seven days of its refund time), with `voided` for a reversal `VoidTransactionID` names. `premium_started` carries the first period and, when charged, the amount (`membershipCharge`).

**Account inbox.** With `Deps.Notifications` set, `publishInbox` runs inside `NotificationService.create`'s transaction. `inboxKey` selects payment issues (`notificationinbox.PaymentIssueKinds`) not caused by a checkout or change click, `premium_ended` with `access_ended`, and `premium_renewed` with `downgrade_applied`; every other fact stays in the log only. `issueRevision` upserts `notification_issue_revisions` under `notificationinbox.RequireOrdering` (a per-merchant shared advisory lock, refused while `notification_channel_state.restore_required`), then `Sink.Publish` joins the same transaction. `notificationinbox.Withdraw` bumps and tombstones only keys that were published: on settlement (`WithdrawOrderPaymentNotices`, `WithdrawOrder`), an accepted card (`WithdrawMethodNotice`), confirmed payment or reinstatement (`WithdrawSubscriptionPaymentNotices`) and restored access. The channel's `Recipient` maps the exact issuer and customer UUID; `adapters/authkit.Notifications` skips other merchants and unlisted issuers. `LocalClient.ExportNotificationAdoption` takes `LockOrdering` for its snapshot; `RestoreNotificationOrdering` re-arms the channel after an archive restore.

**Email.** One sender per deployment: `Deps.Email` (`config.EmailSender`: `Send`, `CheckHealth`); nil sends none. `smtpmail.EmailSender` (`adapters/smtpmail`) wraps a `helpers/smtp` sender a host opened; the standalone server builds one from `email_smtp` (`hostdeps.NewEmailSender`; it does not connect until it sends) and lends it to AuthKit for account email (`controlplane.AuthKitSender`). Neither sets a `Message-ID`: helpers/smtp mints a random one, so a send repeated after a crash is not deduplicated by mail clients. The same sender carries merchant alerts ([findings and alerts](findings.md)).

## Interfaces

- Go: `Client.ListCustomerMessages`, `PreviewMessage`, `CreateTestMessage`; `LocalClient.ExportNotificationAdoption`, `RestoreNotificationOrdering`; `Deps.Notifications` (`openrails.NotificationChannel`), `Deps.Email`, `Deps.MessageRenderer`; `billing.MerchantProfile.NotificationURL`.
- HTTP: `GET /v1/admin/customers/{customer_id}/messages` (`merchant:customer:read`), `POST /v1/merchant/messages/preview` (`merchant:config:read`), `POST /v1/merchant/messages/test` (`merchant:config:manage`).
- Configuration: the merchant's `messages` document and `alert_email`, `email_smtp`.
- Jobs: `openrails.customer_messages_dispatch`, `openrails.customer_message`, `openrails.cleanup_expired_data`.

## Guarantees

### 1. Exactly-once provider calls
Does not apply: this system calls no payment provider. An email is sent at least once: the message is marked `sending` before `Send` and `sent` after it, so a crash between the two can send it once more (no deterministic `Message-ID`: the sender mints its own).

### 2. Agreement with providers
Does not apply: facts are written by the systems that agree with providers. A message reads the fact's PSP only to send nothing about money a test PSP moved.

Checks:
- `CustomerMessageOnTestPSP` reads `psps.live` of the fact's payment, order and subscription.

### 3. Exact money
Messages format each amount once, at its currency's registered scale; the message log keeps no amount.

Checks:
- `messageData` formats every amount with `moneyutil.FormatAmount` and its fact's currency; `TestReceipts` pins `12.50 USD` and `500 JPY`.

### 4. Access control and tenant isolation
The message log is a support read (`merchant:customer:read`); a preview is a configuration read and a test a configuration change. A send job binds its merchant (`merchant.WithID`) before it reads. Inbox recipients are mapped by exact issuer and UUID.

Checks:
- Every query in `customer_messages.sql` but `ClaimPendingCustomerMessages` (marked `CROSS-MERCHANT`) names `merchant_id`.
- OR-J39-S16: another merchant's credential lists none of the customer's messages.
- `adapters/authkit.Notifications` skips another merchant and an unlisted issuer.

### 5. Contracts match the code
Every route here is one catalog entry (`internal/http/routes/messages.go`) that generates its `Client` method, OpenAPI and TypeScript types; the in-process and remote `Client` both send it through `Client.send`. `billing.CustomerMessage`, `MessageData` and `MessageSettings` are in `api/go.txt`, and templates read `MessageData` of `MessageDataVersion`.

Checks:
- `go run ./scripts/contracts` finds `client_gen.go`, `api/openapi.json`, `api/go.txt` and the route tables current.
- OR-J39-S16 reads the message log in process and remotely alike.

### 6. Data lifecycle
`internal/retention/tables.go` classes `customer_messages` as rows (deleted 731 days after `created_at`, `retention.CustomerMessages`: dispute evidence), and `notification_issue_revisions` and `notification_channel_state` as state; each table comment states its class. None is partitioned. A customer's erasure deletes their messages, and a merchant purge all of the merchant's; a merchant archive carries no message.

Checks:
- `CleanupExpiredDataWorker` deletes messages past 731 days in bounded batches (`DeleteCustomerMessagesBefore`).
- SCIM `deleteUser` runs `EraseCustomerMessages`; a merchant purge runs `PurgeMerchantCustomerMessages`.
- `merchantarchive` excludes `customer_messages` and the inbox ordering state (`excludedTables`: a restore sends nothing again).

### 7. Work scales with activity
A message is queued by its fact's insert and dispatched on its commit's notify; the backstop reads only pending rows.

Checks:
- `ClaimPendingCustomerMessages` reads `customer_messages_pending_idx` (`WHERE status = 'pending'`) in batches of 100.
- `CountCustomerMessagesSince` is one customer's messages, on `customer_messages_customer_idx`.
- `SendArgs.InsertOpts` deduplicates by args across states, completed included.

### 8. Abuse protection
A customer is sent at most ten messages an hour. A test message goes only to the merchant's `alert_email`: its request has no address, and a merchant sends ten an hour. `alert_email` and `from_email` are one mailbox each, stored as its address. Templates escape every value in HTML, call a fixed set of functions and render in time and size linear in their source.

Checks:
- `claim` counts under `LockCustomerMessages`, so concurrent jobs cannot pass `RateCap` (OR-J39-S05).
- `CreateTestMessageParams` names no recipient; OR-J39-S18 refuses a `to` field (`unknown_field`).
- `TestHTMLEscapesWhatItIsGiven` renders script and `javascript:` values inert.
- `Service.Test` counts `customer_message_test:<merchant>` past `TestCap` (OR-J39-S18).
- `TestTemplatesCannotGrowWithoutBound` and `TestARenderStopsAtItsCap` refuse loops, recursion and amplifying functions and cap a render.

### 9. Destructive-action safeguards
A billing restore sends customers nothing again, and a fact on a test PSP sends nothing.

Checks:
- `billing.queue_customer_message` returns before inserting while `billing_restore_active`: OR-J39-S02 writes a fact inside a restore's transaction and outside one.

## Tests

- `critical/customer_messages_test.go`: OR-J39-S01 (one receipt across a replayed fact and a refused send), S02 (a restore sends nothing), S03 to S05 (`no_contact`, `test_environment`, `rate_limited` with its finding), S06 (erasure), S07 and S08 (a merchant template; a refused document), S09, S13, S14 and S15 (each cancellation cause sends one confirmation; a refund's access end none), S10 (price change), S11 (refund receipts), S12 (`Deps.MessageRenderer`), S16 (the log route), S17 (preview), S18 (test send and its cap), S19 (SMTP refusals through helpers/smtp), S20 (a last attempt cut off mid-send), S21 (a declined checkout), S22 (a refund found late), S23 (a void), S24 (the subscription receipt), S25 (a staff refund completed late). `internal/modules/messages` has unit tests of rendering, defaults, `Check`, template bounds and type checks, and `refusalOf`.
- Inbox: OR-J31-S40 (adoption export ordering). `TestAccountNotificationChannel` and `TestOrderNotificationRecovery` (`critical/account_notifications_test.go`) publish and withdraw against AuthKit's real store; they run with the suite but map to no scenario id.
- Gaps: no test refuses a message with an SMTP 5xx (`refused`), fails the host's renderer to `render_failed`, drops the dispatcher's connection, or ends a provider-mirrored subscription (told as `access_ended`).
