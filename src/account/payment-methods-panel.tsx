import * as React from "react"
import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon, Delete02Icon } from "@hugeicons/core-free-icons"

import type { CheckoutAppearance } from "#orck/appearance"
import type { BillingError } from "#orck/client/errors"
import type { PaymentMethod } from "#orck/client/types"
import { CardBrandPlate } from "#orck/components/card-brands"
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "#orck/components/ui/alert-dialog"
import { Button } from "#orck/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "#orck/components/ui/dialog"
import { Spinner } from "#orck/components/ui/spinner"
import { useMessages } from "#orck/i18n/context"
import type { Translator } from "#orck/i18n/messages"
import { usePaymentMethods } from "#orck/react/hooks"
import { useScopeProps } from "#orck/scope-context"
import { canSavePaymentMethod, type PspConfig } from "#orck/psp"
import { SavePaymentMethod } from "#orck/save-payment-method"
import { brandName, expiry, RESET } from "./format"
import { EmptyState, ErrorState, ListSkeleton, Section } from "./section"
import { useNotice } from "./notice"
import { BillingStatusBadge } from "./status-badge"

export interface PaymentMethodsPanelProps {
  /**
   * OpenRails's browser PSP configs (checkout config). "Add card" offers
   * each one a card can be saved with in the page.
   */
  psps?: readonly PspConfig[]
  /** Return target after off-page card verification; see `SavePaymentMethod`. */
  cardSetupReturnURL?: (setupId: string) => string
  /** Enables "Make default" for this currency's invoice collection. */
  defaultCurrency?: string
  appearance?: CheckoutAppearance
  className?: string
}

function methodLabel(method: PaymentMethod, { t }: Translator): string {
  const brand = brandName(method.card?.brand, t("paymentMethods.fallbackBrand"))
  return method.card?.last4
    ? t("paymentMethods.cardLabel", { brand, last4: method.card.last4 })
    : brand
}

function removeMessage(error: BillingError, m: Translator): string {
  if (error.status === 409 && error.code === "resource_conflict")
    return m.t("paymentMethods.inUse")
  return m.error(error)
}

