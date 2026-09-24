import * as React from "react"
import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowUpRight01Icon } from "@hugeicons/core-free-icons"
import { cn } from "cn"

import type { CheckoutAppearance } from "#orck/appearance"
import type { SendSolanaTransaction } from "#orck/client/client"
import type { Subscription } from "#orck/client/types"
import { Button, buttonVariants } from "#orck/components/ui/button"
import { Spinner } from "#orck/components/ui/spinner"
import { useMessages } from "#orck/i18n/context"
import { useSubscriptions, type SubscriptionsOptions } from "#orck/react/hooks"
import { useBillingClient } from "#orck/react/context"
import { useUiSettings } from "#orck/scope-context"
import { CancelSubscriptionDialog } from "./cancel-dialog"
import { ChangeCardDialog } from "./change-card-dialog"
import {
  brandName,
  formatDate,
  formatMoney,
  intervalLabel,
  isEnding,
  isLive,
  LIVE_STATUSES,
  subscriptionName,
} from "./format"
import { EmptyState, ErrorState, ListSkeleton, Section } from "./section"
import { useNotice } from "./notice"
import { BillingStatusBadge } from "./status-badge"

export interface SubscriptionsPanelProps extends SubscriptionsOptions {
  /**
   * Wallet for Solana-rail subscriptions. Without it their cancel action is
   * hidden.
   */
  sendSolanaTransaction?: SendSolanaTransaction
  /** Empty-state link to the host's plans page (uses `navigate` if set). */
  plansHref?: string
  /** Host content under a row, e.g. a plan-change control. */
  renderSubscriptionFooter?: (subscription: Subscription) => React.ReactNode
  appearance?: CheckoutAppearance
  className?: string
}

