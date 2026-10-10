import { useEffect, useMemo, useState, type ReactNode } from "react"
import { formatAmount } from "@openrails/billing-ui"

import { CheckoutError, checkoutFetch, getJSON, type Order, type PublicConfig } from "./api"

type State =
  | { kind: "loading" }
  | { kind: "ended"; reason: "not_found" | "expired" | "canceled" | "unavailable" }
  | { kind: "ready"; config: PublicConfig; order: Order }

// Paid, or with the provider (Stripe redirects on a complete session even
// while an asynchronous payment settles): the customer goes back to the
// merchant, who fulfils on order.completed, never on this redirect.
const settled = (status: string) => status === "complete" || status === "processing"

export function App({ orderId, secret }: { orderId: string; secret: string | null }) {
  const send = useMemo(() => (secret ? checkoutFetch(secret) : null), [secret])
  const [state, setState] = useState<State>(() => (secret ? { kind: "loading" } : { kind: "ended", reason: "not_found" }))

  useEffect(() => {
    if (!send) return
    const abort = new AbortController()
    Promise.all([
      getJSON<PublicConfig>(send, "/config", abort.signal),
      getJSON<Order>(send, `/me/orders/${orderId}`, abort.signal),
    ]).then(
      ([config, order]) => setState({ kind: "ready", config, order }),
      (err: unknown) => {
        if (abort.signal.aborted) return
        const code = err instanceof CheckoutError ? err.code : ""
        setState({ kind: "ended", reason: code === "checkout_expired" ? "expired" : code === "checkout_not_found" ? "not_found" : "unavailable" })
      }
    )
    return () => abort.abort()
  }, [send, orderId])

  useEffect(() => {
    if (state.kind === "ready" && settled(state.order.status) && state.order.checkout) {
      window.location.replace(state.order.checkout.success_url)
    }
  }, [state])

  if (state.kind === "loading") return <Shell><p className="muted">Loading…</p></Shell>
  if (state.kind === "ended") return <Shell><Ended reason={state.reason} /></Shell>

  const { config, order } = state
  const merchant = config.merchant
  const decimals = config.currencies.find((c) => c.code === order.currency)?.decimals ?? 2
  const money = (amount: string) => formatAmount(amount, order.currency, decimals)
  const ended = order.status === "canceled" || order.status === "expired"
  return (
    <Shell
      merchant={merchant?.display_name}
      logo={merchant?.logo_url ?? undefined}
      back={order.checkout?.cancel_url ?? undefined}
      support={merchant?.support_url ?? undefined}
    >
      <section className="summary" aria-label="Order summary">
        <ul>
          {order.lines.map((line) => (
            <li key={line.id}>
              <span>
                {line.description}
                {line.quantity && line.quantity > 1 ? ` × ${line.quantity}` : ""}
              </span>
              <span>{money(line.amount)}</span>
            </li>
          ))}
        </ul>
        <p className="total">
          <span>Total</span>
          <span>{money(order.total)}</span>
        </p>
      </section>
      {ended ? (
        <Ended reason={order.status === "expired" ? "expired" : "canceled"} />
      ) : settled(order.status) ? (
        <p className="muted">Returning you to {merchant?.display_name || "the merchant"}…</p>
      ) : (
        <section className="payment" aria-label="Payment" data-order={order.id} />
      )}
    </Shell>
  )
}

function Ended({ reason }: { reason: "not_found" | "expired" | "canceled" | "unavailable" }) {
  const text = {
    not_found: "This checkout link is not valid.",
    expired: "This checkout has expired. Return to the store to start again.",
    canceled: "This order was canceled.",
    unavailable: "Checkout is unavailable right now. Try again in a moment.",
  }[reason]
  return <p className="notice" role="status">{text}</p>
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
