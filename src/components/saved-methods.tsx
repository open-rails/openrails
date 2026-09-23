// Reuse of a card the customer already stored with this merchant. Selecting
// one skips gateway tokenization entirely — the pay request carries the
// stored method's id instead of a fresh token. Rows stay surfaceless like the
// rail list above them; the brand plate and ink shift carry selection, so the
// containment budget still belongs to the host.
import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon } from "@hugeicons/core-free-icons"
import { CardBrandPlate } from "#orck/components/card-brands"

import { Label } from "#orck/components/ui/label"
import { RadioGroup, RadioGroupItem } from "#orck/components/ui/radio-group"
import { cn } from "cn"
import type { SavedPaymentMethod } from "#orck/types"

export const NEW_CARD_VALUE = "new"

// Brand plates carry the network's own wordmark treatment at small size:
// recognizable at a glance without shipping licensed logo art.
const BRAND_PLATE: Record<string, string> = {
  visa: "VISA",
  mastercard: "MC",
  amex: "AMEX",
  discover: "DISC",
  jcb: "JCB",
  diners: "DINERS",
}

function brandPlate(brand?: string): string {
  const key = brand?.trim().toLowerCase() ?? ""
  return BRAND_PLATE[key] ?? "CARD"
}

// The plate is decorative; assistive tech gets the spoken brand instead.
function brandName(brand?: string): string {
  const value = brand?.trim()
  if (!value) return "Card"
  return value[0].toUpperCase() + value.slice(1)
}

function expiryLabel(method: SavedPaymentMethod): string | undefined {
  if (!method.exp_month || !method.exp_year) return undefined
  const month = String(method.exp_month).padStart(2, "0")
  return `Expires ${month}/${String(method.exp_year).slice(-2)}`
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
              brand={method.brand}
              fallback={brandPlate(method.brand)}
            />
            <span
              className={cn(
                "text-muted-foreground min-w-0 flex-1 truncate text-sm font-medium tabular-nums transition-colors",
                active && "text-foreground font-semibold"
              )}
            >
              <span className="sr-only">
                {brandName(method.brand)} card ending {method.last_four}
              </span>
              <span aria-hidden>•••• {method.last_four}</span>
            </span>
            {expires ? (
              <span className="shrink-0 text-xs font-normal text-[color:var(--orck-faint)] tabular-nums">
                {expires}
              </span>
            ) : null}
          </Label>
        )
      })}
      <Label
        htmlFor={`${idPrefix}-new`}
        className={cn(
          "text-muted-foreground flex cursor-pointer items-center gap-3 py-1.5 text-sm font-medium select-none",
          value === NEW_CARD_VALUE && "text-foreground font-semibold",
          disabled && "cursor-default"
        )}
      >
        <RadioGroupItem id={`${idPrefix}-new`} value={NEW_CARD_VALUE} />
        <span
          aria-hidden
          className={cn(
            "border-border text-muted-foreground grid h-7 w-11 shrink-0 place-items-center rounded-md border border-dashed transition-colors",
            value === NEW_CARD_VALUE &&
              "bg-foreground text-background border-solid border-transparent"
          )}
        >
          <HugeiconsIcon icon={Add01Icon} className="size-3.5" />
        </span>
        Use a new card
      </Label>
    </RadioGroup>
  )
}
