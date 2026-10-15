# AuthKit merchant binding

This optional module connects an AuthKit account tenant to an existing
OpenRails merchant. The root OpenRails module continues to depend only on
authentication contracts, so applications using a different identity system
do not link AuthKit's runtime.

`BindMerchant(ctx, pool, database, client, binding)` is an explicit adoption
operation. `MerchantGroups(client)` supplies the read-only metadata dependency
for the same binding at each subsequent `openrails.New`. Both use the native
AuthKit tenant view and group APIs. Neither grants an owner role, creates an
API key, changes a merchant name nor calls a payment provider.

The pool must be the database the AuthKit client uses. Apply both libraries'
migrations and create the tenant first. Declare the `merchant` persona and
the published OpenRails merchant permission catalog in AuthKit. The binding
names the existing merchant UUID and the canonical tenant UUID; its AuthKit
group is keyed by the merchant UUID. No role assignment is inferred from a
deployment's default tenant.

See [the embedded binding guide](../../docs/embedding-tenant-binding.md) for
configuration and restore behavior. Integration tests use only a disposable
database supplied as `OPENRAILS_E2E_DSN`.
