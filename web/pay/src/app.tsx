import { useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import {
  BillingProvider,
  PAYMENT_COMPLETE_MESSAGE,
  PAYMENT_RESIZE_MESSAGE,
  OrderPayment,
  createBillingClient,
  type PaymentFallbacks,
} from "@openrails/billing-ui"
import { isBillingError, type Order, type PublicConfig, type PublicMerchant } from "@openrails/billing-ui/client"

import { paymentPageFetch } from "./api"
import { CaptchaPrompt, captchaFetch } from "./captcha"

type Ended = "not_found" | "expired" | "canceled" | "unavailable"

type State =
  | { kind: "loading" }
  | { kind: "ended"; reason: Ended }
  | { kind: "ready"; config: PublicConfig; merchant: PublicMerchant; order: Order }

// Paid, or with the provider (Stripe redirects on a complete session even
// while an asynchronous payment settles): the customer goes back to the
// merchant, who fulfils on order.completed, never on this redirect.
const settled = (status: string) => status === "complete" || status === "processing"

// An embedded payment page runs in the merchant's frame; opened on its own it
// behaves as a hosted one.
const framed = window.parent !== window

// tellFrame posts to the page framing this one, only on the merchant's
// registered origins (never "*").
const tellFrame = (origins: string[], message: Record<string, unknown>) => {
  for (const origin of origins) window.parent.postMessage(message, origin)
}

// fallbacks are the checkout's, from the pay URL's fragment: applied as on
// the merchant's own page.
export function App({ orderId, secret, fallbacks }: { orderId: string; secret: string | null; fallbacks?: PaymentFallbacks }) {
  const config = useRef<PublicConfig | null>(null)
  // billing-ui's client: every call carries the secret, and a pay the server
  // challenges is resent with the customer's captcha token.
  const client = useMemo(
    () => (secret ? createBillingClient({ baseUrl: "/v1", fetch: captchaFetch(paymentPageFetch(secret), () => config.current?.captcha) }) : null),
    [secret]
  )
  const [state, setState] = useState<State>(() => (secret ? { kind: "loading" } : { kind: "ended", reason: "not_found" }))

  useEffect(() => {
    if (!client) return
    const abort = new AbortController()
    // The deployment's configuration, and the order's merchant's: the secret
    // reads both.
    Promise.all([client.getConfig(abort.signal), client.getCustomerConfig(abort.signal), client.getOrder(orderId, abort.signal)]).then(
      ([loaded, customer, order]) => {
        config.current = loaded
        setState({ kind: "ready", config: loaded, merchant: customer.merchant, order })
      },
      (err: unknown) => {
        if (abort.signal.aborted) return
        const code = isBillingError(err) ? err.code : ""
        setState({ kind: "ended", reason: code === "pay_url_expired" ? "expired" : code === "pay_url_not_found" ? "not_found" : "unavailable" })
      }
    )
    return () => abort.abort()
  }, [client, orderId])

  const embedded = state.kind === "ready" && framed && state.order.embedded
  // The frame sizes itself to the page, so the page never scrolls inside it.
  useEffect(() => {
    document.documentElement.classList.toggle("framed", embedded)
  }, [embedded])
  const origins = state.kind === "ready" ? state.merchant.embed_origins : []
  // The order as it settled in the frame.
  const [finished, setFinished] = useState<Order>()
  const done = (order: Order) => {
    if (!order.pay_url_expires_at) return
    if (embedded) {
      setFinished(order)
      tellFrame(origins, { type: PAYMENT_COMPLETE_MESSAGE, order_id: order.id, success_url: order.success_url ?? null })
      return
    }
    // A customer's own pay URL returns nowhere: the page says it is paid.
    if (order.success_url) window.location.replace(order.success_url)
    else setFinished(order)
  }
  useEffect(() => {
    if (state.kind === "ready" && settled(state.order.status)) done(state.order)
  }, [state])
  // The frame follows the page's height.
  useEffect(() => {
    if (!embedded) return
    const report = () => tellFrame(origins, { type: PAYMENT_RESIZE_MESSAGE, height: document.documentElement.scrollHeight })
    const observer = new ResizeObserver(report)
    observer.observe(document.body)
    report()
    return () => observer.disconnect()
  }, [embedded, origins.join(" ")])

  if (state.kind === "loading") return <Shell><p className="muted">Loading…</p></Shell>
  if (state.kind === "ended") return <Shell><Notice reason={state.reason} /></Shell>

  const { merchant, order } = state
  const leaving = !embedded && !!order.success_url
  return (
    <Shell
      merchant={merchant.display_name}
      logo={merchant.logo_url ?? undefined}
      back={embedded ? undefined : (order.cancel_url ?? undefined)}
    >
      {order.status === "canceled" || order.status === "expired" ? (
        <Notice reason={order.status === "expired" ? "expired" : "canceled"} />
      ) : settled(order.status) || finished ? (
        <p className="muted" role="status">
          {leaving
            ? `Returning you to ${merchant.display_name || "the merchant"}…`
            : (finished ?? order).status === "processing"
              ? "Your payment is processing."
              : "Payment complete."}
        </p>
      ) : (
        <BillingProvider client={client!}>
          <OrderPayment
            orderId={order.id}
            client={client!}
            savedPaymentMethods={order.saved_payment_methods}
            serverCardEntry={false}
            wallets
            fallbacks={fallbacks}
            onSettled={done}
          />
          <CaptchaPrompt />
        </BillingProvider>
      )}
    </Shell>
  )
}

function Notice({ reason }: { reason: Ended }) {
  const text = {
    not_found: "This payment link is not valid.",
    expired: "This payment link has expired. Return to the store to start again.",
    canceled: "This order was canceled.",
    unavailable: "Payment is unavailable right now. Try again in a moment.",
  }[reason]
  return (
    <p className="notice" role="status">
      {text}
    </p>
  )
}

function Shell({
  merchant,
  logo,
  back,
  children,
}: {
  merchant?: string
  logo?: string
  back?: string
  children: ReactNode
}) {
  return (
    <main className="payment">
      <header>
        {back && (
          <a className="back" href={back} rel="noreferrer">
            ← Back
          </a>
        )}
        {logo && <img className="logo" src={logo} alt="" referrerPolicy="no-referrer" />}
        {merchant && <h1>{merchant}</h1>}
      </header>
      {children}
      <footer>
        <a href="https://openrails.dev" target="_blank" rel="noreferrer">
          Powered by OpenRails
        </a>
      </footer>
    </main>
  )
}
