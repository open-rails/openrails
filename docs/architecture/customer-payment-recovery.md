# Customer payment recovery

A customer can pay an unpaid invoice or retry a past-due subscription from the
existing self-service billing surface. These commands accept no customer identifier:
the verified customer principal owns the addressed resource. `Idempotency-Key`
is required, customer-scoped and bound to the accepted request. Same-key replay
returns the existing result after settlement; a different key cannot displace
unresolved work. The existing invoice/subscription read exposes `recovery` and
any unresolved operation, without a second operation API.

These are customer HTTP routes; the Go Client carries merchant routes only.
Embedded hosts verify customer credentials with their AuthKit client or
`Deps.Authenticate`.

The authenticator must verify a customer's credential before mapping it to the
customer and set `CredentialClass: openrails.CredentialUserSession` on the
`openrails.Identity` it returns. The host explicitly maps a verified
credential in `Deps.Authenticate`; device-key credentials are
automation authority and do not establish customer interaction. It is never accepted from a request header/body. Unknown or
automation classes retain their existing self reads but cannot initiate CIT. The host verifier owns verification and any explicit live admission policy. A merchant API key or service credential cannot
become customer-present by supplying a payment method. Ambient host request
context remains isolated. The default runtime Client is the merchant-owner
client and is not a customer credential. An explicit per-mount verifier override
is possible, but is a deliberate host policy decision.


The built-in delegated-token receiver reads the reserved signed attribute
`attributes.openrails_credential_class`. Missing means unknown; only
`user_session` and `automation` are valid explicit values. A delegated subject
alone is not evidence of customer interaction. For AuthKit's HTTP mint route,
the host authorizer can read the original verified claims from its context:

```go
DelegatedAuthorization: func(ctx context.Context, request authkit.DelegationRequest) (authkit.DelegationGrant, error) {
    claims, ok := verify.ClaimsFromContext(ctx)
    if !ok || claims.UserID == "" || claims.UserID != request.UserID {
        return authkit.DelegationGrant{}, authkit.ErrDelegationRefused
    }
    class := openrails.CredentialUserSession
    if claims.DeviceKeyID != "" || claims.TokenType != "" {
        class = openrails.CredentialAutomation
    }
    return authkit.DelegationGrant{Attributes: map[string]any{
        "openrails_credential_class": class,
    }}, nil
}
```

That callback constructs its grant from verified context; it never copies the
requested grant's class. A programmatic issuer without original credential
provenance leaves the attribute absent or marks automation. The device-key
login → HTTP delegation → OpenRails workflow qualifies this distinction with
real signing keys and sender proofs. Altering the signed class invalidates the
token. Host bridges supplying `DelegatedPrincipal` directly obey the same rule.


For NMI invoice pay, a customer action freezes initial or subsequent unscheduled
stored-credential posture. An initial approved reference is captured only from
the qualified provider receipt, atomically with invoice settlement and operation
completion. Local rollback retains custody and recovery commits it without
charging again. Future off-session invoice collection requires that approved
reference. Rounded provider collection settles the exact invoice liability and
credits the excess through the existing spendable credit ledger.

Subscription retry uses the same immutable admission and completion as the
scheduled worker. It freezes customer-initiated recurring reuse of the existing
approved agreement, the subscription's current method, price/benefits and paid
period. The NMI request remains `recurring=rebill_subscription`; preparation
checks the supported fixed-day calendar against the actual provider record.
The client command does not invent new calendar semantics or reset a lapsed
period to the click time. Unsupported cadence or preparation stays refused or
unresolved before a money write.

Current customer-pay support is NMI with PSP-held cards. Unsupported rails or
custody paths return `customer_payment_unsupported`; Stripe's administrative
and scheduled off-session collection is unaffected. Invoice payment does not
reactivate an unrelated subscription.

HTTP commands:

- `POST /v1/me/invoices/{id}/pay-now` with `payment_method_id`.
- `POST /v1/me/subscriptions/{id}/retry-now` with optional `payment_method_id`
  (when supplied, it must be the subscription's current method).

A completed result is 200; unresolved execution is 202 with the operation's
identity and state. A definitive card refusal uses the existing coded 402 error
envelope (processor failures retain the standard processor error mapping).
Error metadata includes the operation ID. Replaying the same key returns that
original refusal even if a later attempt has recovered the account.
`payment_in_progress` and `payment_idempotency_conflict` are 409 refusals.
A caller should retain its key through network uncertainty and read the existing
resource before starting a different action.

Fresh customer invoice/subscription reads include `recovery.last_failure_reason`
when the latest applicable collection failed. This is the same normalized machine
category as a payment error's `decline_reason` metadata, never provider response
text. The existing invoice collection failure count/next attempt and subscription
retry count/next retry fields accompany it. A successful recovery, a newer pending
attempt, or a different subscription period does not inherit an old refusal.
