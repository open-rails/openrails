import * as React from "react"

import {
  appearanceStyle,
  appearanceTheme,
  type CheckoutAppearance,
} from "./appearance"
import { isBillingError } from "./client/errors"
import type { CardSetup } from "./client/types"
import { PayButton, TrustLine } from "./components/pay-button"
import { Button } from "./components/ui/button"
import { Spinner } from "./components/ui/spinner"
import { useMessages } from "./i18n/context"
import { loadStripeFor } from "./lib/stripe"
import { cardSetupDriver, type PspConfig } from "./psp"
import { useBillingContext } from "./react/context"
import {
  TokenizedCardForm,
  type TokenizedCardData,
} from "./tokenized-card-form"
import { cn } from "cn"
import type { StripeElements, Stripe } from "@stripe/stripe-js"

export interface SavePaymentMethodProps {
  /** The PSP to save the card with, from OpenRails's checkout config. */
  psp: PspConfig
  onSaved: (paymentMethodId: string) => void
  /**
   * Where the provider returns after an off-page verification (3-D Secure),
   * given the setup id to confirm with `client.confirmCardSetup`. Default:
   * the current page with `?setup_id=`.
   */
  returnURL?: (setupId: string) => string
  submitLabel?: string
  appearance?: CheckoutAppearance
  className?: string
}

/**
 * Saves a card with any PSP OpenRails serves, in the page. The PSP's
 * configuration picks the flow; card data only ever enters provider frames.
 * Requires `BillingProvider`.
 */
export function SavePaymentMethod(props: SavePaymentMethodProps) {
  const { t } = useMessages()
  const [consent, setConsent] = React.useState(false)
  const driver = cardSetupDriver(props.psp)
  const id = React.useId()
  if (!driver)
    return (
      <p role="alert" className="text-sm text-destructive">
        {t("paymentMethods.unavailable")}
      </p>
    )
  return (
    <div
      className={cn("orck grid gap-4", props.className)}
      data-orck-theme={appearanceTheme(props.appearance)}
      style={appearanceStyle(props.appearance)}
    >
      <label
        htmlFor={`${id}-consent`}
        className="flex items-start gap-2 text-sm"
      >
        <input
          id={`${id}-consent`}
          type="checkbox"
          className="mt-0.5 size-4 accent-[color:var(--primary)]"
          checked={consent}
          onChange={(event) => setConsent(event.target.checked)}
        />
        {t("paymentMethods.consent")}
      </label>
      {driver === "collect_js" ? (
        <TokenizedSetup {...props} consent={consent} />
      ) : (
        <ElementsSetup {...props} consent={consent} />
      )}
    </div>
  )
}

type SetupProps = SavePaymentMethodProps & { consent: boolean }

// Provider SDK errors carry customer-facing text ("Your card was declined.").
const describe = (m: ReturnType<typeof useMessages>, cause: unknown) =>
  !isBillingError(cause) && cause instanceof Error && cause.message
    ? cause.message
    : m.error(cause)

function TokenizedSetup({
  psp,
  onSaved,
  submitLabel,
  appearance,
  consent,
}: SetupProps) {
  const m = useMessages()
  const { client, notify } = useBillingContext()
  const [formKey, setFormKey] = React.useState(0)
  const [error, setError] = React.useState<string>()
  const save = async (card: TokenizedCardData) => {
    setError(undefined)
    let method
    try {
      method = await client.addPaymentMethod({ ...card, provider: psp.key })
    } catch (cause) {
      setError(describe(m, cause))
      setFormKey((key) => key + 1)
      return
    }
    notify({ type: "payment_method.added", paymentMethodId: method.id })
    onSaved(method.id)
  }
  return (
    <>
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
      <TokenizedCardForm
        key={formKey}
        tokenizationKey={psp.config!.tokenization_key!}
        tokenizationURL={psp.config!.tokenization_url!}
        disabled={!consent}
        onTokenized={save}
        submitLabel={submitLabel ?? m.t("paymentMethods.save")}
        appearance={appearance}
      />
    </>
  )
}

const defaultReturnURL = (setupId: string) => {
  const url = new URL(window.location.href)
  url.searchParams.set("setup_id", setupId)
  return url.href
}

function ElementsSetup({
  psp,
  onSaved,
  returnURL = defaultReturnURL,
  submitLabel,
  consent,
}: SetupProps) {
  const m = useMessages()
  const { t } = m
  const { client, notify } = useBillingContext()
  const [idempotencyKey] = React.useState(() => crypto.randomUUID())
  const [setup, setSetup] = React.useState<CardSetup>()
  const [busy, setBusy] = React.useState(false)
  const [error, setError] = React.useState<string>()
  const host = React.useRef<HTMLDivElement>(null)
  const mounted = React.useRef<{ stripe: Stripe; elements: StripeElements }>(
    null
  )

  const saved = (paymentMethodId: string) => {
    notify({ type: "payment_method.added", paymentMethodId })
    onSaved(paymentMethodId)
  }

  const start = async () => {
    setBusy(true)
    setError(undefined)
    try {
      const created = await client.createCardSetup({
        pspId: psp.psp_id,
        idempotencyKey,
      })
      if (created.payment_method_id) return saved(created.payment_method_id)
      if (!created.client_secret)
        throw new Error(t("paymentMethods.unavailable"))
      setSetup(created)
    } catch (cause) {
      setError(describe(m, cause))
    } finally {
      setBusy(false)
    }
  }

  const secret = setup?.client_secret
  React.useEffect(() => {
    if (!secret || !host.current) return
    let cancelled = false
    let unmount: (() => void) | undefined
    void loadStripeFor(psp.config!.publishable_key!).then(
      (stripe) => {
        if (cancelled || !host.current) return
        const dark = !!host.current.closest(".dark")
        const elements = stripe.elements({
          clientSecret: secret,
          appearance: { theme: dark ? "night" : "stripe" },
        })
        const element = elements.create("payment")
        element.mount(host.current)
        mounted.current = { stripe, elements }
        unmount = () => element.destroy()
      },
      (cause) => {
        if (!cancelled) setError(describe(m, cause))
      }
    )
    return () => {
      cancelled = true
      mounted.current = null
      unmount?.()
    }
  }, [secret, psp.config, m])

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    const current = mounted.current
    if (!setup || !current || busy) return
    setBusy(true)
    setError(undefined)
    try {
      const validation = await current.elements.submit()
      if (validation.error) throw new Error(validation.error.message)
      const result = await current.stripe.confirmSetup({
        elements: current.elements,
        confirmParams: { return_url: returnURL(setup.id) },
        redirect: "if_required",
      })
      if (result.error) throw new Error(result.error.message)
      const verified = await client.confirmCardSetup(setup.id)
      if (!verified.payment_method_id)
        throw new Error(t("paymentMethods.verificationPending"))
      saved(verified.payment_method_id)
    } catch (cause) {
      setError(describe(m, cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      {secret ? (
        <form className="grid gap-4" onSubmit={(event) => void submit(event)}>
          <div ref={host} />
          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          ) : null}
          <PayButton
            label={submitLabel ?? t("paymentMethods.save")}
            processing={busy}
          />
          <TrustLine />
        </form>
      ) : (
        <>
          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          ) : null}
          <Button
            variant="outline"
            disabled={!consent || busy}
            onClick={() => void start()}
          >
            {busy ? <Spinner /> : null}
            {t("paymentMethods.enterCard")}
          </Button>
        </>
      )}
    </>
  )
}
