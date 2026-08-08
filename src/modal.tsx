// CheckoutModal — a host, not a flow. It supplies the one surface (the
// Dialog) and renders the same Checkout inside. Closing is blocked while a
// payment is processing; a successful payment closes on the host's terms via
// onComplete.
import * as React from "react"

import { appearanceStyle, appearanceTheme } from "#orck/appearance"
import { Checkout, type CheckoutProps } from "#orck/checkout"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "#orck/components/ui/dialog"
import type { CheckoutPhase } from "#orck/types"

export interface CheckoutModalProps extends CheckoutProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function CheckoutModal({
  open,
  onOpenChange,
  onPhaseChange,
  ...checkoutProps
}: CheckoutModalProps) {
  const [phase, setPhase] = React.useState<CheckoutPhase>("loading")
  const processing = phase === "processing"
  const compact = checkoutProps.layout === "compact"
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next && processing) return
        onOpenChange(next)
      }}
    >
      <DialogContent
        className={`orck max-h-[calc(100dvh-2rem)] max-w-[calc(100vw-2rem)] gap-0 overflow-hidden bg-background text-foreground [&>[data-slot=dialog-close]]:top-5 [&>[data-slot=dialog-close]]:right-5 [&>[data-slot=dialog-close]]:text-foreground ${compact ? "w-[420px] p-6" : "w-[860px] p-8 sm:max-w-[calc(100vw-2rem)]"}`}
        data-orck-theme={appearanceTheme(checkoutProps.appearance)}
        style={appearanceStyle(checkoutProps.appearance)}
        showCloseButton={!processing}
      >
        <DialogTitle className="sr-only">Checkout</DialogTitle>
        <DialogDescription className="sr-only">
          Complete your payment
        </DialogDescription>
        <div
          className={`min-h-0 touch-pan-y [scrollbar-width:none] overflow-y-auto overscroll-contain pr-6 [-webkit-overflow-scrolling:touch] [&::-webkit-scrollbar]:hidden ${compact ? "max-h-[calc(100dvh-5rem)]" : "max-h-[calc(100dvh-6rem)]"}`}
        >
          <Checkout
            {...checkoutProps}
            onPhaseChange={(next) => {
              setPhase(next)
              onPhaseChange?.(next)
            }}
          />
        </div>
      </DialogContent>
    </Dialog>
  )
}
