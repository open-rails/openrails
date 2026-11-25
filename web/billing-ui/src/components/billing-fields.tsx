import type * as React from "react"

import { Input } from "#orck/components/ui/input"
import { Label } from "#orck/components/ui/label"
import {
  COUNTRY_OPTIONS,
  postalField,
  postalMode,
  type BillingFields,
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
  required = true,
}: {
  id: string
  value: string
  onChange: (value: string) => void
  disabled?: boolean
  required?: boolean
}) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id} className={LABEL}>
        {required ? "Country or region" : "Country or region (optional)"}
      </Label>
      <select
        id={id}
        name="country"
        className={`${CONTROL} w-full px-2.5`}
        value={value}
        autoComplete="billing country"
        disabled={disabled}
        required={required}
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

// The card fields (children) sit between the cardholder name and the
// address, whose fields the merchant's billing fields choose.
export function CardBillingFields({
  idPrefix,
  value,
  onChange,
  fields,
  disabled,
  postalError,
  children,
}: {
  idPrefix: string
  value: NMIBilling
  onChange: (next: NMIBilling) => void
  fields: BillingFields
  disabled?: boolean
  postalError?: string
  children?: React.ReactNode
}) {
  const set = (key: keyof NMIBilling) => (next: string) =>
    onChange({ ...value, [key]: next })
  const country = fields.country === "hidden" ? "" : value.country
  const postal = postalMode(fields, country)
  const shown =
    Number(fields.country !== "hidden") + Number(postal !== "hidden")
  return (
    <div className="grid gap-3">
      <BillingTextField
        id={`${idPrefix}-name-on-card`}
        name="name_on_card"
        label="Full name"
        value={value.name_on_card}
        onChange={set("name_on_card")}
        autoComplete="cc-name"
        disabled={disabled}
        maxLength={200}
        required
      />
      {children}
      {shown > 0 ? (
        <div className={shown > 1 ? "grid grid-cols-2 gap-3" : "grid gap-3"}>
          {fields.country !== "hidden" ? (
            <CountryField
              id={`${idPrefix}-country`}
              value={value.country}
              onChange={set("country")}
              disabled={disabled}
              required={fields.country === "required"}
            />
          ) : null}
          {postal !== "hidden" ? (
            <PostalField
              id={`${idPrefix}-postal-code`}
              value={value.zip}
              country={country}
              onChange={set("zip")}
              disabled={disabled}
              required={postal === "required"}
              error={postalError}
            />
          ) : null}
        </div>
      ) : null}
    </div>
  )
}
