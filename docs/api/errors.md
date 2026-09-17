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
| `payment_provider_rejected` | `api_error` | 502 | the provider rejected the charge for a gateway or merchant-configuration reason; another card will not help | `decline_reason`, `failure_code` |

Classification uses only provider facts (response and localization codes), never
provider text. The same codes are returned by payment-method creation and tier
changes. `insufficient_credits` (402) remains the payer-balance denial.

Transport failures preserve their cause: `errors.Is(err, context.Canceled)` and
`errors.Is(err, context.DeadlineExceeded)` work in both modes. A transport failure
is not proof that an operation was rejected or did not commit. Financial retries
must retain the same operation identity and terms. `ErrUnreachable` also matches
server failures; use `errors.As` to distinguish an actual server response from a
lost response. The client does not automatically retry financial operations.

Construction validates static configuration without making a request.
`client.Verify(ctx)` checks live authentication and reachability.
