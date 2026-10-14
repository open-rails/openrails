import type { BillingAppearance } from "#orck/appearance"
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
import { Spinner } from "#orck/components/ui/spinner"
import { useMessages } from "#orck/i18n/context"
import { useScopeProps } from "#orck/scope-context"
import { RESET } from "./format"

// Resuming re-arms renewals: the customer confirms they'll be charged again.
export function ResumeSubscriptionDialog({
  open,
  onOpenChange,
  name,
  price,
  date,
  busy,
  onConfirm,
  appearance,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  name: string
  /** The renewal amount, formatted. */
  price: string | null
  /** The next renewal date, formatted. */
  date: string | null
  busy: boolean
  onConfirm: () => void
  appearance?: BillingAppearance
}) {
  const m = useMessages()
  const t = m.t
  const scope = useScopeProps(appearance)
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
        <AlertDialogHeader>
          <AlertDialogTitle>{t("resume.title", { name })}</AlertDialogTitle>
          <AlertDialogDescription>
            {price && date
              ? t("resume.description", { price, date })
              : t("resume.descriptionNoDate")}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={busy}>{t("resume.keepCanceled")}</AlertDialogCancel>
          <Button type="button" disabled={busy} onClick={onConfirm}>
            {busy ? <Spinner /> : null}
            {t("resume.confirm")}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
