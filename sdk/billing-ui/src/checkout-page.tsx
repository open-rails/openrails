// CheckoutPage — the shared payment page. A host serves it from one HTML entry
// at Config.Checkout.PageURL behind the adapter's CheckoutFramePolicy.
// The session id arrives in the URL fragment; the page reads and pays the
// session with it and, when framed by the app that minted it, speaks the
// frame protocol with that app only.
import * as React from "react"

import type { CheckoutAppearance } from "#orck/appearance"
import { Checkout, type CheckoutLayout } from "#orck/checkout"
import { createBillingClient, type BillingClient } from "#orck/client/client"
import { TerminalView } from "#orck/components/states"
import {
  pageMessage,
  parseAppMessage,
  type CheckoutFrameTheme,
  type PageMessage,
} from "#orck/frame"
import { BillingUiRoot } from "#orck/scope"
import type { CheckoutSource } from "#orck/source"
import type { PayResult } from "#orck/types"

export interface CheckoutPageProps {
  /** OpenRails mount on this host. Default "/billing/v1". */
  baseUrl?: string
  /** Replaces the client built from `baseUrl`. */
  client?: BillingClient
  /**
   * Pay as the signed-in customer on this customer surface (its `/me`
   * prefix) instead of with the session id alone; see `checkoutSource`.
   */
  customerBase?: string
  /** This host's branding; the framing app chooses the theme. */
  appearance?: CheckoutAppearance
  layout?: CheckoutLayout
  defaultCountry?: string
  className?: string
}

const sessionIdFromHash = () => window.location.hash.replace(/^#/, "").trim()

export function CheckoutPage({
  baseUrl,
  client,
  customerBase,
  appearance,
  layout = "auto",
  defaultCountry,
  className,
}: CheckoutPageProps) {
  const [sessionId, setSessionId] = React.useState(sessionIdFromHash)
  React.useEffect(() => {
    const onHash = () => setSessionId(sessionIdFromHash())
    window.addEventListener("hashchange", onHash)
    return () => window.removeEventListener("hashchange", onHash)
  }, [])

  const billing = React.useMemo(
    () => client ?? createBillingClient({ baseUrl }),
    [client, baseUrl]
  )
  const framed = window.parent !== window
  // The app that minted the session, as the server recorded it.
  const [appOrigin, setAppOrigin] = React.useState<string>()
  const [theme, setTheme] = React.useState<CheckoutFrameTheme>()

  const source = React.useMemo<CheckoutSource | null>(() => {
    if (!sessionId) return null
    const inner = billing.checkoutSource(sessionId, { customerBase })
    return {
      async getSession() {
        const session = await inner.getSession()
        if (session.embed_origin) setAppOrigin(session.embed_origin)
        return session
      },
      pay: inner.pay,
    }
  }, [billing, sessionId, customerBase])

  const post = React.useCallback(
    (message: PageMessage) => {
      if (framed && appOrigin) window.parent.postMessage(message, appOrigin)
    },
    [framed, appOrigin]
  )

  React.useEffect(() => {
    if (!framed || !appOrigin) return
    const onMessage = (event: MessageEvent) => {
      if (event.source !== window.parent || event.origin !== appOrigin) return
      const message = parseAppMessage(event.data)
      if (message) setTheme(message.theme)
    }
    window.addEventListener("message", onMessage)
    window.parent.postMessage(pageMessage({ type: "ready" }), appOrigin)
    return () => window.removeEventListener("message", onMessage)
  }, [framed, appOrigin])

  const root = React.useRef<HTMLDivElement>(null)
  React.useEffect(() => {
    const element = root.current
    if (!element || !framed || !appOrigin) return
    const report = () =>
      post(
        pageMessage({
          type: "resize",
          height: Math.ceil(element.getBoundingClientRect().height),
        })
      )
    const observer = new ResizeObserver(report)
    observer.observe(element)
    report()
    return () => observer.disconnect()
  }, [framed, appOrigin, post])

  const onComplete = React.useCallback(
    (result: PayResult) => {
      if (result.status === "succeeded")
        post(pageMessage({ type: "complete", status: result.status }))
    },
    [post]
  )
  // A nested frame may not navigate the top window: its app does.
  const onRedirect = React.useCallback(
    (url: string) => post(pageMessage({ type: "redirect", url })),
    [post]
  )

  const pageAppearance: CheckoutAppearance = {
    ...appearance,
    theme: theme ?? appearance?.theme,
  }
  return (
    <div ref={root} className={className}>
      {source ? (
        <Checkout
          source={source}
          appearance={pageAppearance}
          layout={layout}
          defaultCountry={defaultCountry}
          completionMode="embedded"
          onComplete={onComplete}
          onRedirect={framed && appOrigin ? onRedirect : undefined}
        />
      ) : (
        <BillingUiRoot appearance={pageAppearance}>
          <TerminalView
            merchantName=""
            headline="This checkout link is incomplete"
            sub="Start checkout again from the site you were buying on."
          />
        </BillingUiRoot>
      )}
    </div>
  )
}
