// The method list: flat hairline-separated rows on a shared radio group.
// Selection reads through the filled radio and the row title going ink —
// never a highlighted container (containment budget: one, owned by the
// host). Each row expands its body in place.
import { cn } from "cn"
import { Label } from "#orck/components/ui/label"
import { RadioGroup, RadioGroupItem } from "#orck/components/ui/radio-group"
import { RAIL_META, solanaHint, solanaToken } from "#orck/lib/rail-meta"
import type { PaymentRailOption } from "#orck/types"

export function MethodList({
  rails,
  selected,
  onSelect,
  disabled,
  idPrefix,
  renderBody,
}: {
  rails: PaymentRailOption[]
  selected: string
  onSelect: (optionID: string) => void
  disabled?: boolean
  idPrefix: string
  renderBody: (option: PaymentRailOption, active: boolean) => React.ReactNode
}) {
  return (
    <RadioGroup
      aria-label="Payment method"
      className="grid w-full gap-0"
      value={selected}
      onValueChange={(value) => {
        if (typeof value === "string" && value) onSelect(value)
      }}
      disabled={disabled}
    >
      {rails.map((option) => {
        const meta = RAIL_META[option.rail]
        const token = option.rail === "solana" ? solanaToken(option) : undefined
        const hint = token ? solanaHint(token) : meta.hint
        const active = option.id === selected
        const controlId = `${idPrefix}-option-${option.id}`
        return (
          <div
            key={option.id}
            className="border-t border-[color:var(--orck-hairline)] first:border-t-0"
          >
            <Label
              htmlFor={controlId}
              className={cn(
                "flex cursor-pointer items-center gap-2.5 py-[13px] text-sm font-medium text-muted-foreground select-none",
                active && "font-semibold text-foreground",
                disabled && "cursor-default"
              )}
            >
              <RadioGroupItem id={controlId} value={option.id} />
              {meta.label}
              {hint ? (
                <span className="ml-auto text-xs font-normal text-[color:var(--orck-faint)]">
                  {hint}
                </span>
              ) : null}
            </Label>
            {renderBody(option, active)}
          </div>
        )
      })}
    </RadioGroup>
  )
}

export function MethodBody({
  children,
  hidden,
}: {
  children: React.ReactNode
  hidden?: boolean
}) {
  return (
    <div
      aria-hidden={hidden || undefined}
      className={cn(
        "grid gap-3.5 pt-1 pb-[18px] pl-[26px]",
        hidden && "hidden"
      )}
    >
      {children}
    </div>
  )
}