export function PaymentMethodsPanel({
  psps,
  cardSetupReturnURL,
  defaultCurrency,
  appearance,
  className,
}: PaymentMethodsPanelProps) {
  const m = useMessages()
  const { t } = m
  const scope = useScopeProps(appearance)
  const state = usePaymentMethods()
  const [removing, setRemoving] = React.useState<PaymentMethod | null>(null)
  const [removeError, setRemoveError] = React.useState<string | null>(null)
  const [rowError, setRowError] = React.useState<{
    id: string
    message: string
  } | null>(null)
  const [adding, setAdding] = React.useState(false)
  const savable = (psps ?? []).filter(canSavePaymentMethod)
  const [pspId, setPspId] = React.useState<string>()
  const psp = savable.find((item) => item.psp_id === pspId) ?? savable[0]
  const [notice, announce] = useNotice()
  const currency = defaultCurrency?.toUpperCase()

  const confirmRemove = async () => {
    if (!removing) return
    setRemoveError(null)
    const error = await state.remove(removing.id)
    if (error) {
      setRemoveError(removeMessage(error, m))
      return
    }
    setRemoving(null)
    announce(t("paymentMethods.removed"))
  }

  const makeDefault = async (method: PaymentMethod) => {
    if (!currency) return
    setRowError(null)
    const error = await state.setDefault(method.id, currency)
    if (error) setRowError({ id: method.id, message: m.error(error) })
    else announce(t("paymentMethods.madeDefault"))
  }

  let body: React.ReactNode
  const methods = state.methods
  if (methods === null && state.error) {
    body = <ErrorState error={state.error} onRetry={state.refetch} />
  } else if (methods === null) {
    body = <ListSkeleton />
  } else if (methods.length === 0) {
    body = (
      <EmptyState
        title={t("paymentMethods.empty")}
        description={t("paymentMethods.emptyDescription")}
      />
    )
  } else {
    body = (
      <ul className="-my-1 divide-y divide-border">
        {methods.map((method) => {
          const label = methodLabel(method, m)
          const expires = expiry(method.card)
          const users = (method.subscriptions ?? [])
            .map((s) => s.display_name)
            .filter(Boolean)
          const health = method.health?.expiry_status
          const defaults = method.collection_default_currencies ?? []
          const pending = state.pending[method.id]
          const meta = [
            expires && t("paymentMethods.expires", { date: expires }),
            users.length > 0 &&
              t("paymentMethods.usedBy", { names: users.join(", ") }),
          ].filter(Boolean)
          return (
            <li
              key={method.id}
              data-testid="payment-method-row"
              data-payment-method-id={method.id}
              className="flex flex-wrap items-center gap-x-3 gap-y-2 py-3.5"
            >
              <CardBrandPlate
                brand={method.card?.brand ?? undefined}
                fallback={(method.card?.brand ?? "card").slice(0, 4)}
              />
              <div className="grid min-w-40 flex-1 gap-0.5">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span className="text-sm font-medium tabular-nums">
                    {label}
                  </span>
                  {health === "expired" || health === "expiring_soon" ? (
                    <BillingStatusBadge status={health} />
                  ) : method.health?.active === false ? (
                    <BillingStatusBadge status="needs_attention" />
                  ) : null}
                  {defaults.length > 0 ? (
                    <BillingStatusBadge
                      status="default"
                      label={t("paymentMethods.defaultFor", {
                        currencies: defaults.join(", "),
                      })}
                    />
                  ) : null}
                </div>
                {meta.length > 0 ? (
                  <p className="text-sm text-muted-foreground tabular-nums">
                    {meta.join(" · ")}
                  </p>
                ) : null}
                {rowError?.id === method.id ? (
                  <p role="alert" className="text-sm text-destructive">
                    {rowError.message}
                  </p>
                ) : null}
              </div>
              <div className="ml-auto flex items-center gap-1">
                {currency && !defaults.includes(currency) ? (
                  <Button
                    variant="ghost"
                    size="sm"
                    className="text-muted-foreground"
                    disabled={!!pending}
                    onClick={() => void makeDefault(method)}
                  >
                    {pending === "default" ? <Spinner /> : null}
                    {t("paymentMethods.makeDefault")}
                  </Button>
                ) : null}
                <Button
                  variant="ghost"
                  size="icon-sm"
                  className="text-muted-foreground hover:text-destructive"
                  aria-label={t("paymentMethods.removeLabel", { label })}
                  title={t("paymentMethods.remove")}
                  disabled={!!pending}
                  onClick={() => {
                    setRemoveError(null)
                    setRemoving(method)
                  }}
                >
                  {pending === "remove" ? (
                    <Spinner />
                  ) : (
                    <HugeiconsIcon icon={Delete02Icon} aria-hidden />
                  )}
                </Button>
              </div>
            </li>
          )
        })}
      </ul>
    )
  }

  const removingBusy = removing
    ? state.pending[removing.id] === "remove"
    : false
  const removingLabel = removing ? methodLabel(removing, m) : ""

  return (
    <Section
      title={t("paymentMethods.title")}
      description={t("paymentMethods.description")}
      appearance={appearance}
      className={className}
      data-testid="payment-methods-panel"
      action={
        psp ? (
          <Button variant="outline" size="sm" onClick={() => setAdding(true)}>
            <HugeiconsIcon
              icon={Add01Icon}
              aria-hidden
              data-icon="inline-start"
            />
            {t("paymentMethods.add")}
          </Button>
        ) : null
      }
    >
      {body}
      {methods !== null && state.error ? (
        <div className="pt-3">
          <ErrorState error={state.error} onRetry={state.refetch} />
        </div>
      ) : null}
      {notice}

      <AlertDialog
        open={removing !== null}
        onOpenChange={(open) => {
          if (!open && !removingBusy) setRemoving(null)
        }}
      >
        <AlertDialogContent
          className={`${scope.className} ${RESET} bg-popover text-popover-foreground`}
          data-orck-theme={scope["data-orck-theme"]}
          style={scope.style}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("paymentMethods.removeTitle", { label: removingLabel })}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t("paymentMethods.removeDescription")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {removeError ? (
            <p role="alert" className="text-sm text-destructive">
              {removeError}
            </p>
          ) : null}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={removingBusy}>
              {t("common.cancel")}
            </AlertDialogCancel>
            <Button
              variant="destructive"
              disabled={removingBusy}
              onClick={() => void confirmRemove()}
            >
              {removingBusy ? <Spinner /> : null}
              {t("paymentMethods.removeConfirm")}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {psp ? (
        <Dialog open={adding} onOpenChange={setAdding}>
          <DialogContent
            className={`${scope.className} ${RESET} max-h-[calc(100dvh-2rem)] overflow-y-auto bg-popover text-popover-foreground`}
            data-orck-theme={scope["data-orck-theme"]}
            style={scope.style}
          >
            <DialogHeader>
              <DialogTitle>{t("paymentMethods.addTitle")}</DialogTitle>
              <DialogDescription>
                {t("paymentMethods.addDescription")}
              </DialogDescription>
            </DialogHeader>
            {savable.length > 1 ? (
              <label className="grid gap-1.5 text-sm">
                {t("paymentMethods.provider")}
                <select
                  className="h-9 rounded-md border border-input bg-transparent px-2"
                  value={psp.psp_id}
                  onChange={(event) => setPspId(event.target.value)}
                >
                  {savable.map((item) => (
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
              returnURL={cardSetupReturnURL}
              appearance={appearance}
              onSaved={() => {
                setAdding(false)
                announce(t("paymentMethods.added"))
              }}
            />
          </DialogContent>
        </Dialog>
      ) : null}
    </Section>
  )
}