export function SubscriptionsPanel({
  sendSolanaTransaction,
  plansHref,
  renderSubscriptionFooter,
  appearance,
  className,
  ...options
}: SubscriptionsPanelProps) {
  const m = useMessages()
  const { t } = m
  const { locale, navigate } = useUiSettings()
  const state = useSubscriptions(options)
  const scales = useBillingClient().currencies
  const [cancelling, setCancelling] = React.useState<Subscription | null>(null)
  const [changingCard, setChangingCard] = React.useState<Subscription | null>(
    null
  )
  const [rowError, setRowError] = React.useState<{
    id: string
    message: string
  } | null>(null)
  const [notice, announce] = useNotice()

  const rows = React.useMemo(() => {
    const list = state.subscriptions ?? []
    return [...list.filter(isLive), ...list.filter((s) => !isLive(s))]
  }, [state.subscriptions])

  const resume = async (s: Subscription) => {
    setRowError(null)
    const error = await state.resume(s.id)
    if (error) setRowError({ id: s.id, message: m.error(error) })
    else announce(t("subscriptions.resumed"))
  }

  const onChain = cancelling?.rail === "solana"

  let body: React.ReactNode
  if (state.subscriptions === null && state.error) {
    body = <ErrorState error={state.error} onRetry={state.refetch} />
  } else if (state.subscriptions === null) {
    body = <ListSkeleton />
  } else if (rows.length === 0) {
    body = (
      <EmptyState
        title={t("subscriptions.empty")}
        description={t("subscriptions.emptyDescription")}
        action={
          plansHref ? (
            <a
              href={plansHref}
              className={buttonVariants({ variant: "outline", size: "sm" })}
              onClick={(event) => {
                if (!navigate) return
                event.preventDefault()
                navigate(plansHref)
              }}
            >
              {t("subscriptions.browsePlans")}
            </a>
          ) : null
        }
      />
    )
  } else {
    body = (
      <ul className="-my-1 divide-y divide-border">
        {rows.map((s) => {
          const live = isLive(s)
          const pending = state.pending[s.id]
          const name = subscriptionName(s, m)
          const price = s.price
            ? formatMoney(s.price.unit_amount, s.price.currency, scales, locale)
            : null
          const interval = intervalLabel(s.price, m)
          const periodEnd = formatDate(s.current_period_ends_at, locale)
          const endedAt = formatDate(
            s.ended_at ?? s.cancelled_at ?? s.current_period_ends_at,
            locale
          )
          const scheduled = isEnding(s)
          const renews = live && !scheduled && s.price?.auto_renew !== false
          const when = !live
            ? endedAt && t("subscriptions.endedOn", { date: endedAt })
            : periodEnd &&
              (renews
                ? t("subscriptions.renews", { date: periodEnd })
                : t("subscriptions.endsOn", { date: periodEnd }))
          const paidWith =
            s.rail === "solana"
              ? t("subscriptions.wallet")
              : s.card?.last4
                ? t("subscriptions.paidWith", {
                    brand: brandName(
                      s.card.brand,
                      t("paymentMethods.fallbackBrand")
                    ),
                    last4: s.card.last4,
                  })
                : null
          const portal = live && !scheduled ? s.cancel_portal_url : null
          const canCancel =
            LIVE_STATUSES.has(s.status) &&
            !scheduled &&
            !portal &&
            (s.rail !== "solana" || !!sendSolanaTransaction)
          const canChangeCard =
            (s.status === "active" || s.status === "past_due") &&
            !scheduled &&
            !portal &&
            s.rail !== "solana" &&
            !!s.payment_method_id
          const status = scheduled ? "cancel_scheduled" : s.status
          const footer = renderSubscriptionFooter?.(s)
          return (
            <li
              key={s.id}
              data-testid="subscription-row"
              data-subscription-id={s.id}
              className="grid gap-3 py-4 @md:grid-cols-[1fr_auto] @md:items-center"
            >
              <div className="grid min-w-0 gap-1">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span
                    className={cn(
                      "truncate text-sm font-semibold",
                      !live && "text-muted-foreground"
                    )}
                  >
                    {name}
                  </span>
                  <BillingStatusBadge status={status} />
                </div>
                <p className="text-sm text-muted-foreground tabular-nums">
                  {[when, paidWith].filter(Boolean).join(" · ") ||
                    t("common.notAvailable")}
                </p>
                {price ? (
                  <p
                    className={cn(
                      "text-sm tabular-nums",
                      !live && "text-muted-foreground"
                    )}
                  >
                    <span className="font-medium">{price}</span>
                    {interval ? (
                      <span className="text-muted-foreground"> {interval}</span>
                    ) : null}
                  </p>
                ) : null}
                {s.status === "past_due" ? (
                  <p className="text-sm text-destructive">
                    {t("subscriptions.pastDue")}
                  </p>
                ) : null}
                {rowError?.id === s.id ? (
                  <p role="alert" className="text-sm text-destructive">
                    {rowError.message}
                  </p>
                ) : null}
              </div>
              <div className="flex flex-wrap gap-2 @md:justify-end">
                {s.resumable ? (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={!!pending}
                    onClick={() => void resume(s)}
                  >
                    {pending === "resume" ? <Spinner /> : null}
                    {pending === "resume"
                      ? t("subscriptions.updating")
                      : t("subscriptions.resume")}
                  </Button>
                ) : null}
                {portal ? (
                  <a
                    href={portal}
                    target="_blank"
                    rel="noopener noreferrer"
                    className={buttonVariants({
                      variant: "outline",
                      size: "sm",
                    })}
                  >
                    {t("subscriptions.manageAtProvider")}
                    <HugeiconsIcon icon={ArrowUpRight01Icon} aria-hidden />
                  </a>
                ) : null}
                {canChangeCard ? (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={!!pending}
                    aria-label={t("subscriptions.changeCardLabel", { name })}
                    onClick={() => {
                      setRowError(null)
                      setChangingCard(s)
                    }}
                  >
                    {pending === "payment_method" ? <Spinner /> : null}
                    {t("subscriptions.changeCard")}
                  </Button>
                ) : null}
                {canCancel ? (
                  <Button
                    variant="ghost"
                    size="sm"
                    className="text-muted-foreground hover:text-destructive"
                    disabled={!!pending}
                    aria-label={t("subscriptions.cancelLabel", { name })}
                    onClick={() => {
                      setRowError(null)
                      setCancelling(s)
                    }}
                  >
                    {pending &&
                    pending !== "resume" &&
                    pending !== "payment_method" ? (
                      <Spinner />
                    ) : null}
                    {t("subscriptions.cancel")}
                  </Button>
                ) : null}
              </div>
              {footer ? (
                <div
                  data-testid="subscription-footer"
                  className="border-t border-border/60 pt-3 @md:col-span-2"
                >
                  {footer}
                </div>
              ) : null}
            </li>
          )
        })}
      </ul>
    )
  }

  return (
    <Section
      title={t("subscriptions.title")}
      description={t("subscriptions.description")}
      appearance={appearance}
      className={className}
      data-testid="subscriptions-panel"
    >
      {body}
      {state.subscriptions !== null && state.error ? (
        <div className="pt-3">
          <ErrorState error={state.error} onRetry={state.refetch} />
        </div>
      ) : null}
      {notice}
      <ChangeCardDialog
        key={`card-${changingCard?.id ?? "closed"}`}
        subscription={changingCard}
        name={changingCard ? subscriptionName(changingCard, m) : ""}
        onOpenChange={(open) => !open && setChangingCard(null)}
        appearance={appearance}
        pending={
          !!changingCard && state.pending[changingCard.id] === "payment_method"
        }
        onConfirm={async (paymentMethodId) => {
          if (!changingCard) return null
          const error = await state.setPaymentMethod(
            changingCard.id,
            paymentMethodId
          )
          if (!error) {
            setChangingCard(null)
            announce(t("changeCard.done"))
          }
          return error
        }}
      />
      <CancelSubscriptionDialog
        key={cancelling?.id ?? "closed"}
        open={cancelling !== null}
        onOpenChange={(open) => !open && setCancelling(null)}
        name={cancelling ? subscriptionName(cancelling, m) : ""}
        onChain={onChain}
        appearance={appearance}
        pending={cancelling ? state.pending[cancelling.id] : undefined}
        onConfirm={async (feedback) => {
          if (!cancelling) return null
          const error =
            onChain && sendSolanaTransaction
              ? await state.cancelOnChain(cancelling.id, sendSolanaTransaction)
              : await state.cancel(cancelling.id, feedback)
          if (!error) announce(t("cancel.done"))
          return error
        }}
      />
    </Section>
  )
}
