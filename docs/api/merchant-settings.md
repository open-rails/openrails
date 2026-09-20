# Merchant settings

`GET /v1/merchant/settings` reads the complete declarative merchant document.
`PUT /v1/merchant/settings` replaces that document atomically. The Go methods are
`Client.GetMerchantSettings` and `Client.SetMerchantSettings` in every deployment.

GET includes profile, invoice/arrears policy, checkout routing,
named billing policies, default/tier bindings and delegated
wasted-spend limits. Sending an unchanged GET result preserves the declaration.
Omitted fields in PUT reset to their defaults; empty lists remove declarations.
Unknown fields and null/non-object documents are rejected. The server returns complete
configuration lists; runtime customer records are not part of this document.

All policy references must name policies in the same document. Invalid fields,
missing references or database errors leave every setting unchanged. Readers
observe a complete document before or after a concurrent replacement. Financial
policy resolution reads PostgreSQL directly so another runtime does not retain
an old process-local cached cap.

Per-customer policy bindings are runtime segmentation, outside
this declaration. Use `Client.GetCustomerBillingPolicy` and
`Client.SetCustomerBillingPolicy` (GET/PUT on
`/v1/merchant/customers/{customer_id}/billing-policy`) to read, assign or clear
one customer's explicit policy. A required nullable `policy_name` distinguishes
clearing from an incomplete request. PUT rejects customer bindings and preserves existing runtime
rows. Removing a named policy still referenced by a customer is refused, preserving
the entire previous document. This contract contains no global consumer or wallet
state; those remain OpenRails-SaaS responsibilities.
