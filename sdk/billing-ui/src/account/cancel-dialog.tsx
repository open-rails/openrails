import * as React from "react"

import type { CheckoutAppearance } from "#orck/appearance"

import {
  CANCEL_REASON_MAX,
  CANCEL_REASON_MIN,
  isWalletRejection,
} from "#orck/client/client"
import type { BillingError } from "#orck/client/errors"
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
import { Label } from "#orck/components/ui/label"
import { Spinner } from "#orck/components/ui/spinner"
import { Textarea } from "#orck/components/ui/textarea"
import { useMessages } from "#orck/i18n/context"
import type { SubscriptionAction } from "#orck/react/hooks"
import { useScopeProps } from "#orck/scope-context"
import { RESET } from "./format"

// Key it per subscription so each opening starts blank.
export interface CancelSubscriptionDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Subscription display name. */
  name: string
  /** On-chain (Solana) cancel: the wallet signs after the reason. */
  onChain?: boolean
  /** In-flight stage, when cancelling. */
  pending?: SubscriptionAction
  /** Resolves null on success; the dialog then closes. */
  onConfirm: (reason: string) => Promise<BillingError | null>
  appearance?: CheckoutAppearance
}

export function CancelSubscriptionDialog({
  open,
  onOpenChange,
  name,
  onChain = false,
  pending,
  onConfirm,
  appearance,
}: CancelSubscriptionDialogProps) {
  const m = useMessages()
  const { t } = m
  const scope = useScopeProps(appearance)
  const reasonId = React.useId()
  const hintId = React.useId()
  const [reason, setReason] = React.useState("")
  const [touched, setTouched] = React.useState(false)
  const [error, setError] = React.useState<BillingError | null>(null)
  const busy = pending !== undefined

  const length = reason.trim().length
  const tooShort = length < CANCEL_REASON_MIN
  const confirm = async () => {
    setTouched(true)
    if (tooShort || busy) return
    setError(null)
    const result = await onConfirm(reason)
    if (!result) onOpenChange(false)
    else if (!isWalletRejection(result)) setError(result)
  }

  const stageLabel =
    pending === "cancel" && onChain
      ? t("cancel.preparing")
      : pending === "signing"
        ? t("cancel.signing")
        : pending === "confirming"
          ? t("cancel.confirming")
          : busy
            ? t("cancel.working")
            : onChain
              ? t("cancel.confirmWallet")
              : t("cancel.confirm")

  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        if (!next && busy) return
        onOpenChange(next)
      }}
    >
      <AlertDialogContent
        className={`${scope.className} ${RESET} bg-popover text-popover-foreground`}
        data-orck-theme={scope["data-orck-theme"]}
        style={scope.style}
      >
        <form
          className="grid gap-5"
          noValidate
          onSubmit={(event) => {
            event.preventDefault()
            void confirm()
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{t("cancel.title", { name })}</AlertDialogTitle>
            <AlertDialogDescription>
              {onChain
                ? t("cancel.solanaDescription")
                : t("cancel.description")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="grid gap-2">
            <Label htmlFor={reasonId}>{t("cancel.reasonLabel")}</Label>
            <Textarea
              id={reasonId}
              value={reason}
              maxLength={CANCEL_REASON_MAX}
              placeholder={t("cancel.reasonPlaceholder")}
              aria-describedby={hintId}
              aria-invalid={touched && tooShort ? true : undefined}
              disabled={busy}
              required
              rows={3}
              className="[font:inherit]"
              onChange={(event) => setReason(event.target.value)}
              onBlur={() => length > 0 && setTouched(true)}
            />
            <div
              id={hintId}
              className="flex justify-between gap-3 text-xs text-muted-foreground"
            >
              <span className={touched && tooShort ? "text-destructive" : ""}>
                {touched && tooShort
                  ? t("cancel.reasonTooShort", { min: CANCEL_REASON_MIN })
                  : ""}
              </span>
              <span className="tabular-nums">
                {t("cancel.reasonHint", {
                  count: reason.length,
                  max: CANCEL_REASON_MAX,
                })}
              </span>
            </div>
          </div>
          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {m.error(error)}
            </p>
          ) : null}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>
              {t("cancel.keep")}
            </AlertDialogCancel>
            <Button type="submit" variant="destructive" disabled={busy}>
              {busy ? <Spinner /> : null}
              {stageLabel}
            </Button>
          </AlertDialogFooter>
        </form>
      </AlertDialogContent>
    </AlertDialog>
  )
}
