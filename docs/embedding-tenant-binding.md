# Bind an existing merchant to an account tenant

An embedded host can associate an existing billing identity with one business
tenant. The billing ID remains independent of the tenant ID. The merchant's
permission group uses the billing ID and belongs to that tenant; AuthKit then
evaluates tenant and merchant roles through its native inheritance rules.

The optional `github.com/open-rails/openrails/adapters/authkit` module performs
the one-time binding. It uses an existing AuthKit client and the same PostgreSQL
database as OpenRails. Apply both libraries' migrations first and explicitly
create the intended tenant. Declare AuthKit's `merchant` persona and the
published merchant permissions before binding.

```go
import (
    "github.com/open-rails/openrails"
    "github.com/open-rails/openrails/billing"
    orauth "github.com/open-rails/openrails/adapters/authkit"
)

binding := billing.MerchantBinding{
    ID: existingMerchantID,
    TenantID: tenantID.String(),
}
err := orauth.BindMerchant(ctx, pool, database, authClient, binding)
if err != nil {
    return err
}

cfg.Merchants = map[string]openrails.MerchantDeclaration{
    "host-one": {Binding: &binding},
}
engine, err := openrails.New(ctx, cfg, openrails.Deps{
    Postgres: pool,
    MerchantGroups: orauth.MerchantGroups(authClient),
})
```

Binding locks the existing live merchant, creates or verifies its AuthKit group
with no owner assignment, and records both identities in one transaction. It
does not create a merchant, resolve a name, grant roles, issue keys, start
workers or call payment providers. Repeating the same binding is safe. Another
tenant, group, retired merchant or second merchant for the same tenant is
refused. The association is immutable, including after retirement.

`MerchantDeclaration.Binding` is a trusted Go configuration value. Merchant
configuration documents cannot supply it. At startup the engine verifies the
exact ID, current merchant name, tenant and live group metadata through
`Deps.MerchantGroups`. It does not adopt an unbound row automatically. A host
may implement that small metadata interface with a different identity system;
request authentication remains the separate `Routes.Auth` contract.

For an adopted merchant, customer routes also require the verified user's
source `Identity.TenantID` to match the merchant's tenant. Missing or different
ownership returns `customer_tenant_mismatch` before a customer is admitted.
Global staff use the explicit admin routes; their authority does not create a
personal customer account in another tenant. Hosts must supply signed,
live-validated tenant metadata through their authentication adapter.

Without `Binding`, a declaration keeps the unbound billing mode. It cannot
claim a merchant that already has a permission group. A group-bound deployment
must declare its expected binding rather than relying on a matching name.

Billing archives include the tenant association in their integrity-protected
header. Provision and bind the destination to the same merchant and tenant
before importing; import verifies ownership and never changes it. Unbound
archives keep their prior format. A legacy unbound archive cannot be silently
imported into an adopted tenant; restore it before the separately reviewed
adoption. Source and destination workers remain stopped during the final
archive cutover.
