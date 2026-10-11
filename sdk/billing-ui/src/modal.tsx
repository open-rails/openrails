// OrderModal — a host, not a flow: one Dialog around OrderCheckout. Closing
// is always possible; an accepted payment settles on the server whether or
// not the modal stays open, and the order stays the customer's to pay.

import { appearanceStyle, appearanceTheme } from "#orck/appearance"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "#orck/components/ui/dialog"
import { OrderCheckout, type OrderCheckoutProps } from "#orck/order-checkout"

export interface OrderModalProps extends OrderCheckoutProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function OrderModal({
  open,
  onOpenChange,
  ...checkout
}: OrderModalProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="orck max-h-[calc(100dvh-2rem)] w-[460px] max-w-[calc(100vw-2rem)] gap-0 overflow-hidden bg-background p-6 text-foreground ring-0 [&>[data-slot=dialog-close]]:top-5 [&>[data-slot=dialog-close]]:right-5 [&>[data-slot=dialog-close]]:text-foreground"
        data-orck-theme={appearanceTheme(checkout.appearance)}
        style={appearanceStyle(checkout.appearance)}
        showCloseButton
      >
        <DialogTitle className="sr-only">Checkout</DialogTitle>
        <DialogDescription className="sr-only">
          Complete your payment
        </DialogDescription>
        <div className="max-h-[calc(100dvh-5rem)] min-h-0 touch-pan-y [scrollbar-width:none] overflow-y-auto overscroll-contain pt-7 pr-6 [-webkit-overflow-scrolling:touch] [&::-webkit-scrollbar]:hidden">
          <OrderCheckout {...checkout} />
        </div>
      </DialogContent>
    </Dialog>
  )
}
