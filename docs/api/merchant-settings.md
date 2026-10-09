# Merchant settings

A merchant's settings (`billing.MerchantSettings`) are part of its
configuration: `GET /v1/admin/configuration` (`Client.GetMerchantConfiguration`)
reads them with the configuration's revision, and
`PATCH /v1/admin/configuration` (`Client.UpdateMerchantConfiguration`)
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

A customer's own policy is one of its customer settings (below), outside the
merchant settings. Removing a named policy a customer is still bound to is
refused, and the previous settings stay.

## Customer settings

`billing.CustomerSettings` is what the merchant sets for one customer:
`credit_limits` and `trust_levels` by currency, `billing_policy` and
`invoice_profile`. A field at its default is absent: no entry for a currency
without a limit or level, a null `billing_policy` inherits the tier's or the
default policy, and a null `invoice_profile` invoices at net 0, charged
automatically.

- `Client.GetCustomer` (`GET /v1/admin/customers/{customer_id}`) answers
  them as the customer's `settings`; `Client.ListCustomers` does for each
  customer it lists.
- `Client.UpdateCustomer` (`PATCH /v1/admin/customers/{customer_id}`)
  changes one customer's settings and answers the customer. It changes only
  the fields it names: `credit_limits` and `trust_levels` merge by currency
  (amount `"0"` or level `""` clears one), and `null` clears `billing_policy`
  or `invoice_profile`.
- The change is validated whole before anything is written. A refusal names
  the field, as in `credit_limits[0].amount`. A customer that does not exist
  is `404 customer_not_found` (settings never create one), and an undeclared
  policy is `404 billing_policy_not_found`.
- The read needs `Permissions.AdminRead` and the write `AdminUpdate`. Every
  field can change the customer's spending authority, so the write is
  sensitive: a person needs a recent sign-in for it (`step_up_required`). Writes count against the
  per-administrator grant limit, one per item.

```json
{"items": [{"customer_id": "…", "credit_limits": [{"currency": "USD", "amount": "50000000"}], "billing_policy": "cloud_monthly"}]}
```
