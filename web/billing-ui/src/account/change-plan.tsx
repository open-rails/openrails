import * as React from "react"
import { cn } from "cn"

import type { BillingAppearance } from "#orck/appearance"
import { authenticateChange } from "#orck/authenticate"
import type { SendSolanaTransaction } from "#orck/client/client"
import { isBillingError, type BillingError } from "#orck/client/errors"
import type {
  CurrencyScales,
  Subscription,
  SubscriptionChangeOption,
} from "#orck/client/types"
import { Button } from "#orck/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "#orck/components/ui/dialog"
import { Label } from "#orck/components/ui/label"
import { RadioGroup, RadioGroupItem } from "#orck/components/ui/radio-group"
import { Spinner } from "#orck/components/ui/spinner"
import { useMessages } from "#orck/i18n/context"
import type { Translator } from "#orck/i18n/messages"
import { useCurrencyScales, useCustomerConfig } from "#orck/react/config"
import { useBillingContext } from "#orck/react/context"
import {
  useChangeOptions,
  type ChangeOptionsState,
  type SubscriptionAction,
  type SubscriptionsState,
} from "#orck/react/hooks"
import { useScopeProps, useUiSettings } from "#orck/scope-context"
import { formatDate, formatMoney, intervalLabel, RESET } from "./format"
import { ErrorState, ListSkeleton } from "./section"

// A plan change is a money write: its Idempotency-Key is reused until the
// change has a definitive answer, so retrying a lost answer replays it.
const keys = new Map<string, string>()
const slot = (subscriptionId: string, priceId: string) =>
  JSON.stringify([subscriptionId, priceId])

function changeKey(subscriptionId: string, priceId: string): string {
  const id = slot(subscriptionId, priceId)
  const key = keys.get(id) ?? crypto.randomUUID()
  keys.set(id, key)
  return key
}

// The change may have landed: the next attempt must replay the same key.
const uncertain = (error: BillingError) =>
  error.code === "subscription_change_in_flight" ||
  error.status === 0 ||
  error.status >= 500

export interface ChangePlanProps {
  subscription: Subscription
  name: string
  pending: SubscriptionAction | undefined
  changeSubscription: SubscriptionsState["changeSubscription"]
  sendSolanaTransaction?: SendSolanaTransaction
  /** Announces the outcome once the dialog closes. */
  onChanged: (text: string) => void
  appearance?: BillingAppearance
}

/** The row's Change plan button and dialog, when another plan is open to it. */
export function ChangePlan({
  subscription,
  name,
  pending,
  changeSubscription,
  sendSolanaTransaction,
  onChanged,
  appearance,
}: ChangePlanProps) {
  const m = useMessages()
  const { t } = m
  const { locale } = useUiSettings()
  const { client, notify } = useBillingContext()
  const { config: merchant } = useCustomerConfig()
  const options = useChangeOptions(subscription.id)
  const [open, setOpen] = React.useState(false)
  const [working, setWorking] = React.useState(false)

  if (!open && !options.options?.length) return null

  const confirm = async (
    option: SubscriptionChangeOption
  ): Promise<string | null> => {
    const id = subscription.id
    const priceId = option.price.id
    let result = await changeSubscription(
      id,
      { priceId, idempotencyKey: changeKey(id, priceId) },
      sendSolanaTransaction
    )
    if (isBillingError(result)) {
      if (!uncertain(result)) keys.delete(slot(id, priceId))
      return m.error(result)
    }
    const next = result.next_action
    if (next?.type === "redirect_to_url" && next.url) {
      window.location.assign(next.url)
      return null
    }
    if (result.status === "requires_action" && next?.type === "authenticate") {
      const psp = merchant?.payment.psps.find((p) => p.psp_id === next.psp_id)
      if (!psp) return m.t("paymentMethods.unavailable")
      try {
        result = await authenticateChange(client, id, result, psp)
      } catch (err) {
        if (isBillingError(err)) return m.error(err)
        return (err instanceof Error && err.message) || m.t("errors.generic")
      }
      notify({
        type: "subscription.changed",
        subscriptionId: id,
        change: result,
      })
    }
    if (result.status === "requires_action")
      return m.t("changePlan.actionRequired")
    if (result.status !== "processing") keys.delete(slot(id, priceId))
    if (result.status === "blocked")
      return result.message || m.t("errors.generic")
    setOpen(false)
    const product = option.product.display_name || name
    const date = formatDate(
      result.delayed_start ?? result.next_charge_date,
      locale
    )
    onChanged(
      result.status === "processing"
        ? t("changePlan.processing")
        : result.effective === "period_end" && date
          ? t("changePlan.scheduled", { name: product, date })
          : t("changePlan.done", { name: product })
    )
    return null
  }

  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        disabled={!!pending}
        aria-label={t("subscriptions.changePlanLabel", { name })}
        onClick={() => {
          options.refetch()
          setOpen(true)
        }}
      >
        {t("subscriptions.changePlan")}
      </Button>
      <ChangePlanDialog
        open={open}
        onOpenChange={setOpen}
        name={name}
        options={options}
        busy={working || !!pending}
        stage={pending}
        onConfirm={async (option) => {
          setWorking(true)
          try {
            return await confirm(option)
          } finally {
            setWorking(false)
          }
        }}
        appearance={appearance}
      />
    </>
  )
}

