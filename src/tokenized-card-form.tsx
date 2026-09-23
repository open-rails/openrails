import * as React from "react"
import {
  appearanceStyle,
  appearanceTheme,
  type CheckoutAppearance,
} from "./appearance"
import { CardBillingFields } from "./components/billing-fields"
import { CardFields } from "./components/card-fields"
import { PayButton, TrustLine } from "./components/pay-button"
import { emptyNMIBilling, nmiBillingSchema } from "./lib/billing"
import { useCollectJS } from "./lib/collect"
import { cn } from "./lib/utils"

// Only tokenized billing details cross the iframe boundary. PAN/CVC are never
// accepted by this component or handed to the host callback.
export interface TokenizedCardData {
  payment_token: string
  name_on_card: string
  country: string
  zip?: string
}
export interface TokenizedCardFormProps {
  tokenizationKey: string
  tokenizationURL: string
  onTokenized: (card: TokenizedCardData) => Promise<void>
  submitLabel?: string
  disabled?: boolean
  appearance?: CheckoutAppearance
  className?: string
}

// The host supplies consent and performs its authorized save-card operation.
// This component neither creates a checkout nor implies payment success.
export function TokenizedCardForm({
  tokenizationKey,
  tokenizationURL,
  onTokenized,
  submitLabel = "Save card",
  disabled = false,
  appearance,
  className,
}: TokenizedCardFormProps) {
  const uid = React.useId().replace(/[^a-zA-Z0-9-]/g, "")
  const ids = React.useMemo(
    () => ({
      number: `orck-save-${uid}-number`,
      expiry: `orck-save-${uid}-expiry`,
      cvv: `orck-save-${uid}-cvv`,
    }),
    [uid]
  )
  const [billing, setBilling] = React.useState(emptyNMIBilling)
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
      await onTokenized({ payment_token: token.token, ...parsed.data })
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
        />
        <CardFields
          ids={ids}
          preview={collect.preview}
          error={collect.loadError}
        />
        {error ? (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        ) : null}
        <PayButton
          label={submitLabel}
          processing={busy}
          disabled={
            disabled ||
            submitted ||
            !collect.ready ||
            Boolean(collect.loadError)
          }
        />
        <TrustLine />
      </form>
    </div>
  )
}
