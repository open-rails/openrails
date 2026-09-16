# Client merchant binding

Use `openrails.WithMerchantID(id)` when constructing a remote client to bind it
once to an immutable merchant UUID. `Runtime.Client()` inherits its configured
merchant at construction. An optional `openrails.WithMerchant(ctx, id)` assertion
must agree with that binding; conflicting calls return a structured 409 conflict.

Both transports carry the expected UUID in `X-OpenRails-Merchant-ID`. Every
merchant route authenticates normally, then compares the resolved merchant with
the asserted UUID before acquiring a merchant database connection or executing
business logic. The header supplies no authority and does not select a different
merchant. A malformed UUID returns 400 and a mismatch returns 409. Calls without
an assertion still use the authenticated credential's merchant.

For a SaaS host, construct one client per merchant using that merchant's
credential and UUID. Global consumer identities and wallet permissions remain
SaaS responsibilities.
