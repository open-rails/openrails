# Customer messages

OpenRails emails customers their billing messages: receipts, payment problems,
cancellations and price changes. A message is a [notification event](events.md)
with a template. It is queued in the transaction that records the fact, so a
rolled-back fact sends nothing and a replayed one sends once. The log of what
was sent (`customer_messages`) keeps no body.

## Sending

- The sender is `Deps.Email` (the server's `email_smtp`); without one a message
  ends `failed` (`no_sender`). It is from the profile's `from_email` and display
  name, else the sender's own address.
- The recipient is the customer's address in the directory
  ([customer contacts](customer-contacts.md)); none ends `no_contact`.
- A fact on a test or sandbox PSP sends nothing (`test_environment`): its
  `psp_id`, or its payment's, order's or subscription's PSP.
- A customer is sent at most ten messages an hour; more are `rate_limited` and
  open a `life.customer.messages_rate_limited` finding.
- A failed send is retried with backoff, ten attempts in all. Only a mail
  server's 5xx to the recipient (`RCPT TO`) is final (`refused`). A 5xx to the
  sender (`AUTH` or `MAIL FROM`) is retried and opens a
  `life.customer.messages_sender_refused` finding naming the from address.

## Event types and defaults

Every event type below has a built-in English message.

| On by default | |
|---|---|
| `one_off_purchase_completed` | Receipt for a one-off purchase |
| `refund_completed` | Refund receipt |
| `premium_started` | The subscription started |
| `payment_method_failed` | A payment failed, with the update-card link; not the failure that ended a subscription (`recovery_action: resubscribe`), whose ending confirms it, nor a declined checkout (`source: customer_checkout` or `customer_subscription_change`), which the page reports |
| `payment_authentication_required` | The bank wants the customer to confirm a payment |
| `payment_method_update_required` | The card on file no longer works |
| `subscription_canceled` | Cancellation confirmation while paid access remains, worded by `Reason`: `user_cancel` (the customer, with a resume link), `admin` (staff), `expired` or `non_recoverable` (failed payments) or anything else |
| `premium_ended` | Cancellation confirmation once access has ended, for the causes below |
| `order_collection_stopped` | Collection stopped after failed payments: the amount is still owed |
| `subscription_reprice_scheduled` | Price-change notice: old and new amounts, the date and how to cancel |

Off by default: `premium_renewed`, `payment_method_auto_updated`,
`account_delinquent`, `account_delinquency_cleared`, `usage_charged`,
`balance_low`, `balance_depleted`, `order_due`, `order_overdue`,
`subscription_plan_change_scheduled`, `subscription_changed`, `agreement_revoked`.

### Cancellation confirmations

A cancellation the customer, failed payments or staff cause is confirmed once.
While paid access remains the fact is `subscription_canceled`; when access
ends with it, `premium_ended`. `premium_ended` is on by default only for these
`Reason`s, each in its own words:

| `Reason` | Cause |
|---|---|
| `user_cancel` | The customer canceled |
| `admin` | Staff canceled, through OpenRails (`CancelSubscription`, with or without `revoke_access`) |
| `expired` | Payments failed: dunning spent its retries, the wait for a new card ran out, or the provider stopped after failed rebills |
| `non_recoverable` | The card can no longer be charged |

Any other ending sends nothing by default: `refund` (a refund that revoked
access), `chargeback`, `rail_cancel` (the provider, a void or a tier change),
`access_ended` (access reached its end, no cause recorded) and `unknown`. A
merchant's `enabled` for `premium_ended` replaces the rule for every reason.
An ending OpenRails mirrors from a provider's records through reconciliation
is told when access closes, as `access_ended`.

## The messages document

A merchant's `messages` document turns event types on or off and replaces any
message. It sits beside the merchant document: `merchants.<slug>.messages` in a
manifest, `MerchantDocument.Messages` embedded, or
`<scope_prefix>/merchants/<merchant-id>/messages` in [Vault](vault.md).

```yaml
messages:
  default_language: en
  events:
    premium_renewed:
      enabled: true
    one_off_purchase_completed:
      templates:
        en:
          subject: "Your {{.Merchant.Name}} receipt"
          text: |
            You paid {{.Event.Amount}} for {{.Event.ProductName}}.
          html: |
            <p>You paid <strong>{{.Event.Amount}}</strong> for {{.Event.ProductName}}.</p>
  layouts:
    en:
      text: |
        {{template "body" .}}
        -- {{.Merchant.Name}}
```

- An event type left out keeps its default; `enabled` alone keeps the built-in
  message.
- A template needs a `subject` and a body; a body it leaves out is not sent.
- A layout wraps every message's body where it runs `{{template "body" .}}`; a
  part left out is the built-in one. The built-in layouts greet the customer
  and close with the merchant's name, support, terms and privacy links.
- Messages are written in `default_language` (a BCP 47 tag, `en` when unset).
  Each part is the merchant's in the first of that language and `en` that has
  it, else the built-in English one. Customers carry no language yet.

## Data (version 1)

Templates render `billing.MessageData`, whose Go doc lists every field. Each is
text already formatted for the customer, empty when the fact or profile has
none: test it with `{{with}}` or `{{if}}`. Within version 1 fields are only added.

| Field | |
|---|---|
| `.Version`, `.EventType`, `.Language` | `1`, the event type, the message's language |
| `.Customer.ID`, `.Customer.Name` | The customer's id; their name, else username |
| `.Merchant.Name`, `.LogoURL`, `.SupportURL`, `.BillingURL`, `.SubscribeURL`, `.TermsURL`, `.PrivacyURL`, `.StatementDescriptor` | The merchant's profile |
| `.Event.*` | The fact's fields (`billing.MessageEvent`): amounts with their currency at its registered scale (`12.50 USD`), dates in UTC (`January 2, 2006`), `Reason` as the event has it, `ProductName` (what an order bought, else the subscription's product) |
| `.Links.ManageBilling`, `.UpdatePaymentMethod`, `.Pay` | The profile's `billing_url` |
| `.Links.Resubscribe` | `subscribe_url`, else `billing_url` |

