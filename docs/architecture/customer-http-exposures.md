# Customer routes

Configure the engine once, then mount the routes an `openrails.Routes` selects
with a framework adapter. There is one customer surface, `/v1/me`, always
mounted. A standalone server's `Routes` is the standalone surface: identity,
merchant, platform, customer, callback and enabled console registrations.
Process health endpoints remain host-owned. Provider callbacks are addressed by
account (`/v1/webhooks/{rail}/{account_id}`) in every posture.

The customer is the subject of the identity `Routes.Auth` admits: a user acting
itself. An invoker acting for someone else, or an application, is refused. The
surface serves `Config.Merchant` embedded; on a standalone server it serves the
merchant each request selects: the `OpenRails-Merchant` header or the
merchant's API host. A request that selects none is refused
`merchant_unresolved`, and an unknown or deleted merchant `merchant_not_found`.
OpenRails binds the merchant before the `Auth` runs, and the `Auth` reads it
with `openrails.RequestMerchant` rather than resolving the header itself. The
customer is the identity's subject at that merchant, so selecting another
merchant never reaches another subject's billing.

The `Auth` verifies the actual credential; a URL parameter or request-body
field is never authority. Ambient cookies never reach it: a browser call carries
its credential in a header. Original URL, RawPath, RequestURI and body remain
available for signature and sender-proof checks. Mounting copies the selection;
conflicting method/path patterns fail before router mutation.

Every adapter mounts on the application's root router: Gin's root Engine,
Fiber's root App, a root ServeMux or Chi Mux. `Routes.Prefix` places the
embedded API (`/billing` serves `/billing/v1/*`) and the admin console beside it
(`/billing/admin`); a standalone server's routes, with their issuer-anchored
AuthKit and console URLs, sit at the root.

A checkout session's id alone pays it with a new card; a saved card needs its
customer's proof. The session's customer reads and pays it at
`/v1/me/checkout-sessions/{id}` and `/v1/me/checkout-sessions/{id}/pay` (beside
`/v1/me/checkout-sessions`, which mints one), and the `Auth` is that proof. The
session must be the request's merchant's and the identity's subject its
customer; any other session is `checkout_session_not_found`, and only the
customer acting in person pays (`customer_action_required`). billing-ui's
`client.checkoutSource(id, { customerBase: prefix })` reads and pays there;
without `customerBase` it uses the session id alone at
`/v1/checkout-sessions/{id}`.