function ChangePlanDialog({
  open,
  onOpenChange,
  appearance,
  busy,
  ...form
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  appearance?: BillingAppearance
} & ChangePlanFormProps) {
  const scope = useScopeProps(appearance)
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next && busy) return
        onOpenChange(next)
      }}
    >
      <DialogContent
        showCloseButton={false}
        className={`${scope.className} ${RESET} bg-popover text-popover-foreground`}
        data-orck-theme={scope["data-orck-theme"]}
        style={scope.style}
      >
        {open ? (
          <ChangePlanForm
            {...form}
            busy={busy}
            onCancel={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

interface ChangePlanFormProps {
  name: string
  options: ChangeOptionsState
  busy: boolean
  stage: SubscriptionAction | undefined
  /** Resolves null on success, else the message to show. */
  onConfirm: (option: SubscriptionChangeOption) => Promise<string | null>
  onCancel?: () => void
}

function ChangePlanForm({
  name,
  options,
  busy,
  stage,
  onConfirm,
  onCancel,
}: ChangePlanFormProps) {
  const m = useMessages()
  const { t } = m
  const { locale } = useUiSettings()
  const scales = useCurrencyScales()
  const idPrefix = React.useId()
  const [selected, setSelected] = React.useState("")
  const [error, setError] = React.useState<string | null>(null)
  const list = options.options
  const chosen = list?.find((o) => o.price.id === selected)

  let body: React.ReactNode
  if (list === null && options.error) {
    body = <ErrorState error={options.error} onRetry={options.refetch} />
  } else if (list === null) {
    body = <ListSkeleton rows={2} />
  } else {
    body = (
      <RadioGroup
        value={selected}
        onValueChange={(next) => {
          if (typeof next === "string" && next) setSelected(next)
        }}
        disabled={busy}
        aria-label={t("changePlan.title", { name })}
        className="grid gap-0"
      >
        {list.map((option) => {
          const id = `${idPrefix}-${option.price.id}`
          const price = formatMoney(
            option.price.unit_amount,
            option.price.currency,
            scales,
            locale
          )
          const interval = intervalLabel(option.price, m)
          return (
            <Label
              key={option.price.id}
              htmlFor={id}
              data-testid="change-plan-option"
              className={cn(
                "flex cursor-pointer items-start gap-3 py-2 select-none",
                busy && "cursor-default"
              )}
            >
              <RadioGroupItem
                id={id}
                value={option.price.id}
                className="mt-0.5"
              />
              <span className="grid min-w-0 flex-1 gap-0.5">
                <span className="text-sm font-medium">
                  {option.product.display_name}
                  {price ? (
                    <span className="font-normal text-muted-foreground tabular-nums">
                      {" · "}
                      {price}
                      {interval ? ` ${interval}` : null}
                    </span>
                  ) : null}
                </span>
                <span className="text-xs font-normal text-muted-foreground tabular-nums">
                  {optionTerms(option, m, scales, locale)}
                </span>
              </span>
            </Label>
          )
        })}
      </RadioGroup>
    )
  }

  return (
    <form
      className="grid gap-5"
      noValidate
      onSubmit={(event) => {
        event.preventDefault()
        if (!chosen || busy) return
        setError(null)
        void onConfirm(chosen).then(setError)
      }}
    >
      <DialogHeader>
        <DialogTitle>{t("changePlan.title", { name })}</DialogTitle>
        <DialogDescription>
          {list?.length === 0
            ? t("changePlan.none")
            : t("changePlan.description")}
        </DialogDescription>
      </DialogHeader>
      {body}
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          disabled={busy}
          onClick={onCancel}
        >
          {t("common.cancel")}
        </Button>
        <Button type="submit" disabled={busy || !chosen}>
          {busy ? <Spinner /> : null}
          {stage === "signing"
            ? t("changePlan.signing")
            : stage === "confirming"
              ? t("changePlan.confirming")
              : t("changePlan.confirm")}
        </Button>
      </DialogFooter>
    </form>
  )
}

/** What the change charges and when, from its preview. */
function optionTerms(
  option: SubscriptionChangeOption,
  m: Translator,
  scales: CurrencyScales,
  locale?: string
): string {
  const p = option.preview
  const next = formatMoney(p.next_charge_amount, p.currency, scales, locale)
  const date = formatDate(p.next_charge_date, locale)
  if (!next || !date) return p.message ?? ""
  if (p.effective !== "now")
    return m.t("changePlan.atPeriodEnd", { next, date })
  const amount = formatMoney(p.amount_due_now, p.currency, scales, locale)
  if (!amount) return p.message ?? ""
  return m.t(
    p.is_estimate ? "changePlan.chargeNowEstimate" : "changePlan.chargeNow",
    {
      amount,
      next,
      date,
    }
  )
}
