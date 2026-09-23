import * as React from "react"
import { cn } from "cn"

import type { CheckoutAppearance } from "#orck/appearance"
import type { Payment } from "#orck/client/types"
import { Button } from "#orck/components/ui/button"
import { useMessages } from "#orck/i18n/context"
import type { MessageKey, Translator } from "#orck/i18n/messages"
import { usePayments, type PaymentsOptions } from "#orck/react/hooks"
import { useBillingClient } from "#orck/react/context"
import { useUiSettings } from "#orck/scope-context"
import { brandName, formatDate, formatMoney, paymentItem } from "./format"
import { EmptyState, ErrorState, ListSkeleton, Section } from "./section"
import { BillingStatusBadge } from "./status-badge"

export interface PaymentHistoryProps extends PaymentsOptions {
  appearance?: CheckoutAppearance
  className?: string
}

function methodText(p: Payment, m: Translator): string {
  if (p.card?.last4)
    return `${brandName(p.card.brand, m.t("paymentMethods.fallbackBrand"))} •••• ${p.card.last4}`
  const rail = p.rail ?? ""
  return rail in m.messages.history.rail
    ? m.t(`history.rail.${rail}` as MessageKey)
    : rail || m.t("common.notAvailable")
}

export function PaymentHistory({
  appearance,
  className,
  ...options
}: PaymentHistoryProps) {
  const m = useMessages()
  const { t } = m
  const { locale } = useUiSettings()
  const state = usePayments(options)
  const scales = useBillingClient().currencies

  let body: React.ReactNode
  const payments = state.payments
  if (payments === null && state.error) {
    body = <ErrorState error={state.error} onRetry={state.refetch} />
  } else if (payments === null) {
    body = <ListSkeleton rows={3} />
  } else if (payments.length === 0 && state.page === 0) {
    body = (
      <EmptyState
        title={t("history.empty")}
        description={t("history.emptyDescription")}
      />
    )
  } else {
    body = (
      <div className="-mx-1 overflow-x-auto px-1">
        <table
          className={cn(
            "w-full border-collapse text-sm",
            state.loading && "opacity-60 transition-opacity"
          )}
          aria-busy={state.loading || undefined}
        >
          <thead>
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th
                scope="col"
                className="hidden py-2 pr-3 font-medium @md:table-cell"
              >
                {t("history.date")}
              </th>
              <th scope="col" className="py-2 pr-3 font-medium">
                {t("history.item")}
              </th>
              <th
                scope="col"
                className="hidden py-2 pr-3 font-medium @md:table-cell"
              >
                {t("history.method")}
              </th>
              <th scope="col" className="py-2 pr-3 font-medium">
                {t("history.status")}
              </th>
              <th scope="col" className="py-2 text-right font-medium">
                {t("history.amount")}
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border">
            {payments.map((p) => {
              const refund = p.object === "refund"
              const status = p.status || (refund ? "refunded" : "unknown")
              const amount = formatMoney(p.amount, p.currency, scales, locale)
              const refunded =
                !refund && p.amount_refunded && p.amount_refunded !== "0"
                  ? formatMoney(p.amount_refunded, p.currency, scales, locale)
                  : null
              const date =
                formatDate(p.created_at, locale) ?? t("common.notAvailable")
              const item = paymentItem(p, m)
              return (
                <tr key={p.id} data-testid="payment-row" data-payment-id={p.id}>
                  <td className="hidden py-3 pr-3 align-top whitespace-nowrap tabular-nums @md:table-cell">
                    {date}
                  </td>
                  <td className="py-3 pr-3 align-top">
                    <div data-testid="payment-item" className="font-medium">
                      {item.name}
                    </div>
                    {item.detail ? (
                      <div
                        data-testid="payment-period"
                        className="text-xs text-muted-foreground"
                      >
                        {item.detail}
                      </div>
                    ) : null}
                    <div className="text-xs text-muted-foreground tabular-nums @md:hidden">
                      {date} · {methodText(p, m)}
                    </div>
                  </td>
                  <td className="hidden py-3 pr-3 align-top text-muted-foreground tabular-nums @md:table-cell">
                    {methodText(p, m)}
                  </td>
                  <td className="py-3 pr-3 align-top">
                    <BillingStatusBadge
                      status={status}
                      label={refund ? t("history.refund") : undefined}
                    />
                  </td>
                  <td className="py-3 text-right align-top font-medium whitespace-nowrap tabular-nums">
                    {amount ?? t("common.notAvailable")}
                    {refunded ? (
                      <div className="text-xs font-normal text-muted-foreground">
                        {t("history.refundedAmount", { amount: refunded })}
                      </div>
                    ) : null}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
    )
  }

  const paged = state.page > 0 || state.hasMore
  return (
    <Section
      title={t("history.title")}
      description={t("history.description")}
      appearance={appearance}
      className={className}
      data-testid="payment-history"
    >
      {body}
      {payments !== null && state.error ? (
        <div className="pt-3">
          <ErrorState error={state.error} onRetry={state.refetch} />
        </div>
      ) : null}
      {paged ? (
        <nav
          aria-label={t("history.title")}
          className="flex items-center justify-between gap-3 pt-4"
        >
          <span className="text-xs text-muted-foreground tabular-nums">
            {t("history.page", { page: state.page + 1 })}
          </span>
          <div className="flex gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={state.page === 0 || state.loading}
              onClick={state.previous}
            >
              {t("common.previous")}
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={!state.hasMore || state.loading}
              onClick={state.next}
            >
              {t("common.next")}
            </Button>
          </div>
        </nav>
      ) : null}
    </Section>
  )
}
