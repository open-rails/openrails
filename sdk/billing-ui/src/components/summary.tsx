// Order summary: the money story. Full variant (wide containers) is the
// receipt — line items, tax, a single emphasized rule above "Due today".
// Compact variant (narrow containers) is one line: who + what left, how much
// right.
import { cn } from "cn"
import { useMessages } from "#orck/i18n/context"
import type { Translator } from "#orck/i18n/messages"
import { addAmounts, formatAmount, type Amount } from "#orck/lib/money"
import { accessLabel, everyLabel, perLabel } from "#orck/lib/period"
import type { CheckoutLineItem, CheckoutSession } from "#orck/types"

function renewsLabel(session: CheckoutSession, m: Translator) {
  const every = session.plan.auto_renew
    ? everyLabel(session.plan.billing_interval_hours, m)
    : null
  const access = !session.plan.auto_renew || session.plan.access_duration_hours !== session.plan.billing_interval_hours
    ? accessLabel(session.plan.access_duration_hours, m) ?? m.t("interval.permanent")
    : undefined
  return [every ? m.t("checkout.renews", { period: every }) : undefined, access]
    .filter(Boolean).join(" · ") || undefined
}

function lineItems(
  session: CheckoutSession,
  m: Translator
): CheckoutLineItem[] {
  if (session.line_items && session.line_items.length > 0) {
    return session.line_items
  }
  return [
    {
      label: session.plan.display_name,
      sublabel: renewsLabel(session, m),
      amount: session.plan.unit_amount,
    },
  ]
}

// dueToday is the session's due_today when sent, else the exact sum of the
// line items and tax. An int64 overflow (null) is refused by the formatter,
// never rounded.
function dueToday(session: CheckoutSession, m: Translator): Amount | null {
  if (session.due_today !== undefined) return session.due_today
  return addAmounts(
    ...lineItems(session, m).map((item) => item.amount),
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
  const m = useMessages()
  const { currency, unit_decimals: decimals } = session.plan
  const money = (amount: Amount | null) =>
    formatAmount(amount, currency, decimals)
  return (
    <div className="grid content-start gap-5">
      <Eyebrow>{session.merchant.display_name}</Eyebrow>
      <div className="text-[38px] leading-[1.05] font-semibold tracking-[-0.02em] tabular-nums">
        {money(session.plan.unit_amount)}
        <span className="ml-1 text-[15px] font-normal tracking-normal text-muted-foreground">
          {session.plan.auto_renew ? perLabel(session.plan.billing_interval_hours, m) : null}
        </span>
      </div>
      <div className="grid">
        {lineItems(session, m).map((item) => (
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
            {money(dueToday(session, m))}
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
  const m = useMessages()
  const renews = renewsLabel(session, m)
  const per = session.plan.auto_renew ? perLabel(session.plan.billing_interval_hours, m) : null
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
          {renews ? ` · ${renews}` : ""}
        </div>
      </div>
      <div className="text-[22px] font-semibold tracking-[-0.01em] whitespace-nowrap tabular-nums">
        {formatAmount(
          session.plan.unit_amount,
          session.plan.currency,
          session.plan.unit_decimals
        )}
        {per ? (
          <span className="text-xs font-normal text-muted-foreground">
            {" "}
            {per.replace("/ ", "/")}
          </span>
        ) : null}
      </div>
    </div>
  )
}
