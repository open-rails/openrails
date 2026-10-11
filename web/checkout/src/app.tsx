import { useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { BillingProvider, OrderCheckout, createBillingClient } from "@openrails/billing-ui"
import { isBillingError, type Order, type PublicConfig } from "@openrails/billing-ui/client"

import { checkoutFetch } from "./api"
import { CaptchaPrompt, captchaFetch } from "./captcha"

type Ended = "not_found" | "expired" | "canceled" | "unavailable"

type State = { kind: "loading" } | { kind: "ended"; reason: Ended } | { kind: "ready"; config: PublicConfig; order: Order }

// Paid, or with the provider (Stripe redirects on a complete session even
// while an asynchronous payment settles): the customer goes back to the
// merchant, who fulfils on order.completed, never on this redirect.
const settled = (status: string) => status === "complete" || status === "processing"

export function App({ orderId, secret }: { orderId: string; secret: string | null }) {
  const config = useRef<PublicConfig | null>(null)
  // billing-ui's client: every call carries the secret, and a pay the server
  // challenges is resent with the customer's captcha token.
  const client = useMemo(
    () => (secret ? createBillingClient({ baseUrl: "/v1", fetch: captchaFetch(checkoutFetch(secret), () => config.current?.captcha) }) : null),
    [secret]
  )
  const [state, setState] = useState<State>(() => (secret ? { kind: "loading" } : { kind: "ended", reason: "not_found" }))

  useEffect(() => {
    if (!client) return
    const abort = new AbortController()
    Promise.all([client.getConfig(abort.signal), client.getOrder(orderId, abort.signal)]).then(
      ([loaded, order]) => {
        config.current = loaded
        setState({ kind: "ready", config: loaded, order })
      },
      (err: unknown) => {
        if (abort.signal.aborted) return
        const code = isBillingError(err) ? err.code : ""
        setState({ kind: "ended", reason: code === "checkout_expired" ? "expired" : code === "checkout_not_found" ? "not_found" : "unavailable" })
      }
    )
    return () => abort.abort()
  }, [client, orderId])

  const done = (order: Order) => {
    if (order.checkout) window.location.replace(order.checkout.success_url)
  }
  useEffect(() => {
    if (state.kind === "ready" && settled(state.order.status)) done(state.order)
  }, [state])

  if (state.kind === "loading") return <Shell><p className="muted">Loading…</p></Shell>
  if (state.kind === "ended") return <Shell><Notice reason={state.reason} /></Shell>

  const { config: loaded, order } = state
  const merchant = loaded.merchant
  return (
    <Shell
      merchant={merchant?.display_name}
      logo={merchant?.logo_url ?? undefined}
      back={order.checkout?.cancel_url ?? undefined}
      support={merchant?.support_url ?? undefined}
    >
      {order.status === "canceled" || order.status === "expired" ? (
        <Notice reason={order.status === "expired" ? "expired" : "canceled"} />
      ) : settled(order.status) ? (
        <p className="muted">Returning you to {merchant?.display_name || "the merchant"}…</p>
      ) : (
        <BillingProvider client={client!}>
          <OrderCheckout
            orderId={order.id}
            client={client!}
            savedPaymentMethods={order.checkout?.saved_payment_methods ?? false}
            serverCardEntry={false}
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
    not_found: "This checkout link is not valid.",
    expired: "This checkout has expired. Return to the store to start again.",
    canceled: "This order was canceled.",
    unavailable: "Checkout is unavailable right now. Try again in a moment.",
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
  support,
  children,
}: {
  merchant?: string
  logo?: string
  back?: string
  support?: string
  children: ReactNode
}) {
  return (
    <main className="checkout">
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
        {support && (
          <a href={support} rel="noreferrer">
            Support
          </a>
        )}
        <span>Powered by OpenRails</span>
      </footer>
    </main>
  )
}
