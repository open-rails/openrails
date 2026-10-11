import { useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import {
  BillingProvider,
  CHECKOUT_COMPLETE_MESSAGE,
  CHECKOUT_RESIZE_MESSAGE,
  OrderCheckout,
  createBillingClient,
  type BillingClient,
} from "@openrails/billing-ui"
import { isBillingError, type Order, type PublicConfig } from "@openrails/billing-ui/client"

import { checkoutFetch } from "./api"
import { CaptchaPrompt, captchaFetch } from "./captcha"
import { beginSignIn, finishSignIn, linkedHere, signInInPopup, type NetworkSignIn } from "./network"

type Ended = "not_found" | "expired" | "canceled" | "unavailable"

type State = { kind: "loading" } | { kind: "ended"; reason: Ended } | { kind: "ready"; config: PublicConfig; order: Order }

// Paid, or with the provider (Stripe redirects on a complete session even
// while an asynchronous payment settles): the customer goes back to the
// merchant, who fulfils on order.completed, never on this redirect.
const settled = (status: string) => status === "complete" || status === "processing"

// An embedded checkout runs in the merchant's frame; opened on its own it
// behaves as a hosted one.
const framed = window.parent !== window

// tellFrame posts to the page framing this one, only on the merchant's
// registered origins (never "*").
const tellFrame = (origins: string[], message: Record<string, unknown>) => {
  for (const origin of origins) window.parent.postMessage(message, origin)
}

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

  const embedded = state.kind === "ready" && framed && state.order.checkout?.ui_mode === "embedded"
  const origins = state.kind === "ready" ? (state.config.merchant?.embed_origins ?? []) : []
  // The order as it settled in the frame.
  const [finished, setFinished] = useState<Order>()
  const done = (order: Order) => {
    if (!order.checkout) return
    if (!embedded) {
      window.location.replace(order.checkout.success_url)
      return
    }
    setFinished(order)
    tellFrame(origins, { type: CHECKOUT_COMPLETE_MESSAGE, order_id: order.id, success_url: order.checkout.success_url })
  }
  useEffect(() => {
    if (state.kind === "ready" && settled(state.order.status)) done(state.order)
  }, [state])
  // The frame follows the page's height.
  useEffect(() => {
    if (!embedded) return
    const report = () => tellFrame(origins, { type: CHECKOUT_RESIZE_MESSAGE, height: document.documentElement.scrollHeight })
    const observer = new ResizeObserver(report)
    observer.observe(document.body)
    report()
    return () => observer.disconnect()
  }, [embedded, origins.join(" ")])

  if (state.kind === "loading") return <Shell><p className="muted">Loading…</p></Shell>
  if (state.kind === "ended") return <Shell><Notice reason={state.reason} /></Shell>

  const { config: loaded, order } = state
  const merchant = loaded.merchant
  return (
    <Shell
      merchant={merchant?.display_name}
      logo={merchant?.logo_url ?? undefined}
      back={embedded ? undefined : (order.checkout?.cancel_url ?? undefined)}
      support={merchant?.support_url ?? undefined}
    >
      {order.status === "canceled" || order.status === "expired" ? (
        <Notice reason={order.status === "expired" ? "expired" : "canceled"} />
      ) : settled(order.status) || finished ? (
        <p className="muted" role="status">
          {!embedded
            ? `Returning you to ${merchant?.display_name || "the merchant"}…`
            : (finished ?? order).status === "processing"
              ? "Your payment is processing."
              : "Payment complete."}
        </p>
      ) : (
        <BillingProvider client={client!}>
          {loaded.network ? <SaveWithNetwork network={loaded.network} client={client!} orderId={order.id} /> : null}
          <OrderCheckout
            orderId={order.id}
            client={client!}
            savedPaymentMethods={order.checkout?.saved_payment_methods ?? false}
            serverCardEntry={false}
            wallets
            onSettled={done}
          />
          <CaptchaPrompt />
        </BillingProvider>
      )}
    </Shell>
  )
}

// SaveWithNetwork offers, before the card, to link this purchase to the
// shopper's account on the customer network (decision: every checkout
// offers it prominently).
function SaveWithNetwork({ network, client, orderId }: { network: NetworkSignIn; client: BillingClient; orderId: string }) {
  const [state, setState] = useState<"idle" | "busy" | "linked">(() => (linkedHere(orderId) ? "linked" : "idle"))
  const [error, setError] = useState<string>()
  const name = useMemo(() => {
    try {
      return new URL(network.issuer).host
    } catch {
      return "your account"
    }
  }, [network.issuer])
  if (state === "linked")
    return (
      <p className="network" role="status">
        Saved to your {name} account.
      </p>
    )
  const save = async () => {
    setError(undefined)
    setState("busy")
    // A framed page cannot leave its frame: the sign-in opens in a popup,
    // opened by the click itself.
    const popup = framed ? window.open("about:blank", "openrails-sign-in", "popup,width=480,height=720") : null
    try {
      const url = await beginSignIn(network, client, orderId, framed)
      if (!framed) {
        window.location.assign(url)
        return
      }
      if (!popup) throw new Error("Allow pop-ups to sign in.")
      const { state: signIn, code } = await signInInPopup(popup, url)
      await finishSignIn(signIn, code, client)
      setState("linked")
    } catch (err) {
      popup?.close()
      setError(err instanceof Error && err.message ? err.message : "The sign-in did not finish.")
      setState("idle")
    }
  }
  return (
    <div className="network">
      <button type="button" onClick={() => void save()} disabled={state === "busy"}>
        Save with {name}
      </button>
      <span className="muted">Keep this purchase in your {name} account.</span>
      {error ? (
        <span role="alert" className="error">
          {error}
        </span>
      ) : null}
    </div>
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
