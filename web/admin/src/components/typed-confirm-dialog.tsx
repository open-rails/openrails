import * as React from "react"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { DIALOG_CONFIRM } from "@/lib/dialog-width"

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
          <Label htmlFor="typed-confirm">
            Type <span className="font-semibold">{confirmationWord}</span> to
            confirm
          </Label>
          <Input
            id="typed-confirm"
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
            Keep it
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
