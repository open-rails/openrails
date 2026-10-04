// CheckoutFrame — an app's view of the shared payment page. It frames the
// minted session's url, sizes itself to the page, gives it the app's theme and
// navigates the app for redirect rails. It exchanges messages with the page's
// origin only. `onComplete` is a hint: confirm access from your own API.
import * as React from "react"

import {
  appMessage,
  pageOrigin,
  parsePageMessage,
  type CheckoutFrameTheme,
} from "#orck/frame"
import { safeRedirectURL } from "#orck/lib/redirect"
import type { CheckoutSessionStatus } from "#orck/types"

export interface CheckoutFrameProps {
  /** The minted session's `url` (createCheckoutSession). */
  url: string
  onComplete?: (status: CheckoutSessionStatus) => void
  /** Default "auto": the buyer's system preference. */
  theme?: CheckoutFrameTheme
  /** Accessible name of the frame. */
  title?: string
  /** Height before the page reports its own, in pixels. Default 480. */
  minHeight?: number
  className?: string
  style?: React.CSSProperties
}

export function CheckoutFrame({
  url,
  onComplete,
  theme = "auto",
  title = "Secure checkout",
  minHeight = 480,
  className,
  style,
}: CheckoutFrameProps) {
  const frame = React.useRef<HTMLIFrameElement>(null)
  const origin = React.useMemo(() => pageOrigin(url), [url])
  const [height, setHeight] = React.useState<number>()
  const ready = React.useRef(false)
  const themeRef = React.useRef(theme)
  const onCompleteRef = React.useRef(onComplete)
  React.useEffect(() => {
    onCompleteRef.current = onComplete
  })

  React.useEffect(() => {
    ready.current = false
    if (!origin) return
    const onMessage = (event: MessageEvent) => {
      const page = frame.current?.contentWindow
      if (!page || event.source !== page || event.origin !== origin) return
      const message = parsePageMessage(event.data)
      if (!message) return
      switch (message.type) {
        case "ready":
          ready.current = true
          page.postMessage(
            appMessage({ type: "init", theme: themeRef.current }),
            origin
          )
          return
        case "resize":
          setHeight(Math.ceil(message.height))
          return
        case "complete":
          onCompleteRef.current?.(message.status)
          return
        case "redirect": {
          const href = safeRedirectURL(message.url)
          if (href) (window.top ?? window).location.href = href
          return
        }
      }
    }
    window.addEventListener("message", onMessage)
    return () => window.removeEventListener("message", onMessage)
  }, [origin, url])

  React.useEffect(() => {
    themeRef.current = theme
    const page = frame.current?.contentWindow
    if (ready.current && page && origin)
      page.postMessage(appMessage({ type: "init", theme }), origin)
  }, [theme, origin])

  if (!origin) return null
  return (
    <iframe
      ref={frame}
      src={url}
      title={title}
      allow="payment"
      className={className}
      style={{
        width: "100%",
        border: 0,
        display: "block",
        ...style,
        height: Math.max(minHeight, height ?? 0),
      }}
    />
  )
}
