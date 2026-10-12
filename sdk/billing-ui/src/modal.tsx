// The payment dialogs — hosts, not flows: one Dialog around OrderCheckout, or
// around the order's embedded checkout on the checkout host. Closing is always
// possible; an accepted payment settles on the server whether or not the
// dialog stays open, and the order stays the customer's to pay.
import type { ReactNode } from "react"

import {
  appearanceStyle,
  appearanceTheme,
  type CheckoutAppearance,
} from "#orck/appearance"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "#orck/components/ui/dialog"
import {
  EmbeddedCheckout,
  type EmbeddedCheckoutProps,
} from "#orck/embedded-checkout"
import { OrderCheckout, type OrderCheckoutProps } from "#orck/order-checkout"

interface DialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

function PayDialog({
  open,
  onOpenChange,
  appearance,
  children,
}: DialogProps & { appearance?: CheckoutAppearance; children: ReactNode }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="orck max-h-[calc(100dvh-2rem)] w-[460px] max-w-[calc(100vw-2rem)] gap-0 overflow-hidden bg-background p-6 text-foreground ring-0 [&>[data-slot=dialog-close]]:top-5 [&>[data-slot=dialog-close]]:right-5 [&>[data-slot=dialog-close]]:text-foreground"
        data-orck-theme={appearanceTheme(appearance)}
        style={appearanceStyle(appearance)}
        showCloseButton
      >
        <DialogTitle className="sr-only">Checkout</DialogTitle>
        <DialogDescription className="sr-only">
          Complete your payment
        </DialogDescription>
        <div className="max-h-[calc(100dvh-5rem)] min-h-0 touch-pan-y [scrollbar-width:none] overflow-y-auto overscroll-contain pt-7 pr-6 [-webkit-overflow-scrolling:touch] [&::-webkit-scrollbar]:hidden">
          {children}
        </div>
      </DialogContent>
    </Dialog>
  )
}

export interface OrderModalProps extends OrderCheckoutProps, DialogProps {}

export function OrderModal({
  open,
  onOpenChange,
  ...checkout
}: OrderModalProps) {
  return (
    <PayDialog
      open={open}
      onOpenChange={onOpenChange}
      appearance={checkout.appearance}
    >
      <OrderCheckout {...checkout} />
    </PayDialog>
  )
}

export interface EmbeddedModalProps extends EmbeddedCheckoutProps, DialogProps {
  appearance?: CheckoutAppearance
}

export function EmbeddedModal({
  open,
  onOpenChange,
  appearance,
  ...checkout
}: EmbeddedModalProps) {
  return (
    <PayDialog open={open} onOpenChange={onOpenChange} appearance={appearance}>
      <EmbeddedCheckout {...checkout} />
    </PayDialog>
  )
}
