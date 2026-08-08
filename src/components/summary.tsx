// Order summary: the money story. Full variant (wide containers) is the
// receipt — line items, tax, a single emphasized rule above "Due today".
// Compact variant (narrow containers) is one line: who + what left, how much
// right.
import { cn } from "#orck/lib/utils"
import { formatMoney, formatPeriod, periodNoun } from "#orck/lib/money"
import type { CheckoutLineItem, CheckoutSession } from "#orck/types"

function lineItems(session: CheckoutSession): CheckoutLineItem[] {
  if (session.line_items && session.line_items.length > 0) {
    return session.line_items
  }
  const renews = session.plan.automatically_renews
  const noun = periodNoun(session.plan.period_hours)
  return [
    {
      label: session.plan.display_name,
      sublabel: renews && noun ? `Renews ${noun}` : undefined,
      amount_micros: session.plan.unit_amount_micros,
    },
  ]
}

function dueToday(session: CheckoutSession): number {
  if (session.due_today_micros !== undefined) return session.due_today_micros
  const items = lineItems(session).reduce(
    (total, item) => total + item.amount_micros,
    0
  )
  return items + (session.tax_micros ?? 0)
}

export function Eyebrow({
  children,
  className,
}: {
  children: React.ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        "text-[11px] font-semibold tracking-[0.09em] text-muted-foreground uppercase",
        className
      )}
    >
      {children}
    </div>
  )
}

function Row({
  label,
  sublabel,
  amount,
  quiet,
}: {
  label: string
  sublabel?: string
  amount: string
  quiet?: boolean
}) {
  return (
    <div
      className={cn(
        "flex items-start justify-between gap-4 border-b border-[color:var(--orck-hairline)] py-[11px] text-[13.5px]",
        quiet && "text-muted-foreground"
      )}
    >
      <span>
        {label}
        {sublabel ? (
          <span className="block text-xs text-muted-foreground">
            {sublabel}
          </span>
        ) : null}
      </span>
      <span className="whitespace-nowrap tabular-nums">{amount}</span>
    </div>
  )
}

export function OrderSummary({ session }: { session: CheckoutSession }) {
  const currency = session.plan.currency
  return (
    <div className="grid content-start gap-5">
      <Eyebrow>{session.merchant.display_name}</Eyebrow>
      <div className="text-[38px] leading-[1.05] font-semibold tracking-[-0.02em] tabular-nums">
        {formatMoney(session.plan.unit_amount_micros, currency)}
        <span className="ml-1 text-[15px] font-normal tracking-normal text-muted-foreground">
          {formatPeriod(session.plan.period_hours)}
        </span>
      </div>
      <div className="grid">
        {lineItems(session).map((item) => (
          <Row
            key={item.label}
            label={item.label}
            sublabel={item.sublabel}
            amount={formatMoney(item.amount_micros, currency)}
          />
        ))}
        {session.tax_micros !== undefined ? (
          <Row
            label="Tax"
            amount={formatMoney(session.tax_micros, currency)}
            quiet
          />
        ) : null}
        <div className="-mt-px flex items-start justify-between gap-4 border-t border-border pt-[13px] text-[13.5px] font-semibold">
          <span>Due today</span>
          <span className="whitespace-nowrap tabular-nums">
            {formatMoney(dueToday(session), currency)}
          </span>
        </div>
      </div>
    </div>
  )
}

export function CompactSummary({
  session,
  className,
}: {
  session: CheckoutSession
  className?: string
}) {
  const renews = session.plan.automatically_renews
  const noun = periodNoun(session.plan.period_hours)
  return (
    <div
      className={cn(
        "flex items-baseline justify-between gap-3 border-b border-[color:var(--orck-hairline)] pb-3.5",
        className
      )}
    >
      <div className="grid gap-0.5">
        <Eyebrow>{session.merchant.display_name}</Eyebrow>
        <div className="text-[12.5px] text-muted-foreground">
          {session.plan.display_name}
          {renews && noun ? ` · renews ${noun}` : ""}
        </div>
      </div>
      <div className="text-[22px] font-semibold tracking-[-0.01em] whitespace-nowrap tabular-nums">
        {formatMoney(session.plan.unit_amount_micros, session.plan.currency)}
        <span className="text-xs font-normal text-muted-foreground">
          {" "}
          {formatPeriod(session.plan.period_hours).replace("/ ", "/")}
        </span>
      </div>
    </div>
  )
}
