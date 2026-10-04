# Merchant settings

A merchant's settings (`billing.MerchantSettings`) are part of its
configuration: `GET /v1/merchant/configuration` (`Client.GetMerchantConfiguration`)
reads them with the configuration's revision, and
`POST /v1/merchant/configuration/applications` (`Client.ApplyMerchantConfiguration`)
changes them against that revision
([configuration applications](../merchant-configuration-applications.md)). A
mode-1 merchant declares the same document under `settings:` in its YAML.

The settings hold the profile, invoice/arrears policy, checkout routing, the
dunning policy, named billing policies, default/tier bindings and delegated
wasted-spend limits. An application changes only the fields it names; an
explicit empty list removes those declarations. Unknown fields are rejected.

All policy references must name policies in the resulting document. Invalid
fields, missing references or database errors leave every setting unchanged.
Financial policy resolution reads PostgreSQL directly, so another runtime never
keeps an old cached cap.

Per-customer policy bindings are runtime segmentation, outside the settings. Use
`Client.GetCustomerBillingPolicy` and `Client.SetCustomerBillingPolicy`
(GET/PUT `/v1/merchant/customers/{customer_id}/billing-policy`) to read, assign
or clear one customer's explicit policy. Removing a named policy a customer is
still bound to is refused, and the previous settings stay.
