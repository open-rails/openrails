# Client merchant binding

A client has one merchant binding, fixed at construction; there is no per-call
merchant selection. `openrails.WithMerchantID(id)` binds a remote client to an
immutable merchant UUID. `Runtime.Client()` inherits the runtime's configured
merchant; a multi-merchant runtime requires `WithMerchantID` and refuses an
unbound client or one that names a different merchant than the runtime.

Both transports carry the expected UUID in `X-OpenRails-Merchant-ID`. Every
merchant route authenticates normally, then compares the resolved merchant with
the asserted UUID before acquiring a merchant database connection or executing
business logic. The header supplies no authority and does not select a different
merchant. A malformed UUID returns 400 and a mismatch returns 409. A remote
client built without `WithMerchantID` uses the authenticated credential's
merchant.

For a SaaS host, construct one client per merchant: over HTTP with that
merchant's credential and UUID, or in process with `WithMerchantID`. Global consumer identities and wallet permissions remain
SaaS responsibilities.
