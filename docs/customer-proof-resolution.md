# Existing-customer credential resolution

Draft design for the hosted account extraction. No public resolver is implemented
yet. This task builds on the separately qualified host-authorized CustomerRoutes
mount; that mount consumes an existing relationship, while this resolver will
prove the merchant side of creating a relationship.

Proposed API: `Engine.ResolveCustomer(ctx, authority, verified) (CustomerBinding, error)`.
The host supplies the same trusted authenticator metadata and original Verified
request. The resolver must not call Authenticate again, consume another sender
proof, mint a proxy credential or trust merchant/customer selectors from a body.

Reuse the existing core merchant-bound/declared-issuer, self-scope, source-tenant
and stored-customer-issuer checks. Require a live merchant and an existing matching
customer; resolving never creates or admits one, changes its issuer, creates a
link, or enters the legacy native network service. A platform subject matching a
customer UUID without merchant-specific credential authority grants nothing.

Native user credentials must resolve through an explicit merchant-bound self
context or its declared issuer. Merchant-issued assertions and foreign resource
credentials retain their verified merchant binding and issuer scope. Console
tokens without customer self scope, delegated identities, applications, API keys
and personal automation credentials are not customer-link proof.

A fresh successful authentication already rejects expired and revoked credentials.
The remaining design question is retained liveness during a database wait:
`auth.Verified` has no public liveness-only capability. Recent sign-in is not an
acceptable substitute for backend customer assertions, and a fake permission
check must not supply proof. If this operation rechecks retained liveness, add a
small documented credential-check capability and implement it through the
authority's retained verification state, never by authenticating the request again.

Qualification will use real AuthKit-issued merchant credentials, including
sender-constrained proof. Observe correct binding and denial for foreign issuer,
merchant, customer and tenant; deleted/absent customer and merchant; console and
automation credentials; expiry and revocation. Check no customer, issuer or link
side effects and exactly one verification of each original request. SaaS still
owns its independent fresh platform nonce/ID-token proof and one-use challenge.
Payment URLs remain a separate explicitly limited proof class.
