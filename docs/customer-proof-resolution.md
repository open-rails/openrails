# Existing-customer credential resolution

`Engine.ResolveCustomer` proves the merchant side of a potential hosted account
relationship. `Engine.CustomerRoutes` instead consumes an already authorized
relationship. Both preserve the original request's verified authority.

API: `Engine.ResolveCustomer(ctx, authority, verified) (CustomerProof, error)`.
The host supplies the same trusted authenticator metadata and original Verified
request. The resolver must not call Authenticate again, consume another sender
proof, mint a proxy credential or trust merchant/customer selectors from a body.

The resolver reuses the core merchant-bound/declared-issuer, self-scope, source-tenant
and stored-customer-issuer checks. It requires a live merchant and an existing matching
customer; resolving never creates or admits one, changes its issuer, creates a
link, or enters the legacy native network service. A platform subject matching a
customer UUID without merchant-specific credential authority grants nothing.

Native user credentials must resolve through an explicit merchant-bound self
context or its declared issuer. Merchant-issued assertions and foreign resource
credentials retain their verified merchant binding and issuer scope. Console
tokens without customer self scope, delegated identities, applications, API keys
and personal automation credentials are not customer-link proof.

A retained credential must implement AuthKit's documented optional
`auth.CredentialChecker` capability. The resolver checks the original credential
before reading billing state and after the final storage query. Unsupported
credentials fail closed. Neither a permission check nor recent sign-in replaces
this liveness check; a merchant backend's assertion has no recent human sign-in.

The authenticator and Verified must come from the same trusted provider. Pass the
original Verified, including its scope, assertion and liveness capabilities;
reconstructing an identity from claims or stripping its capabilities is not a
supported integration. The returned `CustomerProof` carries the merchant/customer pair and a copy of
the validated row's `Issuer`. Nil preserves exactly SQL NULL (the host/merchant
native namespace); a non-nil value preserves the stored external issuer, including
an empty stored value. Nil never proves platform account ownership. This is
request-scoped evidence, not a saved permission or a durable account link. A host remains responsible for its separate
platform proof, consent and transactional relationship creation.

Missing customers return `billing.ErrNotFound`; an issuer mismatch or unresolved
merchant has its existing billing error code. Credential expiry, revocation,
unsupported credentials and authority outages retain AuthKit's error classes.
Resolution never starts a new HTTP authentication or consumes another DPoP proof.

The critical scenarios use real AuthKit-issued merchant credentials, including
sender-constrained proof. Observe correct binding and denial for foreign issuer,
merchant, customer and tenant; deleted/absent customer and merchant; console and
automation credentials; expiry and revocation. Check no customer, issuer or link
side effects and exactly one verification of each original request. SaaS still
owns its independent fresh platform nonce/ID-token proof and one-use challenge.
Payment URLs remain a separate explicitly limited proof class and are not
accepted by this resolver. The API does not expose a new HTTP route.