Each link falls back to `support_url`.

## Functions

Subjects and text bodies are Go `text/template`, HTML bodies `html/template`,
which escapes every value for where it is used. A template may call `and`,
`or`, `not`, `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `len`, `index`, `slice` and
three more: `default FALLBACK VALUE` (`{{default "there" .Customer.Name}}`),
`upper` and `lower`. Options are `missingkey=error`. A text body's runs of
blank lines collapse to one; a subject's whitespace to single spaces.

A message renders in time and size linear in its template:

- no `range`, `define`, `block` or variable (`with` and `$` reach the data);
- no `template` but the layout's one `{{template "body" .}}`;
- no `print`, `printf`, `println`, `html`, `js`, `urlquery` or `call`;
- each part's source is at most 64 KiB, and a part rendering past 256 KiB fails.

## Validation

A messages document is checked as it loads (a file at startup, Vault on each
change): its languages are tags, each event type is one listed above, every
field a template names, in any branch, is one of `billing.MessageData`, and
every message renders against its event type's sample data for a customer of
each language the document names. One that fails is refused alone, naming the
event type or layout and the error (`events.one_off_purchase_completed: text: …
can't evaluate field Total`). The version before it keeps serving (none: the
defaults), and a `consistency.merchant_config.not_served` finding stays open
until it serves.

## The log and checking a template

| | |
|---|---|
| `Client.ListCustomerMessages`, `GET /v1/admin/customers/{customer_id}/messages` | The customer's messages (`billing.CustomerMessage`), newest first: event, status, attempts, last error, recipient, sent time. No body. `merchant:customer:read` |
| `Client.PreviewMessage`, `POST /v1/merchant/messages/preview` | Renders an event type from the merchant's templates over a sample customer and fact, with the merchant's profile, in `language` or the default language. `template` replaces that language's template for this render only; one that does not render is `400 message_template_invalid`, naming the error. `merchant:config:read` |
| `Client.CreateTestMessage`, `POST /v1/merchant/messages/test` | Sends that rendering to the merchant's `alert_email`, the only address a test goes to (none: `409 alert_email_required`), at most ten an hour per merchant (`429 rate_limit_exceeded`, with `Retry-After`). It is not logged. `merchant:config:manage` |

A status is `pending`, `queued` or `sending` on the way, then `sent`,
`disabled`, `test_environment`, `no_contact`, `rate_limited` or `failed`
(`last_error`: `send_failed`, `refused`, `render_failed`, `invalid_recipient`,
`no_sender`, `event_missing`, `event_unreadable`, `unavailable`). Previews and
tests render the templates; a host's `MessageRenderer` is not asked.

## Rendering in your code

An embedded host may render any message itself:

```go
deps.MessageRenderer = func(ctx context.Context, m billing.MessageData) (billing.MessageContent, bool, error) {
    if m.EventType != "one_off_purchase_completed" {
        return billing.MessageContent{}, false, nil // the merchant's templates
    }
    return billing.MessageContent{Subject: "Thanks!", Text: "You paid " + m.Event.Amount + "."}, true, nil
}
```

Content it returns true for is sent as is, before the merchant's templates. An
error is retried like a send; on the last attempt the message is `failed`
(`render_failed`).
