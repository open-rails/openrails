# Merchant binding

A request acts on one merchant: the one its credential is bound to. Nothing
in the request selects it (not the Host, a header, the path, the query or the
body), and no deployment default fills in. A credential bound to
no live merchant reaches no merchant route: `403 merchant_unresolved`. Every
merchant-bound answer names the merchant it acted on in
`OpenRails-Bound-Merchant`.

This is the same embedded and standalone, for one merchant or many.

## The three bindings

Right after `Authenticate`, OpenRails reads the first that applies:

| Binding | The merchant is | Credentials |
|---|---|---|
| Scope | the merchant whose scope the credential's `BoundScope` is (helpers/auth `Bound`): `{Authority: the Authenticator's RootScope authority, ID: the merchant's id}`. A scope of no live merchant is refused; it never falls through to the issuer | an API key of the merchant's group; a merchant application's token; a trusted issuer's token; a customer token minted for the merchant; a console token exchanged for it; a host's own session bound to it |
| Issuer | the merchant whose `MerchantDeclaration.Issuer` is the credential's issuer. An issuer belongs to one merchant of an engine | a single-merchant host's own sign-ins, its customers' and its staff's |
| Object | the merchant of the row an OpenRails secret opens | a checkout URL's secret, a checkout session or attempt, a PSP's webhook signature, a provisioning token, the in-process handle |

The public routes take no credential and name their merchant themselves:
`GET /v1/products?merchant=<merchant id>&keys=…`, and an order's
Solana Pay request by its order. `GET /v1/config` is the deployment's and
names none; a merchant's own part is `GET /v1/me/config`, for a credential
bound to it.

## Getting a bound credential

**A backend.**

- In process, the merchant's handle: `bill.Merchant(id)` or
  `bill.MerchantBySlug(slug)`. Holding the engine is the authority; no
  credential crosses a wire.
- Over HTTP, `openrails.NewRemoteMerchant` with the merchant's API key or a
  token of its application (client credentials). A backend serving several
  merchants passes `openrails.RemoteOptions` one credential per merchant to
  `openrails.NewRemote` and takes each handle from `Merchant(id)`. A handle
  sends `OpenRails-Expected-Merchant` on each call once its merchant is known.
  The server rejects a different binding with `412 merchant_mismatch` before
  running the operation. The handle also verifies `OpenRails-Bound-Merchant`
  and reports a mismatch as `openrails.BoundMerchantError`.

The expectation is a precondition, never a merchant selector or an additional
permission. Invalid or duplicate values return `400 invalid_param`. Every
request authenticates again, so a token callback that changes merchants is
refused on that call. `NewRemoteMerchant` learns its merchant from the first
bound response and then sends the expectation; use `NewRemote` with the known
merchant ID to guard the first write too. Older servers may ignore the optional
header: response checking can detect a mismatch but cannot undo a write there.

**A browser**, on the customer routes (`/v1/me`):

- Embedded, one merchant: the host's own sign-in, bound by the issuer the
  merchant declares. billing-ui sends the user's token as it is.
- Embedded, several merchants: the host binds the session to the store the
  customer signed in at. With AuthKit, a group-bound mint
  (`MintAccessToken` with `Group`); with your own auth, the session's
  `BoundScope` ([examples/own-auth](../examples/own-auth)).
- Standalone or hosted: the merchant's backend mints a customer token,
  `Client.CustomerToken` (an RFC 7523 assertion redeemed at the server's token
  endpoint, valid 15 minutes), or signs the assertion alone with
  `Client.CustomerAssertion` for the browser to redeem with its own DPoP key.
  billing-ui's `tokenSource` keeps the token in memory and fetches the next
  before it expires.

**The admin console.** On a single-merchant embedded mount, the host's
sign-in, bound by its issuer; there is no switcher. On the standalone server,
choosing a merchant exchanges the person's token for one bound to it (RFC
8693 with `group_id`, through the server's `openrails-console` client), held
in memory for the tab. The exchange is refused where the person holds no
role. See [admin console](admin-console.md).

## What it may do there

The binding picks the merchant; permissions pick the routes. A staff or
programmatic route admits its `merchant:` permission held in the merchant's
scope, or its `root:` twin held in the root scope ([auth](auth.md#permissions)).
A person's credential bound to a merchant keeps their root roles; an
application's holds nothing outside its merchant.

## `merchant_unresolved`

`403 merchant_unresolved` means the credential is valid but bound to no live
merchant here:

- a person's own sign-in on a deployment where no merchant declares its
  issuer: switch merchant (the console's exchange, or your session's binding);
- a scope that is no merchant's, or a deleted or archived merchant's;
- an in-process handle of an unknown merchant (`billing.ErrMerchantUnresolved`).

No header, parameter or Host changes the answer: fix the credential.
