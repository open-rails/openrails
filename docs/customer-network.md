# Hosted customer accounts

Platform accounts, cross-merchant links and their lifecycle belong to
OpenRails SaaS. The self-hostable/embedded OpenRails library owns merchant
billing customers, orders, subscriptions, payment authority and generic customer
APIs. Declaring an AuthKit agreement does not enable a platform service.

Hosts can use `Engine.ResolveCustomer` for read-only proof of an existing
merchant customer and `Engine.CustomerRoutes` to reuse customer operations
through their current owned account mapping. The original verified credential
and sensitive-action checks remain intact. A standalone host may supply
`server.Deps.CustomerAdmissionPolicy` to refuse ordinary automatic customer
admission before any customer row is created. This hook does not change staff,
AuthKit, mapped-customer or pay-URL routes.

The removed network routes and SDK methods have no compatibility aliases.
Existing hosted deployments must follow the explicit stopped-writer
[ownership and migration plan](customer-network-removal-plan.md) before the
forward removal migration. Populated legacy links or reader cursors are refused
until transferred or exported; released migration bytes and historical retained
facts are preserved. Source replacement does not itself qualify a production
cutover or shared-card-vault behavior.
