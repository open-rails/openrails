// CCBill requires a complete billing address before it can build the hosted
// FlexForms hand-off. Name stays canonical in this UI and on the wire;
// OpenRails performs any provider-specific split at its boundary.
import {
  BillingTextField,
  CountryField,
  PostalField,
} from "#orck/components/billing-fields"
import type { CCBillBilling } from "#orck/lib/ccbill"

export function CCBillFields({
  idPrefix,
  value,
  onChange,
  disabled,
  error,
}: {
  idPrefix: string
  value: CCBillBilling
  onChange: (next: CCBillBilling) => void
  disabled?: boolean
  error?: string
}) {
  const set = (key: keyof CCBillBilling) => (next: string) =>
    onChange({ ...value, [key]: next })
  return (
    <>
      <BillingTextField
        id={`${idPrefix}-email`}
        name="email"
        label="Email"
        value={value.email}
        onChange={set("email")}
        autoComplete="email"
        placeholder="jane@example.com"
        disabled={disabled}
        type="email"
        maxLength={320}
        required
      />
      <BillingTextField
        id={`${idPrefix}-name-on-card`}
        name="name_on_card"
        label="Name on card"
        value={value.name_on_card}
        onChange={set("name_on_card")}
        autoComplete="cc-name"
        placeholder="Name as shown on card"
        disabled={disabled}
        maxLength={200}
        required
      />
      <BillingTextField
        id={`${idPrefix}-address`}
        name="address1"
        label="Address"
        value={value.address1}
        onChange={set("address1")}
        autoComplete="billing address-line1"
        placeholder="123 Main St"
        disabled={disabled}
        maxLength={200}
        required
      />
      <div className="grid grid-cols-2 gap-3">
        <BillingTextField
          id={`${idPrefix}-city`}
          name="city"
          label="City"
          value={value.city}
          onChange={set("city")}
          autoComplete="billing address-level2"
          placeholder="Springfield"
          disabled={disabled}
          maxLength={100}
          required
        />
        <PostalField
          id={`${idPrefix}-zip`}
          value={value.zip}
          country={value.country}
          onChange={set("zip")}
          disabled={disabled}
          required
        />
      </div>
      <div className="grid grid-cols-2 gap-3">
        <BillingTextField
          id={`${idPrefix}-state`}
          name="state"
          label="State / region (optional)"
          value={value.state ?? ""}
          onChange={set("state")}
          autoComplete="billing address-level1"
          placeholder="IL"
          disabled={disabled}
          maxLength={100}
        />
        <CountryField
          id={`${idPrefix}-country`}
          value={value.country}
          onChange={set("country")}
          disabled={disabled}
        />
      </div>
      {error ? (
        <p className="text-destructive text-[13px]" role="alert">
          {error}
        </p>
      ) : null}
    </>
  )
}
