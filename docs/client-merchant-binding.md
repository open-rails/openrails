# Client merchant binding

A client has one merchant binding, fixed at construction; there is no per-call
merchant selection. `openrails.WithMerchantID(id)` binds a remote client to an
immutable merchant UUID. `openrails.New` binds its Client to `Config.Merchant`;
an engine without one serves clients derived with
`client.With(openrails.WithMerchantID(id))`, and an engine with one refuses a
client that names a different merchant.

Both transports carry the expected UUID as `OpenRails-Merchant: id:<uuid>`.
Every merchant route resolves it, authenticates normally, then compares the
credential's merchant with it before acquiring a merchant database connection
or executing business logic. The header supplies no authority and does not
select a merchant the credential is not bound to. A malformed selector returns
`400 merchant_selector_invalid` and a mismatch `409 merchant_binding_mismatch`.
See [client-merchant-selection.md](client-merchant-selection.md) for the slug
form and per-call selection.

For a SaaS host, construct one client per merchant: over HTTP with that
merchant's credential and UUID, or in process with `client.With(openrails.WithMerchantID(id))`. Global consumer identities and wallet permissions remain
SaaS responsibilities.
