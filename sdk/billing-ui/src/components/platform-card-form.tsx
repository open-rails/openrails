import * as React from "react"
import BasisTheory from "@basis-theory/web-elements"
import { CardBillingFields } from "./billing-fields"
import { CardFields } from "./card-fields"
import { PayButton, TrustLine } from "./pay-button"
import {
  emptyNMIBilling,
  initialCountry,
  nmiBillingSchema,
} from "../lib/billing"
import type { TokenizedCardData } from "../tokenized-card-form"

// Only the vendor's iframe handles PAN/CVC. An unconfirmed payment retains
// exactly the same token and billing details so Retry replays its request.
export function PlatformCardForm({
  publicKey,
  submitLabel,
  disabled,
  onTokenized,
  purpose = "payment",
  defaultCountry,
}: {
  purpose?: "payment" | "setup"
  defaultCountry?: string
  publicKey: string
  submitLabel: string
  disabled: boolean
  onTokenized: (card: TokenizedCardData) => Promise<boolean>
}) {
  const uid = React.useId().replace(/[^a-zA-Z0-9-]/g, "")
  const ids = React.useMemo(
    () => ({
      number: `bt-${uid}-number`,
      expiry: `bt-${uid}-expiry`,
      cvv: `bt-${uid}-cvv`,
    }),
    [uid]
  )
  const [billing, setBilling] = React.useState(() => ({
    ...emptyNMIBilling,
    country: initialCountry(defaultCountry),
  }))
  const [ready, setReady] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [submitted, setSubmitted] = React.useState(false)
  const [retry, setRetry] = React.useState(false)
  const [error, setError] = React.useState<string>()
  const pending = React.useRef<TokenizedCardData>(undefined)
  const tokenize = React.useRef<() => Promise<string>>(undefined)
  const submitting = React.useRef(false)
  const authorization = React.useRef({ generation: 0, disabled })
  React.useLayoutEffect(() => {
    authorization.current = {
      generation: authorization.current.generation + 1,
      disabled,
    }
  }, [publicKey, disabled])
  React.useEffect(() => {
    let active = true
    const bt = BasisTheory(publicKey)
    const number = bt.createElement("cardNumber")
    const expiry = bt.createElement("expiry")
    const cvv = bt.createElement("cvv")
    Promise.all([
      number.mount(`#${ids.number}`),
      expiry.mount(`#${ids.expiry}`),
      cvv.mount(`#${ids.cvv}`),
    ]).then(
      () => {
        if (!active) return
        tokenize.current = async () =>
          (
            await bt.tokenIntents.create({
              type: "card",
              data: {
                number,
                expiration_month: expiry,
                expiration_year: expiry,
                cvc: cvv,
              },
            })
          ).id
        setReady(true)
      },
      () => {
        if (active) setError("Secure card entry could not be loaded.")
      }
    )
    return () => {
      active = false
      authorization.current.generation++
      tokenize.current = undefined
      void bt.destroy()
    }
  }, [publicKey, ids])
  const retryLabel = purpose === "setup" ? "Retry saving card" : "Retry payment"
  const unconfirmedMessage =
    purpose === "setup"
      ? "The card save is not confirmed. Retry to check the same card."
      : "The payment result is not confirmed. Retry to check the same payment."
  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (disabled || submitting.current || submitted || !tokenize.current) return
    const parsed = nmiBillingSchema.safeParse(billing)
    if (!parsed.success) {
      setError(parsed.error.issues[0]?.message ?? "Check the billing details")
      return
    }
    submitting.current = true
    setBusy(true)
    setError(undefined)
    const generation = authorization.current.generation
    try {
      const card = pending.current ?? {
        payment_token: await tokenize.current(),
        ...parsed.data,
      }
      if (
        authorization.current.generation !== generation ||
        authorization.current.disabled
      )
        return
      pending.current = card
      const accepted = await onTokenized(card)
      if (accepted) setSubmitted(true)
      else pending.current = undefined
      setRetry(false)
    } catch {
      const sent = !!pending.current
      setRetry(sent)
      setError(
        sent ? unconfirmedMessage : "Check your card details and try again."
      )
    } finally {
      submitting.current = false
      setBusy(false)
    }
  }
  return (
    <form
      aria-label={
        purpose === "setup"
          ? "Secure platform card setup"
          : "Secure platform card payment"
      }
      className="grid gap-4"
      onSubmit={(event) => void submit(event)}
    >
      <CardBillingFields
        idPrefix={`bt-${uid}`}
        value={billing}
        onChange={setBilling}
        disabled={disabled || busy || retry || submitted}
      >
        <CardFields ids={ids} preview={false} />
      </CardBillingFields>
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
      <PayButton
        label={retry ? retryLabel : submitLabel}
        processing={busy}
        disabled={disabled || submitted || !ready}
      />
      <TrustLine />
    </form>
  )
}
