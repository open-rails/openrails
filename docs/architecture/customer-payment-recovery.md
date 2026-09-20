# Customer payment recovery

A customer can pay an unpaid invoice or retry a past-due subscription from the
existing self-service billing surface. These commands accept no payer identifier:
the verified customer principal owns the addressed resource. `Idempotency-Key`
is required, payer-scoped and bound to the accepted request. Same-key replay
returns the existing result after settlement; a different key cannot displace
unresolved work. The existing invoice/subscription read exposes `recovery` and
any unresolved operation, without a second operation API.

The shared Go Client runs identically over HTTP or in process. Embedded hosts
configure `embed.Options.DelegatedAuthenticator` once (the ordinary Handler
inherits it) and construct a customer Client with explicit credentials:

```go
customer, err := runtime.Client(openrails.WithTokenProvider(func(ctx context.Context) (string, error) {
    return verifiedCustomerToken, nil
}))
result, err := customer.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{
    InvoiceID: invoiceID,
    PaymentMethodID: paymentMethodID,
    IdempotencyKey: stableKeyForThisCustomerAction,
})
```

The authenticator must verify a customer's credential before mapping it to the
payer and set `CredentialClassUserSession`. The AuthKit bridge derives this
from verified claims; device-key credentials are automation authority and do
not establish customer interaction. Custom trusted bridges explicitly attest
this class. It is never accepted from a request header/body. Unknown or
automation classes retain their existing self reads but cannot initiate CIT. The `embed/authkit` bridge performs the existing host verification and
supports a live admission veto. A merchant API key or service credential cannot
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
    class := billingauth.CredentialClassUserSession
    if claims.DeviceKeyID != "" || claims.TokenType != "" {
        class = billingauth.CredentialClassAutomation
    }
    return authkit.DelegationGrant{Attributes: map[string]any{
        billingauth.DelegatedCredentialClassAttribute: class,
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
unresolved before a money write. Live NMI scheduling effects remain a separate
provider qualification requirement.

Current customer-pay support is NMI with PSP-held cards. Unsupported rails or
custody paths return `customer_payment_unsupported`. Stripe's customer-action/
3DS continuation remains an explicit #809 follow-up; its existing administrative
and scheduled off-session collection is unchanged. Invoice payment does not
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
