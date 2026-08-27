// CCBill REST/tokenized-card hand-off uses the compact billing identity below.
// Name stays canonical; the server derives IP and performs provider projection.
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
      <div className="grid grid-cols-2 gap-3">
        <CountryField
          id={`${idPrefix}-country`}
          value={value.country}
          onChange={set("country")}
          disabled={disabled}
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
      {error ? (
        <p className="text-destructive text-[13px]" role="alert">
          {error}
        </p>
      ) : null}
    </>
  )
}
