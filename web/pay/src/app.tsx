import { useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import {
  BillingProvider,
  PAYMENT_COMPLETE_MESSAGE,
  PAYMENT_RESIZE_MESSAGE,
  InvoicePayment,
  createBillingClient,
  type PaymentFallbacks,
} from "@openrails/billing-ui"
import { isBillingError, type CustomerConfig, type Invoice, type PublicConfig } from "@openrails/billing-ui/client"

import { paymentPageFetch } from "./api"
import { CaptchaPrompt, captchaFetch } from "./captcha"

type Ended = "not_found" | "expired" | "canceled" | "unavailable"

type State =
  | { kind: "loading" }
  | { kind: "ended"; reason: Ended }
  | { kind: "ready"; config: PublicConfig; customer: CustomerConfig; invoice: Invoice }

// Paid, or with the provider (Stripe redirects on a complete session even
// while an asynchronous payment settles): the customer goes back to the
// merchant, who fulfils on invoice.completed, never on this redirect.
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
export function App({ invoiceId, secret, fallbacks }: { invoiceId: string; secret: string | null; fallbacks?: PaymentFallbacks }) {
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
    // The deployment's configuration, and the invoice's merchant's: the secret
    // reads both, once, for the page and the payment form.
    Promise.all([client.getConfig(abort.signal), client.getCustomerConfig(abort.signal), client.getInvoice(invoiceId, abort.signal)]).then(
      ([loaded, customer, invoice]) => {
        config.current = loaded
        setState({ kind: "ready", config: loaded, customer, invoice })
      },
      (err: unknown) => {
        if (abort.signal.aborted) return
        const code = isBillingError(err) ? err.code : ""
        setState({ kind: "ended", reason: code === "pay_url_expired" ? "expired" : code === "pay_url_not_found" ? "not_found" : "unavailable" })
      }
    )
    return () => abort.abort()
  }, [client, invoiceId])

  const embedded = state.kind === "ready" && framed && state.invoice.embedded
  // The frame sizes itself to the page, so the page never scrolls inside it.
  useEffect(() => {
    document.documentElement.classList.toggle("framed", embedded)
  }, [embedded])
  const origins = state.kind === "ready" ? state.customer.merchant.embed_origins : []
  // The invoice as it settled in the frame.
  const [finished, setFinished] = useState<Invoice>()
  const done = (invoice: Invoice) => {
    if (!invoice.pay_url_expires_at) return
    if (embedded) {
      setFinished(invoice)
      tellFrame(origins, { type: PAYMENT_COMPLETE_MESSAGE, invoice_id: invoice.id, success_url: invoice.success_url ?? null })
      return
    }
    // A customer's own pay URL returns nowhere: the page says it is paid.
    if (invoice.success_url) window.location.replace(invoice.success_url)
    else setFinished(invoice)
  }
  useEffect(() => {
    if (state.kind === "ready" && settled(state.invoice.status)) done(state.invoice)
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

  const { customer, invoice } = state
  const merchant = customer.merchant
  const leaving = !embedded && !!invoice.success_url
  return (
    <Shell
      merchant={merchant.display_name}
      logo={merchant.logo_url ?? undefined}
      back={embedded ? undefined : (invoice.cancel_url ?? undefined)}
    >
      {invoice.status === "canceled" || invoice.status === "expired" ? (
        <Notice reason={invoice.status === "expired" ? "expired" : "canceled"} />
      ) : settled(invoice.status) || finished ? (
        <p className="muted" role="status">
          {leaving
            ? `Returning you to ${merchant.display_name || "the merchant"}…`
            : (finished ?? invoice).status === "processing"
              ? "Your payment is processing."
              : "Payment complete."}
        </p>
      ) : (
        <BillingProvider client={client!} config={state.config} customerConfig={customer}>
          <InvoicePayment
            invoiceId={invoice.id}
            client={client!}
            savedPaymentMethods={invoice.saved_payment_methods}
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
    canceled: "This invoice was canceled.",
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
