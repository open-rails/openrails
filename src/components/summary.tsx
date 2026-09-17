// Order summary: the money story. Full variant (wide containers) is the
// receipt — line items, tax, a single emphasized rule above "Due today".
// Compact variant (narrow containers) is one line: who + what left, how much
// right.
import { cn } from "#orck/lib/utils"
import {
  addAmounts,
  formatAmount,
  formatPeriod,
  periodNoun,
  type Amount,
} from "#orck/lib/money"
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
      amount: session.plan.unit_amount,
    },
  ]
}

// dueToday is the host's figure when it sends one; otherwise the exact sum of
// the line items and tax. An int64 overflow (null) is refused by the
// formatter, never rounded.
function dueToday(session: CheckoutSession): Amount | null {
  if (session.due_today !== undefined) return session.due_today
  return addAmounts(
    ...lineItems(session).map((item) => item.amount),
    session.tax ?? "0"
  )
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
  const { currency, unit_decimals: decimals } = session.plan
  const money = (amount: Amount | null) =>
    formatAmount(amount, currency, decimals)
  return (
    <div className="grid content-start gap-5">
      <Eyebrow>{session.merchant.display_name}</Eyebrow>
      <div className="text-[38px] leading-[1.05] font-semibold tracking-[-0.02em] tabular-nums">
        {money(session.plan.unit_amount)}
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
            amount={money(item.amount)}
          />
        ))}
        {session.tax !== undefined ? (
          <Row label="Tax" amount={money(session.tax)} quiet />
        ) : null}
        <div className="-mt-px flex items-start justify-between gap-4 border-t border-border pt-[13px] text-[13.5px] font-semibold">
          <span>Due today</span>
          <span className="whitespace-nowrap tabular-nums">
            {money(dueToday(session))}
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
        {formatAmount(
          session.plan.unit_amount,
          session.plan.currency,
          session.plan.unit_decimals
        )}
        <span className="text-xs font-normal text-muted-foreground">
          {" "}
          {formatPeriod(session.plan.period_hours).replace("/ ", "/")}
        </span>
      </div>
    </div>
  )
}
