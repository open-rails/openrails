// Shows the customer's cart, priced live, removes lines and checks it out:
// one order made from the cart (cart: true, at the total shown), then paid
// here with OrderCheckout unless the host takes the order itself. Hosts add
// and change lines with client.setCartLine.
import * as React from "react"

import {
  appearanceStyle,
  appearanceTheme,
  type CheckoutAppearance,
} from "./appearance"
import type { BillingClient } from "./client/client"
import type { Cart as CartData, Order } from "./client/types"
import { PayButton } from "./components/pay-button"
import { LoadingSkeleton } from "./components/states"
import { useMessages } from "./i18n/context"
import { formatAmount } from "./lib/money"
import { OrderCheckout } from "./order-checkout"
import { useCurrencyScales } from "./react/config"
import { useBillingContext } from "./react/context"
import { cn } from "cn"

export interface CartProps {
  /** The client to read and buy through; default: the provider's. */
  client?: BillingClient
  /**
   * The order made from the cart. Default: pay it here with OrderCheckout.
   */
  onOrder?: (order: Order) => void
  /** Passed to OrderCheckout: the order is complete, or processing. */
  onSettled?: (order: Order) => void
  appearance?: CheckoutAppearance
  className?: string
}

/** The customer's cart. Requires `BillingProvider`. */
export function Cart({
  client: given,
  onOrder,
  onSettled,
  appearance,
  className,
}: CartProps) {
  const m = useMessages()
  const context = useBillingContext()
  const client = given ?? context.client
  const scales = useCurrencyScales()
  const [cart, setCart] = React.useState<CartData>()
  const [order, setOrder] = React.useState<Order>()
  const [error, setError] = React.useState<string>()
  const [busy, setBusy] = React.useState(false)
  // One key per checkout attempt: a retry answers the same order.
  const [key, setKey] = React.useState(() => crypto.randomUUID())

  React.useEffect(() => {
    const abort = new AbortController()
    client.getCart(abort.signal).then(setCart, (cause: unknown) => {
      if (!abort.signal.aborted) setError(m.error(cause))
    })
    return () => abort.abort()
  }, [client, m])

  const act = async (work: () => Promise<void>) => {
    if (busy) return
    setBusy(true)
    setError(undefined)
    try {
      await work()
    } catch (cause) {
      setError(m.error(cause))
    } finally {
      setBusy(false)
    }
  }

  if (order && !onOrder)
    return (
      <OrderCheckout
        orderId={order.id}
        client={client}
        onSettled={onSettled}
        appearance={appearance}
        className={className}
      />
    )
  if (!cart)
    return error ? (
      <p role="alert" className="text-sm text-destructive">
        {error}
      </p>
    ) : (
      <LoadingSkeleton />
    )

  const decimals = scales[cart.currency.toUpperCase()] ?? 2
  const money = (amount: string) =>
    formatAmount(amount, cart.currency, decimals)
  const buyable =
    cart.lines.length > 0 && cart.lines.every((line) => !line.refusal)

  const remove = (priceId: string) =>
    act(async () => {
      setCart(await client.setCartLine(priceId, { quantity: 0 }))
      setKey(crypto.randomUUID())
    })

  const checkout = () =>
    act(async () => {
      const result = await client.createOrder({
        cart: true,
        expectedTotal: cart.total,
        idempotencyKey: key,
      })
      if (result.error) throw result.error
      setOrder(result.order)
      onOrder?.(result.order)
    })

  return (
    <div
      className={cn("orck grid gap-5", className)}
      data-orck-theme={appearanceTheme(appearance)}
      style={appearanceStyle(appearance)}
    >
      {cart.lines.length === 0 ? (
        <p role="status" className="text-sm text-muted-foreground">
          Your cart is empty.
        </p>
      ) : (
        <div className="grid">
          {cart.lines.map((line) => (
            <div
              key={line.price_id}
              className="grid gap-1 border-b border-border py-2 text-[13.5px]"
            >
              <div className="flex items-start justify-between gap-4">
                <span>{line.description || line.price_id}</span>
                <span className="whitespace-nowrap tabular-nums">
                  {money(line.amount)}
                </span>
              </div>
              <div className="flex items-center gap-2">
                {line.quantity != null && line.quantity > 1 ? (
                  <span className="text-[13px] text-muted-foreground">
                    Quantity {line.quantity}
                  </span>
                ) : null}
                <button
                  type="button"
                  className="text-[13px] text-muted-foreground underline"
                  disabled={busy}
                  onClick={() => void remove(line.price_id)}
                >
                  Remove
                </button>
              </div>
              {line.refusal ? (
                <p className="text-[13px] text-destructive">
                  {line.refusal.message}
                </p>
              ) : null}
            </div>
          ))}
          <div className="flex items-start justify-between gap-4 pt-3 text-[13.5px] font-semibold">
            <span>Total</span>
            <span className="whitespace-nowrap tabular-nums">
              {money(cart.total)}
            </span>
          </div>
        </div>
      )}
      {cart.lines.length > 0 ? (
        <form
          className="grid gap-2"
          onSubmit={(event) => {
            event.preventDefault()
            void checkout()
          }}
        >
          <PayButton label="Check out" processing={busy} disabled={!buyable} />
          <button
            type="button"
            className="text-[13px] text-muted-foreground underline"
            disabled={busy}
            onClick={() =>
              void act(async () => {
                await client.clearCart()
                setCart(await client.getCart())
              })
            }
          >
            Empty cart
          </button>
        </form>
      ) : null}
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
    </div>
  )
}
