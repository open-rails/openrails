# Error contract

Standalone, SaaS-hosted and embedded clients use the same HTTP handlers and error
payload. An unsuccessful request returns its HTTP status and this envelope:

```json
{"error":{"type":"invalid_request_error","code":"idempotency_key_reused","message":"The operation has different monetary terms","request_id":"request-correlation-id","param":"amount","metadata":{"original_amount":"1000000"}}}
```

`type` is the category, `code` is the machine-readable reason, and `message` is a
human diagnostic that may change. `request_id`, `param` and `metadata` are optional.
Batch admission places the same error object inside each unsuccessful item's
`error` field; an ordinary admission denial instead returns its complete decision.

The Go client returns `*openrails.StatusError` with the full `ErrorDetails`, HTTP
`Status`, and `RetryAfter` response header. `errors.Is` classifies by status and
machine code, never by message text. In particular, `idempotency_key_reused` means
changed operation terms, and `insufficient_credits` means insufficient credit;
other HTTP 402 responses do not imply a low credit balance. Metadata numbers
are decoded as `json.Number` so their integer precision is preserved. The body
request ID takes precedence, with `X-Request-ID` as a fallback.

## Payment refusals

A checkout the provider refused returns HTTP 402 and `errors.Is(err,
openrails.ErrPaymentRefused)`; nothing was charged and the customer may try again
with another instrument in a new checkout session. The code says what happened,
`metadata` says why:

| Code | Type | Status | Meaning | Metadata |
| --- | --- | --- | --- | --- |
| `card_declined` | `card_error` | 402 | the provider declined the presented card | `decline_reason` (normalized: `insufficient_funds`, `expired_card`, `cvv_avs`, `card_declined`, `fraud_suspected`, ...), `failure_code` (provider's verbatim code) |
| `payment_method_stale` | `card_error` | 402 | the saved payment method named by the request can no longer be charged for this customer and processor; collect the card again | — |
| `payment_method_required` | `invalid_request_error` | 400 | the charge named neither a saved `payment_method_id` nor a new card `payment_token`; OpenRails never charges an implied card such as the default | `param`: `payment_method_id` |
| `payment_provider_rejected` | `api_error` | 502 | the provider rejected the charge for a gateway or merchant-configuration reason; another card will not help | `decline_reason`, `failure_code` |

Classification uses only provider facts (response and localization codes), never
provider text. The same codes are returned by payment-method creation and tier
changes. `insufficient_credits` (402) remains the payer-balance denial.
Refusals the transport answers before a handler runs use the same envelope:
a request body over the deployment's cap (1 MiB in every deployment, the
in-process embedded Client included) is `413` with code `request_body_too_large`
(`openrails.ErrRequestBodyTooLarge`); an unreadable body is `400
invalid_request_body`; an unauthenticated request on a host-authenticated
route is `401 unauthorized`.

Reassigning a subscription to a saved method vaulted by a different provider
account (`PUT .../subscriptions/{id}/payment-method`) is `409
payment_method_psp_mismatch` (`openrails.ErrPaymentMethodPSPMismatch`, also
`ErrConflict`): provider vault references are account-scoped, nothing was sent to
the provider, and the card must be collected again on the subscription's active
account. The same code answers whether the mismatch is seen at the HTTP
pre-check or by the durable intent, which re-verifies it under the method's row
lock before any provider call.

Repointing an NMI-billed subscription to another card in the same NMI vault is
`409 payment_method_same_vault`: NMI schedules bill a vault, not one of its
cards, so the change would do nothing at NMI. Vault the new card separately.

`409 payment_duplicate_refused`: the provider refused the charge or card
verification unprocessed as a duplicate of an identical charge (same card and
amount) it had just made. Nothing was charged or saved; send the request again
in a few minutes.

NMI-billed (legacy) subscriptions: a cancel while the merchant's destructive
switch is off is `409 provider_cancel_held` (nothing changed; NMI keeps billing
until an operator arms the switch); a tier change to a price of another billing
cycle is `409 tier_change_cadence_unsupported` (NMI's billing date is kept); a
change of a schedule on a named NMI plan to a price without a linked NMI plan of
the same amount and cycle is `409 tier_change_requires_linked_plan` (NMI changes
named-plan schedules only by switching plans; nothing was charged).
Engine takeovers answer
`engine_takeover_ineligible`, `engine_takeover_no_recurring_agreement`,
`engine_takeover_boundary_too_close`, `engine_takeover_in_flight`,
`engine_takeover_committed`, `engine_takeover_conflict` (409) and
`engine_takeover_not_found` (404).

A method moved into third-party custody is refused with `409
payment_method_not_psp_vaulted` on the same route, even when its PSP id still
matches. Its retained PSP vault reference is historical correlation, not a
usable address for changing provider-managed recurring billing. The producer,
executor and verifier enforce this before provider traffic.

Transport failures preserve their cause: `errors.Is(err, context.Canceled)` and
`errors.Is(err, context.DeadlineExceeded)` work in both modes. A transport failure
is not proof that an operation was rejected or did not commit. Financial retries
must retain the same operation identity and terms. `ErrUnreachable` also matches
server failures; use `errors.As` to distinguish an actual server response from a
lost response. The client does not automatically retry financial operations.

Construction validates static configuration without making a request.
`client.Verify(ctx)` checks live authentication and reachability.
