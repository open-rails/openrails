import type * as React from "react"

import { Input } from "#orck/components/ui/input"
import { Label } from "#orck/components/ui/label"
import {
  COUNTRY_OPTIONS,
  isPostalRequired,
  postalField,
  type NMIBilling,
} from "#orck/lib/billing"

const LABEL = "text-[13px] font-medium text-foreground"
const CONTROL =
  "h-[38px] rounded-[9px] border border-input bg-card text-sm text-foreground"
const INPUT = `${CONTROL} px-2.5`

export function BillingTextField({
  id,
  name,
  label,
  value,
  onChange,
  autoComplete,
  placeholder,
  disabled,
  maxLength,
  required,
  type,
  inputMode,
  pattern,
}: {
  id: string
  name: string
  label: string
  value: string
  onChange: (value: string) => void
  autoComplete: string
  placeholder?: string
  disabled?: boolean
  maxLength?: number
  required?: boolean
  type?: React.HTMLInputTypeAttribute
  inputMode?: React.HTMLAttributes<HTMLInputElement>["inputMode"]
  pattern?: string
}) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id} className={LABEL}>
        {label}
      </Label>
      <Input
        id={id}
        name={name}
        type={type}
        className={INPUT}
        value={value}
        autoComplete={autoComplete}
        placeholder={placeholder}
        disabled={disabled}
        maxLength={maxLength}
        required={required}
        inputMode={inputMode}
        pattern={pattern}
        onChange={(event) => onChange(event.target.value)}
      />
    </div>
  )
}

export function CountryField({
  id,
  value,
  onChange,
  disabled,
}: {
  id: string
  value: string
  onChange: (value: string) => void
  disabled?: boolean
}) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id} className={LABEL}>
        Country
      </Label>
      <select
        id={id}
        name="country"
        className={`${CONTROL} w-full px-2.5`}
        value={value}
        autoComplete="billing country"
        disabled={disabled}
        required
        onChange={(event) => onChange(event.target.value)}
      >
        <option value="">Select country</option>
        {COUNTRY_OPTIONS.map((country) => (
          <option key={country.code} value={country.code}>
            {country.name}
          </option>
        ))}
      </select>
    </div>
  )
}

export function PostalField({
  id,
  value,
  country,
  onChange,
  disabled,
  required,
  error,
}: {
  id: string
  value: string
  country: string
  onChange: (value: string) => void
  disabled?: boolean
  required: boolean
  error?: string
}) {
  const field = postalField(country, required)
  return (
    <div className="grid gap-1.5">
      <BillingTextField
        id={id}
        name="zip"
        label={field.label}
        value={value}
        onChange={onChange}
        autoComplete="billing postal-code"
        placeholder={field.placeholder}
        disabled={disabled}
        maxLength={32}
        required={required}
        inputMode={field.inputMode}
        pattern={field.pattern}
      />
      {error ? (
        <p className="text-[12.5px] text-destructive" role="alert">
          {error}
        </p>
      ) : null}
    </div>
  )
}

export function CardBillingFields({
  idPrefix,
  value,
  onChange,
  disabled,
  postalError,
}: {
  idPrefix: string
  value: NMIBilling
  onChange: (next: NMIBilling) => void
  disabled?: boolean
  postalError?: string
}) {
  const set = (key: keyof NMIBilling) => (next: string) =>
    onChange({ ...value, [key]: next })
  const postalRequired = isPostalRequired(value.country)
  return (
    <div className="grid gap-3">
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
          id={`${idPrefix}-postal-code`}
          value={value.zip}
          country={value.country}
          onChange={set("zip")}
          disabled={disabled}
          required={postalRequired}
          error={postalError}
        />
      </div>
    </div>
  )
}
