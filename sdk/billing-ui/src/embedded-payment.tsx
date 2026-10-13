// An order's pay URL framed on the merchant's own page, as Stripe's
// embedded Checkout: the order's pay URL (embedded) in an
// iframe the payment origin allows only on the merchant's registered origins.
// The framed page reports its height and the order's completion, and only
// messages from the pay URL's origin and this iframe are heard. Fulfil
// on order.completed, never on the completion message. Fallbacks reach the
// framed page in the URL fragment, never through the server.
import * as React from "react"

import { withPaymentFallbacks, type PaymentFallbacks } from "./fallbacks"

/** The framed page's message once the order completes. */
export const PAYMENT_COMPLETE_MESSAGE = "openrails.payment.complete"
/** The framed page's message when its height changes. */
export const PAYMENT_RESIZE_MESSAGE = "openrails.payment.resize"

export interface EmbeddedPaymentCompletion {
  orderId: string
  /**
   * The order's success_url, with the order id filled in; "" for a
   * customer's own pay URL, which returns nowhere (the frame says it is paid).
   */
  successUrl: string
}

export interface EmbeddedPaymentProps {
  /** The order's pay_url, secret and all. */
  url: string
  /**
   * Called once the order completes. Without it the page goes to the
   * order's success_url, if it has one.
   */
  onComplete?: (completion: EmbeddedPaymentCompletion) => void
  /** As OrderPayment's: the framed page keeps these PSPs back the same way. */
  fallbacks?: PaymentFallbacks
  title?: string
  className?: string
}

export function EmbeddedPayment({
  url,
  onComplete,
  fallbacks,
  title = "Payment",
  className,
}: EmbeddedPaymentProps) {
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
        message.type === PAYMENT_RESIZE_MESSAGE &&
        typeof message.height === "number" &&
        message.height > 0 &&
        message.height < 20_000
      ) {
        setHeight(Math.ceil(message.height))
        return
      }
      if (
        message.type !== PAYMENT_COMPLETE_MESSAGE ||
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
      src={withPaymentFallbacks(url, fallbacks)}
      title={title}
      className={className}
      allow="payment *; publickey-credentials-get *; publickey-credentials-create *"
      style={{ border: 0, width: "100%", height: height ?? 640 }}
    />
  )
}
