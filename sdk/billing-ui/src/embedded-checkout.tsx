// An order's checkout framed on the merchant's own page, as Stripe's
// embedded Checkout: the order's checkout URL (ui_mode embedded) in an
// iframe the checkout host allows only on the merchant's registered origins.
// The framed page reports its height and the order's completion, and only
// messages from the checkout URL's origin and this iframe are heard. Fulfil
// on order.completed, never on the completion message.
import * as React from "react"

/** The framed page's message once the order completes. */
export const CHECKOUT_COMPLETE_MESSAGE = "openrails.checkout.complete"
/** The framed page's message when its height changes. */
export const CHECKOUT_RESIZE_MESSAGE = "openrails.checkout.resize"

export interface EmbeddedCheckoutCompletion {
  orderId: string
  /**
   * The checkout's success_url, with the order id filled in; "" for a
   * customer's own checkout, which returns nowhere (the frame says it is paid).
   */
  successUrl: string
}

export interface EmbeddedCheckoutProps {
  /** The order's checkout.url, secret and all. */
  url: string
  /**
   * Called once the order completes. Without it the page goes to the
   * checkout's success_url, if it has one.
   */
  onComplete?: (completion: EmbeddedCheckoutCompletion) => void
  title?: string
  className?: string
}

export function EmbeddedCheckout({
  url,
  onComplete,
  title = "Checkout",
  className,
}: EmbeddedCheckoutProps) {
  const frame = React.useRef<HTMLIFrameElement>(null)
  const [height, setHeight] = React.useState<number>()
  const completed = React.useRef(onComplete)
  React.useEffect(() => {
    completed.current = onComplete
  })
  const origin = React.useMemo(() => {
    try {
      const parsed = new URL(url)
      return parsed.protocol === "https:" || parsed.protocol === "http:"
        ? parsed.origin
        : null
    } catch {
      return null
    }
  }, [url])

  React.useEffect(() => {
    if (!origin) return
    let done = false
    const heard = (event: MessageEvent) => {
      if (
        event.origin !== origin ||
        !frame.current ||
        event.source !== frame.current.contentWindow
      )
        return
      const data: unknown = event.data
      if (!data || typeof data !== "object") return
      const message = data as Record<string, unknown>
      if (
        message.type === CHECKOUT_RESIZE_MESSAGE &&
        typeof message.height === "number" &&
        message.height > 0 &&
        message.height < 20_000
      ) {
        setHeight(Math.ceil(message.height))
        return
      }
      if (
        message.type !== CHECKOUT_COMPLETE_MESSAGE ||
        typeof message.order_id !== "string" ||
        done
      )
        return
      done = true
      const successUrl =
        typeof message.success_url === "string" ? message.success_url : ""
      if (completed.current)
        completed.current({ orderId: message.order_id, successUrl })
      else if (/^https?:\/\//i.test(successUrl))
        window.location.assign(successUrl)
    }
    window.addEventListener("message", heard)
    return () => window.removeEventListener("message", heard)
  }, [origin])

  if (!origin) return null
  return (
    <iframe
      ref={frame}
      src={url}
      title={title}
      className={className}
      allow="payment *; publickey-credentials-get *; publickey-credentials-create *"
      style={{ border: 0, width: "100%", height: height ?? 640 }}
    />
  )
}
