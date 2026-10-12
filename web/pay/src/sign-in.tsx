import { useEffect, useState } from "react"
import { createBillingClient } from "@openrails/billing-ui"

import { paymentPageFetch } from "./api"
import { SIGN_IN_MESSAGE, finishSignIn, pendingOrder, popupState } from "./network"
import { takeSecret } from "./secret"

// The network sign-in's return (/pay/sign-in): a popup hands the code to the
// framed page that opened it; a return in this tab links the order's
// customer, then goes back to the order.
// The return is handled once per load, StrictMode's second effect run
// included.
let handled = false

export function SignInReturn() {
  const [message, setMessage] = useState("Signing you in…")
  const [order, setOrder] = useState<string | null>(null)
  useEffect(() => {
    if (handled) return
    handled = true
    const params = new URLSearchParams(window.location.search)
    const state = params.get("state") ?? ""
    const code = params.get("code")
    if (popupState(state)) {
      window.opener?.postMessage({ type: SIGN_IN_MESSAGE, state, code, error: params.get("error") }, window.location.origin)
      window.close()
      setMessage("You can close this window.")
      return
    }
    const pending = pendingOrder(state)
    const secret = pending ? takeSecret(pending) : null
    if (!pending || !secret) {
      setMessage("This sign-in is no longer pending. Return to the store to start again.")
      return
    }
    setOrder(pending)
    const back = () => window.location.replace(`/pay/${pending}`)
    if (!code) {
      back()
      return
    }
    const client = createBillingClient({ baseUrl: "/v1", fetch: paymentPageFetch(secret) })
    finishSignIn(state, code, client).then(back, (err: unknown) =>
      setMessage(err instanceof Error && err.message ? err.message : "The sign-in did not finish.")
    )
  }, [])
  return (
    <main className="payment">
      <p className="muted" role="status">
        {message}
      </p>
      {order && message !== "Signing you in…" ? <a href={`/pay/${order}`}>Back to payment</a> : null}
    </main>
  )
}
