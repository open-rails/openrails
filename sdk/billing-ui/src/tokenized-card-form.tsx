import * as React from "react"
import {
  appearanceStyle,
  appearanceTheme,
  type BillingAppearance,
} from "./appearance"
import { CardBillingFields } from "./components/billing-fields"
import { CardFields } from "./components/card-fields"
import { PayButton, TrustLine } from "./components/pay-button"
import {
  emptyNMIBilling,
  initialCountry,
  nmiBillingSchema,
  reuseNeedsOptIn,
} from "./lib/billing"
import { collectCardDisplay, useCollectJS } from "./lib/collect"
import { cn } from "cn"

// Only tokenized billing details cross the iframe boundary. PAN/CVC are never
// accepted by this component or handed to the host callback.
export interface TokenizedCardData {
  payment_token: string
  name_on_card: string
  country: string
  zip?: string
  /** Display metadata from the gateway: last four, brand, MM/YY. */
  last_four?: string
  card_type?: string
  expiry_date?: string
  /** The customer's opt-in to keep the card, when the form asked for it. */
  reusable?: boolean
}
export interface TokenizedCardFormProps {
  tokenizationKey: string
  tokenizationURL: string
  /**
   * The host's use of the card. Resolving `false` says it was definitely
   * refused (a decline): the form takes another card.
   */
  onTokenized: (card: TokenizedCardData) => Promise<void | boolean>
  submitLabel?: string
  disabled?: boolean
  /** Billing country to preselect; default: the browser locale's region. */
  defaultCountry?: string
  /**
   * The merchant the card would be kept with, for a one-off purchase: where
   * keeping it needs an opt-in (EEA, UK) the form asks, unticked.
   */
  saveOptInMerchant?: string
  /** Shown above the button (the terms the customer accepts). */
  terms?: React.ReactNode
  /** The name the charge carries on the card statement. */
  statementDescriptor?: string
  appearance?: BillingAppearance
  className?: string
}

// The host supplies consent and performs its authorized save-card operation.
// This component neither creates a purchase nor implies payment success.
export function TokenizedCardForm({
  tokenizationKey,
  tokenizationURL,
  onTokenized,
  submitLabel = "Save card",
  disabled = false,
  defaultCountry,
  saveOptInMerchant,
  terms,
  statementDescriptor,
  appearance,
  className,
}: TokenizedCardFormProps) {
  const [keepCard, setKeepCard] = React.useState(false)
  const uid = React.useId().replace(/[^a-zA-Z0-9-]/g, "")
  const ids = React.useMemo(
    () => ({
      number: `orck-save-${uid}-number`,
      expiry: `orck-save-${uid}-expiry`,
      cvv: `orck-save-${uid}-cvv`,
    }),
    [uid]
  )
  const [billing, setBilling] = React.useState(() => ({
    ...emptyNMIBilling,
    country: initialCountry(defaultCountry),
  }))
  const [busy, setBusy] = React.useState(false)
  const [submitted, setSubmitted] = React.useState(false)
  const [error, setError] = React.useState<string>()
  const authorization = React.useRef({ generation: 0, disabled })
  React.useLayoutEffect(() => {
    authorization.current = {
      generation: authorization.current.generation + 1,
      disabled,
    }
  }, [tokenizationKey, tokenizationURL, disabled])
  const mounted = React.useRef(true)
  React.useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const collect = useCollectJS({
    enabled: true,
    active: true,
    tokenizationKey,
    scriptURL: tokenizationURL,
    selectors: {
      number: `#${ids.number}`,
      expiry: `#${ids.expiry}`,
      cvv: `#${ids.cvv}`,
    },
  })
  // The button stays off until every required billing field is filled.
  const billingComplete = nmiBillingSchema.safeParse(billing).success
  const asksToKeep =
    saveOptInMerchant !== undefined && reuseNeedsOptIn(billing.country)
  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (disabled || busy || submitted) return
    const parsed = nmiBillingSchema.safeParse(billing)
    if (!parsed.success) {
      setError(parsed.error.issues[0]?.message ?? "Check the billing details")
      return
    }
    setBusy(true)
    setError(undefined)
    const generation = authorization.current.generation
    let sent = false
    try {
      const token = await collect.tokenize()
      if (
        !mounted.current ||
        authorization.current.generation !== generation ||
        authorization.current.disabled
      )
        return
      sent = true
      setSubmitted(true)
      const accepted = await onTokenized({
        payment_token: token.token,
        ...parsed.data,
        ...collectCardDisplay(token.card),
        ...(asksToKeep ? { reusable: keepCard } : {}),
      })
      if (accepted === false && mounted.current) setSubmitted(false)
    } catch (cause) {
      if (mounted.current)
        setError(
          sent
            ? "The card-save result is not confirmed. Check your saved cards before starting another attempt."
            : cause instanceof Error
              ? cause.message
              : "Card entry could not be completed."
        )
    } finally {
      if (mounted.current) setBusy(false)
    }
  }
  return (
    <div
      className={cn("orck", className)}
      data-orck-theme={appearanceTheme(appearance)}
      style={appearanceStyle(appearance)}
    >
      <form
        aria-label="Secure card setup"
        autoComplete="on"
        className="grid gap-4"
        onSubmit={(event) => void submit(event)}
      >
        <CardBillingFields
          idPrefix={`orck-save-${uid}`}
          value={billing}
          onChange={setBilling}
          disabled={disabled || busy || submitted}
        >
          <CardFields
            ids={ids}
            preview={collect.preview}
            brand={collect.brand}
            error={collect.loadError}
            fieldErrors={collect.fieldErrors}
          />
        </CardBillingFields>
        {asksToKeep ? (
          <label className="flex items-start gap-2 text-[13px] text-foreground">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={keepCard}
              onChange={(event) => setKeepCard(event.target.checked)}
              disabled={disabled || busy || submitted}
            />
            Save payment details to {saveOptInMerchant} for future purchases
          </label>
        ) : null}
        {terms ? (
          <p className="text-xs text-muted-foreground">{terms}</p>
        ) : null}
        {error ? (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        ) : null}
        <PayButton
          label={submitLabel}
          processing={busy}
          disabled={
            disabled ||
            submitted ||
            !billingComplete ||
            !collect.ready ||
            !collect.valid ||
            Boolean(collect.loadError)
          }
        />
        {statementDescriptor ? (
          <p className="-mt-1 text-center text-xs text-muted-foreground">
            The charge will appear as &ldquo;{statementDescriptor}&rdquo; on
            your card statement.
          </p>
        ) : (
          <TrustLine />
        )}
      </form>
    </div>
  )
}
