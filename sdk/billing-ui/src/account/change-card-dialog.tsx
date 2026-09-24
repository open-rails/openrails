import * as React from "react"

import type { CheckoutAppearance } from "#orck/appearance"
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
import { useScopeProps } from "#orck/scope-context"
import { cardText, RESET } from "./format"
import { ErrorState, ListSkeleton } from "./section"

/** Cards the subscription's PSP can charge; expired cards are left out. */
function eligibleCards(
  s: Subscription,
  methods: readonly PaymentMethod[]
): PaymentMethod[] {
  return methods.filter(
    (m) =>
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
  /** Resolves null on success; the dialog then closes. */
  onConfirm: (paymentMethodId: string) => Promise<BillingError | null>
  appearance?: CheckoutAppearance
}

export function ChangeCardDialog({
  subscription,
  name,
  onOpenChange,
  pending,
  onConfirm,
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
        className={`${scope.className} ${RESET} bg-popover text-popover-foreground`}
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
}: {
  subscription: Subscription
  name: string
  pending: boolean
  onCancel: () => void
  onConfirm: (paymentMethodId: string) => Promise<BillingError | null>
}) {
  const m = useMessages()
  const { t } = m
  const idPrefix = React.useId()
  const methods = usePaymentMethods()
  const current = subscription.payment_method_id ?? ""
  const [selected, setSelected] = React.useState(current)
  const [error, setError] = React.useState<BillingError | null>(null)
  const cards = methods.methods
    ? eligibleCards(subscription, methods.methods)
    : null
  const canSwitch = !!cards && cards.some((c) => c.id !== current)

  const confirm = async () => {
    if (!selected || selected === current || pending) return
    setError(null)
    const result = await onConfirm(selected)
    if (result) setError(result)
  }

  let list: React.ReactNode
  if (cards === null && methods.error) {
    list = <ErrorState error={methods.error} onRetry={methods.refetch} />
  } else if (cards === null) {
    list = <ListSkeleton rows={2} />
  } else {
    list = (
      <RadioGroup
        value={selected}
        onValueChange={(next) => {
          if (typeof next === "string" && next) setSelected(next)
        }}
        disabled={pending}
        aria-label={t("changeCard.title", { name })}
        className="grid gap-0"
      >
        {cards.map((card) => {
          const id = `${idPrefix}-${card.id}`
          return (
            <Label
              key={card.id}
              htmlFor={id}
              data-testid="change-card-option"
              className={cn(
                "flex cursor-pointer items-center gap-3 py-1.5 select-none",
                pending && "cursor-default"
              )}
            >
              <RadioGroupItem id={id} value={card.id} />
              <CardBrandPlate
                brand={card.card?.brand ?? undefined}
                fallback={(card.card?.brand ?? "card").slice(0, 4)}
              />
              <span className="min-w-0 flex-1 truncate text-sm font-medium tabular-nums">
                {cardText(card.card, m)}
              </span>
              {card.id === current ? (
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
    <form
      className="grid gap-5"
      noValidate
      onSubmit={(event) => {
        event.preventDefault()
        void confirm()
      }}
    >
      <DialogHeader>
        <DialogTitle>{t("changeCard.title", { name })}</DialogTitle>
        <DialogDescription>
          {cards !== null && !canSwitch
            ? t("changeCard.none")
            : t("changeCard.description")}
        </DialogDescription>
      </DialogHeader>
      {list}
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {m.error(error)}
        </p>
      ) : null}
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          disabled={pending}
          onClick={onCancel}
        >
          {t("common.cancel")}
        </Button>
        <Button
          type="submit"
          disabled={pending || !selected || selected === current}
        >
          {pending ? <Spinner /> : null}
          {t("changeCard.confirm")}
        </Button>
      </DialogFooter>
    </form>
  )
}
