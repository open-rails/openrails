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

- `Client.ListCustomerSettings` (`GET /v1/merchant/customers/settings`) lists
  them, newest customer first. `ids=a,b` instead reads 1 to 100 named
  customers in one page, with no other parameter; an unknown one is absent.
- `Client.UpdateCustomerSettings` (`PATCH /v1/merchant/customers/settings`)
  changes 1 to 100 distinct customers, all or none, and answers their settings
  in request order. An item changes only the fields it names:
  `credit_limits` and `trust_levels` merge by currency (amount `"0"` or level
  `""` clears one), and `null` clears `billing_policy` or `invoice_profile`.
- The batch is validated whole before anything is written. A refusal names
  the field, as in `items[1].credit_limits[0].amount`. A customer that does
  not exist is `404 customer_not_found` (settings never create one), and an
  undeclared policy is `404 billing_policy_not_found`.
- The read is a staff read and the write a staff write, both in the
  `openrails.CustomerSettings` resource group: a host that wants settings
  writes stricter guards that group. Every field can change the customer's
  spending authority, so the write is sensitive: a person needs a recent
  sign-in for it (`step_up_required`). Writes count against the
  per-administrator grant limit, one per item.

```json
{"items": [{"customer_id": "…", "credit_limits": [{"currency": "USD", "amount": "50000000"}], "billing_policy": "cloud_monthly"}]}
```
