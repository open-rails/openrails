# Client merchant binding

A client has one merchant binding, fixed at construction; there is no per-call
merchant selection. `openrails.WithMerchantID(id)` binds a remote client to an
immutable merchant UUID. `openrails.New` binds its Client to `Config.Merchant`;
an engine without one serves clients derived with
`client.With(openrails.WithMerchantID(id))`, and an engine with one refuses a
client that names a different merchant.

Both transports carry the expected UUID in `X-OpenRails-Merchant-ID`. Every
merchant route authenticates normally, then compares the resolved merchant with
the asserted UUID before acquiring a merchant database connection or executing
business logic. The header supplies no authority and does not select a different
merchant. A malformed UUID returns 400 and a mismatch returns 409. A remote
client built without `WithMerchantID` uses the authenticated credential's
merchant.

For a SaaS host, construct one client per merchant: over HTTP with that
merchant's credential and UUID, or in process with `client.With(openrails.WithMerchantID(id))`. Global consumer identities and wallet permissions remain
SaaS responsibilities.
