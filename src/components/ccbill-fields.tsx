// CCBill billing identity. The engine's ccbill rail requires these fields
// before it will build the FlexForms hand-off (requireBillingFields); we
// validate with the same rules client-side so the customer gets field-level
// errors instead of a burned attempt. State is optional; country is
// ISO-3166 alpha-2.
import { Input } from "#orck/components/ui/input"
import { Label } from "#orck/components/ui/label"
import type { CCBillBilling } from "#orck/lib/ccbill"

const LABEL = "text-[13px] font-medium text-foreground"
const INPUT = "h-[38px] rounded-[9px] bg-card text-sm"

function Field({
  id,
  label,
  value,
  onChange,
  autoComplete,
  placeholder,
  disabled,
  maxLength,
}: {
  id: string
  label: string
  value: string
  onChange: (value: string) => void
  autoComplete?: string
  placeholder?: string
  disabled?: boolean
  maxLength?: number
}) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id} className={LABEL}>
        {label}
      </Label>
      <Input
        id={id}
        className={INPUT}
        value={value}
        autoComplete={autoComplete}
        placeholder={placeholder}
        disabled={disabled}
        maxLength={maxLength}
        onChange={(event) => onChange(event.target.value)}
      />
    </div>
  )
}

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
      <Field
        id={`${idPrefix}-email`}
        label="Email"
        value={value.email}
        onChange={set("email")}
        autoComplete="email"
        placeholder="jane@example.com"
        disabled={disabled}
      />
      <div className="grid grid-cols-2 gap-3">
        <Field
          id={`${idPrefix}-first`}
          label="First name"
          value={value.first_name}
          onChange={set("first_name")}
          autoComplete="given-name"
          placeholder="Jane"
          disabled={disabled}
        />
        <Field
          id={`${idPrefix}-last`}
          label="Last name"
          value={value.last_name}
          onChange={set("last_name")}
          autoComplete="family-name"
          placeholder="Doe"
          disabled={disabled}
        />
      </div>
      <Field
        id={`${idPrefix}-address`}
        label="Address"
        value={value.address1}
        onChange={set("address1")}
        autoComplete="address-line1"
        placeholder="123 Main St"
        disabled={disabled}
      />
      <div className="grid grid-cols-2 gap-3">
        <Field
          id={`${idPrefix}-city`}
          label="City"
          value={value.city}
          onChange={set("city")}
          autoComplete="address-level2"
          placeholder="Springfield"
          disabled={disabled}
        />
        <Field
          id={`${idPrefix}-zip`}
          label="ZIP"
          value={value.zip}
          onChange={set("zip")}
          autoComplete="postal-code"
          placeholder="62704"
          disabled={disabled}
        />
      </div>
      <div className="grid grid-cols-2 gap-3">
        <Field
          id={`${idPrefix}-state`}
          label="State (optional)"
          value={value.state ?? ""}
          onChange={set("state")}
          autoComplete="address-level1"
          placeholder="IL"
          disabled={disabled}
        />
        <Field
          id={`${idPrefix}-country`}
          label="Country"
          value={value.country}
          onChange={(next) =>
            onChange({ ...value, country: next.toUpperCase() })
          }
          autoComplete="country"
          placeholder="US"
          disabled={disabled}
          maxLength={2}
        />
      </div>
      {error ? (
        <p className="text-[13px] text-destructive" role="alert">
          {error}
        </p>
      ) : null}
    </>
  )
}
