import * as React from "react"

import type { BillingAppearance } from "#orck/appearance"
import { cn } from "cn"

import type { BillingError } from "#orck/client/errors"
import type { PaymentMethod, Subscription } from "#orck/client/types"
import { CardBrandPlate } from "#orck/components/card-brands"
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
import { usePaymentMethods } from "#orck/react/hooks"
import { useCustomerConfig } from "#orck/react/config"
import { canSavePaymentMethod, routedPsps } from "#orck/psp"
import { SavePaymentMethod } from "#orck/save-payment-method"
import { useScopeProps } from "#orck/scope-context"
import { cardText, RESET } from "./format"
import { ErrorState, ListSkeleton } from "./section"

/** The choice that makes the subscription follow the default card. */
const FOLLOW_DEFAULT = "default"

/** Cards the subscription's PSP can charge; expired cards are left out. */
function eligibleCards(
  s: Subscription,
  methods: readonly PaymentMethod[]
): PaymentMethod[] {
  return methods.filter(
    (m) =>
      m.health?.active !== false &&
      m.health?.expiry_status !== "expired" &&
      (!s.psp_id || !m.psp_id || m.psp_id === s.psp_id) &&
      (!s.rail || !m.rail || m.rail === s.rail)
  )
}

export interface ChangeCardDialogProps {
  subscription: Subscription | null
  name: string
  onOpenChange: (open: boolean) => void
  pending: boolean
  /**
   * The subscription's own card, or null to follow the default card. Resolves
   * null on success; the dialog then closes.
   */
  onConfirm: (paymentMethodId: string | null) => Promise<BillingError | null>
  mode?: "change" | "resume"
  description?: string
  appearance?: BillingAppearance
}

