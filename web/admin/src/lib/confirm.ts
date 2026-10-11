// The console's promise-based confirm; ConfirmProvider renders its dialog.
import * as React from "react"

export interface ConfirmOptions {
  title: string
  description?: React.ReactNode
  confirmLabel?: string
  cancelLabel?: string
  tone?: "default" | "destructive"
}

export type Confirm = (options: ConfirmOptions) => Promise<boolean>

export const ConfirmContext = React.createContext<Confirm | null>(null)

export function useConfirm(): Confirm {
  const confirm = React.useContext(ConfirmContext)
  if (!confirm) throw new Error("useConfirm needs a ConfirmProvider above it")
  return confirm
}
