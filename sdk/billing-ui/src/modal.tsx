// CheckoutModal — a host, not a flow. It supplies the one surface (the
// Dialog) and renders the same Checkout inside. Closing is always possible:
// an accepted payment settles on the server whether or not the modal stays
// open. A successful payment closes on the host's terms via onComplete.
import * as React from "react"

import { appearanceStyle, appearanceTheme } from "#orck/appearance"
import { Checkout, type CheckoutProps } from "#orck/checkout"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "#orck/components/ui/dialog"

export interface CheckoutModalProps extends CheckoutProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /**
   * Rendered instead of the checkout while set, e.g. the host's sign-in.
   * Clear it to continue to payment in the same modal.
   */
  gate?: React.ReactNode
}

export function CheckoutModal({
  open,
  onOpenChange,
  onPhaseChange,
  gate,
  ...checkoutProps
}: CheckoutModalProps) {
  const compact = checkoutProps.layout === "compact"
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className={`orck max-h-[calc(100dvh-2rem)] max-w-[calc(100vw-2rem)] gap-0 overflow-hidden bg-background text-foreground ring-0 [&>[data-slot=dialog-close]]:top-5 [&>[data-slot=dialog-close]]:right-5 [&>[data-slot=dialog-close]]:text-foreground ${compact ? "w-[420px] p-6" : "w-[860px] p-8 sm:max-w-[calc(100vw-2rem)]"}`}
        data-orck-theme={appearanceTheme(checkoutProps.appearance)}
        style={appearanceStyle(checkoutProps.appearance)}
        showCloseButton
      >
        <DialogTitle className="sr-only">Checkout</DialogTitle>
        <DialogDescription className="sr-only">
          Complete your payment
        </DialogDescription>
        <div
          className={`min-h-0 touch-pan-y [scrollbar-width:none] overflow-y-auto overscroll-contain pr-6 [-webkit-overflow-scrolling:touch] [&::-webkit-scrollbar]:hidden ${compact ? "max-h-[calc(100dvh-5rem)] pt-7" : "max-h-[calc(100dvh-6rem)]"}`}
        >
          {gate ?? (
            <Checkout {...checkoutProps} onPhaseChange={onPhaseChange} />
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
