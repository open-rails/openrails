// Cards the customer already stored with this merchant. Selecting one skips
// gateway tokenization: the pay request carries the stored method's id. Rows
// stay surfaceless like the rail list; the brand plate and ink carry selection.
import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon } from "@hugeicons/core-free-icons"
import { CardBrandPlate } from "#orck/components/card-brands"

import { Label } from "#orck/components/ui/label"
import { RadioGroup, RadioGroupItem } from "#orck/components/ui/radio-group"
import { cn } from "cn"
import type { SavedPaymentMethod } from "#orck/types"

export const NEW_CARD_VALUE = "new"

// Text plates for brands CardBrandPlate has no mark for.
const BRAND_PLATE: Record<string, string> = {
  visa: "VISA",
  mastercard: "MC",
  amex: "AMEX",
  discover: "DISC",
  jcb: "JCB",
  diners: "DINERS",
}

function brandPlate(brand?: string | null): string {
  const key = brand?.trim().toLowerCase() ?? ""
  return BRAND_PLATE[key] ?? "CARD"
}

// The plate is decorative; assistive tech gets the spoken brand instead.
function brandName(brand?: string | null): string {
  const value = brand?.trim()
  if (!value) return "Card"
  return value[0].toUpperCase() + value.slice(1)
}

function expiryLabel(method: SavedPaymentMethod): string | undefined {
  const card = method.card
  if (!card?.exp_month || !card.exp_year) return undefined
  const month = String(card.exp_month).padStart(2, "0")
  return `${month}/${String(card.exp_year).slice(-2)}`
}

export function SavedMethods({
  methods,
  value,
  onChange,
  disabled,
  idPrefix,
}: {
  methods: SavedPaymentMethod[]
  value: string
  onChange: (value: string) => void
  disabled?: boolean
  idPrefix: string
}) {
  return (
    <RadioGroup
      value={value}
      onValueChange={(next) => {
        if (typeof next === "string" && next) onChange(next)
      }}
      disabled={disabled}
      aria-label="Saved cards"
      className="grid gap-0"
    >
      {methods.map((method) => {
        const id = `${idPrefix}-${method.id}`
        const active = value === method.id
        const expires = expiryLabel(method)
        const brand = method.card?.brand
        const last4 = method.card?.last4
        return (
          <Label
            key={method.id}
            htmlFor={id}
            className={cn(
              "flex cursor-pointer items-center gap-3 py-1.5 select-none",
              disabled && "cursor-default"
            )}
          >
            <RadioGroupItem id={id} value={method.id} />
            <CardBrandPlate
              brand={brand ?? undefined}
              fallback={brandPlate(brand)}
            />
            <span
              className={cn(
                "min-w-0 flex-1 truncate text-sm font-medium text-muted-foreground tabular-nums transition-colors",
                active && "font-semibold text-foreground"
              )}
            >
              <span className="sr-only">
                {brandName(brand)} card ending {last4}
                {expires ? `, expires ${expires}` : ""}
              </span>
              <span aria-hidden>
                {brandName(brand)} •••• {last4 ?? "····"}
                {expires ? (
                  <span className="font-normal text-muted-foreground">
                    {" "}
                    · {expires}
                  </span>
                ) : null}
              </span>
            </span>
          </Label>
        )
      })}
      <Label
        htmlFor={`${idPrefix}-new`}
        className={cn(
          "flex cursor-pointer items-center gap-3 py-1.5 text-sm font-medium text-muted-foreground select-none",
          value === NEW_CARD_VALUE && "font-semibold text-foreground",
          disabled && "cursor-default"
        )}
      >
        <RadioGroupItem id={`${idPrefix}-new`} value={NEW_CARD_VALUE} />
        <span
          aria-hidden
          className={cn(
            "grid h-7 w-11 shrink-0 place-items-center rounded-md border border-dashed border-border text-muted-foreground transition-colors",
            value === NEW_CARD_VALUE &&
              "border-solid border-transparent bg-foreground text-background"
          )}
        >
          <HugeiconsIcon icon={Add01Icon} className="size-3.5" />
        </span>
        Use a new card
      </Label>
    </RadioGroup>
  )
}
