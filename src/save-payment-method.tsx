import * as React from "react"

import {
  appearanceStyle,
  appearanceTheme,
  type CheckoutAppearance,
} from "./appearance"
import { isBillingError } from "./client/errors"
import { PayButton, TrustLine } from "./components/pay-button"
import {
  StripeCardEntry,
  type StripeCardHandle,
} from "./components/stripe-card"
import { useMessages } from "./i18n/context"
import { initialCountry } from "./lib/billing"
import { cardSetupDriver, type PspConfig } from "./psp"
import { useBillingContext } from "./react/context"
import {
  TokenizedCardForm,
  type TokenizedCardData,
} from "./tokenized-card-form"
import { cn } from "cn"

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
  /** Billing country to preselect; default: the browser locale's region. */
  defaultCountry?: string
  submitLabel?: string
  appearance?: CheckoutAppearance
  className?: string
}

/**
 * Saves a card to the customer's account with any PSP OpenRails serves, in
 * the page. The PSP's configuration picks the flow; card data only ever
 * enters provider frames. Requires `BillingProvider`.
 */
export function SavePaymentMethod(props: SavePaymentMethodProps) {
  const { t } = useMessages()
  const driver = cardSetupDriver(props.psp)
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
      <p className="text-sm text-muted-foreground">
        {t("paymentMethods.saveNotice")}
      </p>
      {driver === "collect_js" ? (
        <TokenizedSetup {...props} />
      ) : (
        <ElementsSetup {...props} />
      )}
    </div>
  )
}

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
  defaultCountry,
}: SavePaymentMethodProps) {
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
        onTokenized={save}
        defaultCountry={defaultCountry}
        submitLabel={submitLabel ?? m.t("paymentMethods.save")}
        appearance={appearance}
      />
    </>
  )
}

function ElementsSetup({
  psp,
  onSaved,
  returnURL,
  submitLabel,
  defaultCountry,
}: SavePaymentMethodProps) {
  const m = useMessages()
  const { t } = m
  const { client, notify } = useBillingContext()
  const card = React.useRef<StripeCardHandle>(null)
  const [complete, setComplete] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [error, setError] = React.useState<string>()
  const country = React.useMemo(
    () => initialCountry(defaultCountry),
    [defaultCountry]
  )

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (!card.current || busy) return
    setBusy(true)
    setError(undefined)
    try {
      const id = await card.current.save()
      notify({ type: "payment_method.added", paymentMethodId: id })
      onSaved(id)
    } catch (cause) {
      setError(describe(m, cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="grid gap-4" onSubmit={(event) => void submit(event)}>
      <StripeCardEntry
        ref={card}
        psp={psp}
        client={client}
        returnURL={returnURL}
        defaultCountry={country || undefined}
        onCompleteChange={setComplete}
        unavailableMessage={t("paymentMethods.unavailable")}
        verificationPendingMessage={t("paymentMethods.verificationPending")}
      />
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
      <PayButton
        label={submitLabel ?? t("paymentMethods.save")}
        processing={busy}
        disabled={!complete}
      />
      <TrustLine />
    </form>
  )
}