export function ChangeCardDialog({
  subscription,
  name,
  onOpenChange,
  pending,
  onConfirm,
  mode = "change",
  description,
  appearance,
}: ChangeCardDialogProps) {
  const scope = useScopeProps(appearance)
  return (
    <Dialog
      open={subscription !== null}
      onOpenChange={(next) => {
        if (!next && pending) return
        onOpenChange(next)
      }}
    >
      <DialogContent
        showCloseButton={false}
        className={`${scope.className} ${RESET} max-h-[calc(100dvh-2rem)] overflow-y-auto bg-popover text-popover-foreground`}
        data-orck-theme={scope["data-orck-theme"]}
        style={scope.style}
      >
        {subscription ? (
          <ChangeCardForm
            subscription={subscription}
            name={name}
            pending={pending}
            onCancel={() => onOpenChange(false)}
            onConfirm={onConfirm}
            mode={mode}
            description={description}
            appearance={appearance}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

// Mounted only while open, so the card list loads on demand.
function ChangeCardForm({
  subscription,
  name,
  pending,
  onCancel,
  onConfirm,
  mode,
  description,
  appearance,
}: {
  subscription: Subscription
  name: string
  pending: boolean
  onCancel: () => void
  onConfirm: (paymentMethodId: string | null) => Promise<BillingError | null>
  mode: "change" | "resume"
  description?: string
  appearance?: BillingAppearance
}) {
  const m = useMessages()
  const { t } = m
  const idPrefix = React.useId()
  const methods = usePaymentMethods()
  const current = subscription.payment_method_id ?? FOLLOW_DEFAULT
  const [selected, setSelected] = React.useState(current)
  const [error, setError] = React.useState<BillingError | null>(null)
  const [adding, setAdding] = React.useState(false)
  const [submitting, setSubmitting] = React.useState(false)
  const initialized = React.useRef(false)
  const busy = pending || submitting
  const cards = React.useMemo(
    () =>
      methods.methods ? eligibleCards(subscription, methods.methods) : null,
    [methods.methods, subscription]
  )
  React.useEffect(() => {
    if (mode !== "resume" || cards === null || initialized.current) return
    initialized.current = true
    setSelected(
      cards.some((card) => card.id === current) ? current : (cards[0]?.id ?? "")
    )
    setAdding(cards.length === 0)
  }, [cards, current, mode])
  const currency = subscription.price?.currency?.toUpperCase()
  const defaultCard = currency
    ? cards?.find((c) => (c.default_currencies ?? []).includes(currency))
    : undefined
  const choices = [
    ...(mode === "change" && defaultCard
      ? [{ id: FOLLOW_DEFAULT, card: defaultCard }]
      : []),
    ...(cards ?? []).map((card) => ({ id: card.id, card })),
  ]
  const canSwitch = choices.some((c) => c.id !== current)
  const selectable = choices.some((c) => c.id === selected)

  const confirm = async () => {
    if (!selectable || (mode === "change" && selected === current) || busy)
      return
    setError(null)
    setSubmitting(true)
    try {
      const result = await onConfirm(
        selected === FOLLOW_DEFAULT ? null : selected
      )
      if (result) setError(result)
    } finally {
      setSubmitting(false)
    }
  }

  let list: React.ReactNode
  if (cards === null && methods.error) {
    list = <ErrorState error={methods.error} onRetry={methods.refetch} />
  } else if (cards === null) {
    list = <ListSkeleton rows={2} />
  } else if (mode === "resume") {
    list = adding ? (
      <ResumeCardEntry
        subscription={subscription}
        appearance={appearance}
        onSaved={(id) => {
          setSelected(id)
          setAdding(false)
          methods.refetch()
        }}
      />
    ) : (
      <label className="grid gap-2 text-sm">
        {t("changeCard.title", { name })}
        <select
          aria-label={t("changeCard.title", { name })}
          disabled={busy}
          value={selectable ? selected : ""}
          onChange={(event) => setSelected(event.target.value)}
          className="h-10 rounded-md border border-input bg-background px-3"
        >
          {!selectable ? (
            <option value="" disabled>
              {t("resume.chooseCard")}
            </option>
          ) : null}
          {choices.map(({ id, card }) => (
            <option key={id} value={id}>
              {cardText(card.card, m)}
            </option>
          ))}
        </select>
      </label>
    )
  } else {
    list = (
      <RadioGroup
        value={selected}
        onValueChange={(next) => {
          if (typeof next === "string" && next) setSelected(next)
        }}
        disabled={busy}
        aria-label={t("changeCard.title", { name })}
        className="grid gap-0"
      >
        {choices.map(({ id: value, card }) => {
          const id = `${idPrefix}-${value}`
          const label = cardText(card.card, m)
          return (
            <Label
              key={value}
              htmlFor={id}
              data-testid="change-card-option"
              className={cn(
                "flex cursor-pointer items-center gap-3 py-1.5 select-none",
                busy && "cursor-default"
              )}
            >
              <RadioGroupItem id={id} value={value} />
              <CardBrandPlate
                brand={card.card?.brand ?? undefined}
                fallback={(card.card?.brand ?? "card").slice(0, 4)}
              />
              <span className="min-w-0 flex-1 truncate text-sm font-medium tabular-nums">
                {value === FOLLOW_DEFAULT
                  ? t("changeCard.useDefault", { card: label })
                  : label}
              </span>
              {value === current ? (
                <span className="shrink-0 text-xs text-muted-foreground">
                  {t("changeCard.current")}
                </span>
              ) : null}
            </Label>
          )
        })}
      </RadioGroup>
    )
  }

  return (
    <div className="grid gap-5">
      <DialogHeader>
        <DialogTitle>
          {t(mode === "resume" ? "resume.title" : "changeCard.title", { name })}
        </DialogTitle>
        <DialogDescription>
          {description ??
            (cards !== null && !canSwitch
              ? t("changeCard.none")
              : t("changeCard.description"))}
        </DialogDescription>
      </DialogHeader>
      {list}
      {cards !== null && methods.error ? (
        <ErrorState error={methods.error} onRetry={methods.refetch} />
      ) : null}
      {mode === "resume" &&
      !adding &&
      cards !== null &&
      !selectable &&
      !methods.loading &&
      !methods.error ? (
        <p role="status" className="text-sm text-muted-foreground">
          {t("resume.savedCardUnavailable")}
        </p>
      ) : null}
      {mode === "resume" && cards !== null ? (
        <Button
          type="button"
          variant="link"
          disabled={busy || (adding && cards.length === 0)}
          onClick={() => setAdding(!adding)}
        >
          {adding ? t("changeCard.title", { name }) : t("paymentMethods.add")}
        </Button>
      ) : null}
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {m.error(error)}
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
        {!adding ? (
          <Button
            type="button"
            onClick={() => void confirm()}
            disabled={
              busy || !selectable || (mode === "change" && selected === current)
            }
          >
            {busy ? <Spinner /> : null}
            {t(mode === "resume" ? "resume.confirm" : "changeCard.confirm")}
          </Button>
        ) : null}
      </DialogFooter>
    </div>
  )
}

function ResumeCardEntry({
  subscription,
  appearance,
  onSaved,
}: {
  subscription: Subscription
  appearance?: BillingAppearance
  onSaved: (id: string) => void
}) {
  const { t } = useMessages()
  const config = useCustomerConfig()
  const available = routedPsps(config.config?.payment.psps ?? []).filter(
    (psp) =>
      canSavePaymentMethod(psp) &&
      (!subscription.psp_id || psp.psp_id === subscription.psp_id) &&
      (!subscription.rail || psp.rail === subscription.rail)
  )
  const [selected, select] = React.useState<string>()
  const psp = available.find((item) => item.psp_id === selected) ?? available[0]
  if (config.error)
    return <ErrorState error={config.error} onRetry={config.refetch} />
  if (config.loading) return <ListSkeleton rows={2} />
  if (!psp)
    return (
      <p role="status" className="text-sm text-muted-foreground">
        {t("paymentMethods.addUnavailable")}
      </p>
    )
  return (
    <div className="grid gap-4">
      {available.length > 1 ? (
        <label className="grid gap-2 text-sm">
          {t("paymentMethods.provider")}
          <select
            value={psp.psp_id}
            onChange={(event) => select(event.target.value)}
          >
            {available.map((item) => (
              <option key={item.psp_id} value={item.psp_id}>
                {item.display_name}
              </option>
            ))}
          </select>
        </label>
      ) : null}
      <SavePaymentMethod
        key={psp.psp_id}
        psp={psp}
        appearance={appearance}
        onSaved={onSaved}
      />
    </div>
  )
}
