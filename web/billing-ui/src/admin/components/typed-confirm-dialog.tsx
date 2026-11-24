import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import * as React from "react"

import { Button } from "#orck/admin/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "#orck/admin/components/ui/dialog"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import { DIALOG_CONFIRM } from "#orck/admin/lib/dialog-width"

// TypedConfirmDialog requires typing a confirmation phrase before a
// destructive action (e.g. cancelling a subscription) can proceed.
export function TypedConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmationWord,
  actionLabel,
  onConfirm,
  children,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description: string
  confirmationWord: string
  actionLabel: string
  onConfirm: () => Promise<void> | void
  children?: React.ReactNode
}) {
  const { id: adminId } = useAdminUI()

  const [typed, setTyped] = React.useState("")
  const [busy, setBusy] = React.useState(false)

  const handleOpenChange = (next: boolean) => {
    if (!next) setTyped("")
    onOpenChange(next)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className={DIALOG_CONFIRM}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>
        {children}
        <div className="grid gap-2">
          <Label htmlFor={adminId("typed-confirm")}>
            <AdminMessage id={"Type "} />
            <span className="font-semibold">{confirmationWord}</span>
            <AdminMessage id={" to confirm"} />
          </Label>
          <Input
            id={adminId("typed-confirm")}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            autoComplete="off"
          />
        </div>
        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => handleOpenChange(false)}
            disabled={busy}
          >
            <AdminMessage id={"Keep it"} />
          </Button>
          <Button
            variant="destructive"
            disabled={typed !== confirmationWord || busy}
            onClick={async () => {
              setBusy(true)
              try {
                await onConfirm()
                handleOpenChange(false)
              } finally {
                setBusy(false)
              }
            }}
          >
            {busy ? "Working…" : actionLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
