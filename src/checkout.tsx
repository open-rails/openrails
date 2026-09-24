// Checkout — the entire flow as one self-contained component. Hosts render
// it inline on any page; CheckoutModal wraps it in a dialog; the hosted page
// is another thin host. Its only inputs are a CheckoutSource (data), an
// appearance (theming), and lifecycle callbacks. The container query is the
// mode switch: wide containers get the split layout, narrow ones the compact
// stack.
//
// Cards are one panel: the saved cards for the checkout PSP, an inline new
// card, and one button that is the payer's explicit confirmation of the
// displayed terms. With a BillingProvider, a new card is saved to the
// customer's account first and the payment charges it by id; a decline keeps
// the buyer on the panel to pick another card and retry.
import * as React from "react"

import {
  appearanceStyle,
  appearanceTheme,
  type CheckoutAppearance,
} from "#orck/appearance"
import { authenticatePayment } from "#orck/authenticate"
import { isBillingError } from "#orck/client/errors"
import type { PaymentMethod } from "#orck/client/types"
import { CardBillingFields } from "#orck/components/billing-fields"
import { CardFields } from "#orck/components/card-fields"
import { CCBillFields } from "#orck/components/ccbill-fields"
import {
  ccbillBillingSchema,
  emptyCCBillBilling,
  type CCBillBilling,
} from "#orck/lib/ccbill"
import { MethodBody, MethodList } from "#orck/components/methods"
import { solanaToken, supportedOptions } from "#orck/lib/rail-meta"
import { PayButton, TrustLine } from "#orck/components/pay-button"
import { SolanaBody } from "#orck/components/qr"
import { NEW_CARD_VALUE, SavedMethods } from "#orck/components/saved-methods"
import {
  LoadingSkeleton,
  SucceededView,
  TerminalView,
} from "#orck/components/states"
import {
  StripeCardEntry,
  type StripeCardHandle,
} from "#orck/components/stripe-card"
import { CompactSummary, OrderSummary } from "#orck/components/summary"
import { useMessages } from "#orck/i18n/context"
import {
  collectCardDisplay,
  useCollectJS,
  type CollectFieldErrors,
} from "#orck/lib/collect"
import {
  emptyNMIBilling,
  initialCountry,
  nmiBillingSchema,
  type NMIBilling,
} from "#orck/lib/billing"
import { amountToDecimal, formatAmount } from "#orck/lib/money"
import { everyLabel } from "#orck/lib/period"
import { isCardRail, railPsp } from "#orck/psp"
import { useOptionalBillingContext } from "#orck/react/context"
import { cn } from "cn"
import type { CheckoutSource } from "#orck/source"
import type {
  CheckoutPhase,
  CheckoutSession,
  PaymentFailure,
  PaymentRailOption,
  PayRequest,
  PayResult,
  SavedPaymentMethod,
} from "#orck/types"

// Layout preference. Responsiveness always wins: the split layout only ever
// renders when the container is genuinely wide enough for it.
// - "auto": split on comfortably wide containers (≥48rem)
// - "wide": prefer the split — it engages earlier (≥36rem); pairs with the
//   wide CheckoutModal
// - "compact": always the stacked view, no matter how wide the container
export type CheckoutLayout = "auto" | "wide" | "compact"

export interface CheckoutProps {
  source: CheckoutSource
  appearance?: CheckoutAppearance
  layout?: CheckoutLayout
  /** Billing country to preselect; default: the browser locale's region. */
  defaultCountry?: string
  /** Return target after an off-page card verification (Stripe setup). */
  cardSetupReturnURL?: (setupId: string) => string
  // Embedded hosts get the result via callback; page hosts also redirect.
  onComplete?: (result: PayResult) => void
  onPhaseChange?: (phase: CheckoutPhase) => void
  // Redirect hosts can announce the short return transition after success;
  // embedded/modal hosts stay put and use the settled confirmation copy.
  completionMode?: "embedded" | "redirect"
  className?: string
}

const POLL_INTERVAL_MS = 3_000
const DECLINED = "Your card was declined. Try another card."

function navigateTop(redirectURL: string): void {
  const parsed = new URL(redirectURL)
  if (parsed.protocol !== "https:" || parsed.username || parsed.password) {
    throw new Error("The payment provider returned an invalid redirect")
  }

  // Cross-origin frames may navigate their top-level browsing context, but
  // cannot read methods such as Location.assign from the top window.
  const target = window.top ?? window
  target.location.href = parsed.href
}

function solanaAmountLabel(
  session: CheckoutSession,
  tokenSymbol: string,
  transactionURL?: string
): string {
  if (transactionURL) {
    try {
      const amount = new URL(transactionURL).searchParams.get("amount")
      if (amount && /^\d+(?:\.\d+)?$/.test(amount))
        return `${amount} ${tokenSymbol}`.trim()
    } catch {
      // The wire schema already requires solana:, so retain the plan fallback.
    }
  }
  const { unit_amount, unit_decimals } = session.plan
  return `${amountToDecimal(unit_amount, unit_decimals) ?? unit_amount} ${tokenSymbol}`.trim()
}

function savedFrom(
  method: PaymentMethod,
  rail: PaymentRailOption
): SavedPaymentMethod {
  return {
    id: method.id,
    option_id: rail.id,
    rail: rail.rail,
    brand: method.card?.brand ?? undefined,
    last_four: method.card?.last4 ?? undefined,
    exp_month: method.card?.exp_month ?? undefined,
    exp_year: method.card?.exp_year ?? undefined,
  }
}

// Provider SDK errors carry customer-facing text ("Your card was declined.").
function describe(cause: unknown, fallback: string): string {
  if (isBillingError(cause)) return cause.message || fallback
  return cause instanceof Error && cause.message ? cause.message : fallback
}

const awaitingPayment = (session: CheckoutSession | null) =>
  session?.status === "processing" ||
  (session?.status === "requires_action" && !!session.operation)

export function Checkout({
  source,
  appearance,
  layout = "auto",
  defaultCountry,
  cardSetupReturnURL,
  onComplete,
  onPhaseChange,
  completionMode = "embedded",
  className,
}: CheckoutProps) {
  const m = useMessages()
  const billingContext = useOptionalBillingContext()
  const client = billingContext?.client
  const uid = React.useId().replace(/[^a-zA-Z0-9-]/g, "")
  const [phase, setPhase] = React.useState<CheckoutPhase>("loading")
  const [session, setSession] = React.useState<CheckoutSession | null>(null)
  const [selected, setSelected] = React.useState<string>("")
  const [payError, setPayError] = React.useState<string>()
  const [failure, setFailure] = React.useState<PaymentFailure>()
  const [billing, setBilling] =
    React.useState<CCBillBilling>(emptyCCBillBilling)
  const [cardBilling, setCardBilling] = React.useState<NMIBilling>(() => ({
    ...emptyNMIBilling,
    country: initialCountry(defaultCountry),
  }))
  const [solanaURL, setSolanaURL] = React.useState<string>()
  // Cards saved during this checkout, most recent first.
  const [addedCards, setAddedCards] = React.useState<SavedPaymentMethod[]>([])
  const [stripeComplete, setStripeComplete] = React.useState(false)
  const [stripeKey, setStripeKey] = React.useState(0)
  const [authenticating, setAuthenticating] = React.useState(false)
  const stripeCard = React.useRef<StripeCardHandle>(null)
  // Set once this component submitted a payment: a later definite decline
  // returns to the panel instead of a terminal page.
  const attempted = React.useRef(false)
  const mounted = React.useRef(true)
  const sourceRef = React.useRef(source)
  React.useEffect(() => {
    sourceRef.current = source
  }, [source])
  const phaseRef = React.useRef(phase)
  const solanaStartedFor = React.useRef<string | undefined>(undefined)

  // onPhaseChange rides a ref so changePhase stays referentially stable —
  // hosts pass inline callbacks (CheckoutModal does), and a changing identity
  // here would re-fire the load effect on every render.
  const onPhaseChangeRef = React.useRef(onPhaseChange)
  React.useEffect(() => {
    onPhaseChangeRef.current = onPhaseChange
  })
  const onCompleteRef = React.useRef(onComplete)
  React.useEffect(() => {
    onCompleteRef.current = onComplete
  })
  const changePhase = React.useCallback((next: CheckoutPhase) => {
    phaseRef.current = next
    setPhase(next)
    onPhaseChangeRef.current?.(next)
  }, [])

  React.useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  // Card rails that save a card in the page need the customer's billing
  // client; without one (a hosted page) only token rails are offered.
  const usable = React.useCallback(
    (rails: PaymentRailOption[]) =>
      supportedOptions(rails).filter(
        (option) => option.driver !== "stripe_elements" || !!client
      ),
    [client]
  )

  // Load the session once per source; map its status straight to a phase.
  React.useEffect(() => {
    let cancelled = false
    solanaStartedFor.current = undefined
    attempted.current = false
    // eslint-disable-next-line react-hooks/set-state-in-effect -- deliberate: a new source restarts the flow from loading
    changePhase("loading")
    setSolanaURL(undefined)
    source
      .getSession()
      .then((loaded) => {
        if (cancelled || !mounted.current) return
        setSession(loaded)
        setSolanaURL(loaded.transaction_url)
        const options = usable(loaded.rails)
        if (loaded.status === "succeeded") {
          changePhase("succeeded")
          onCompleteRef.current?.({
            status: "succeeded",
            payment_id: loaded.payment_id,
            subscription_id: loaded.subscription_id,
          })
          return
        }
        if (loaded.status === "expired" || loaded.status === "canceled") {
          changePhase("expired")
          return
        }
        if (loaded.status === "failed") {
          changePhase("failed")
          return
        }
        if (loaded.status === "blocked") {
          changePhase("blocked")
          return
        }
        if (awaitingPayment(loaded)) {
          const card = options.find(isCardRail)
          if (card) setSelected(card.id)
          changePhase("processing")
          return
        }
        if (options.length === 0) {
          changePhase("unavailable")
          return
        }
        setSelected((current) =>
          options.some((option) => option.id === current)
            ? current
            : options[0].id
        )
        changePhase("ready")
      })
      .catch(() => {
        if (!cancelled && mounted.current) changePhase("error")
      })
    return () => {
      cancelled = true
    }
  }, [source, changePhase, usable])

  // Expiry is enforced client-side too, so the page never invites a payment
  // the backend will refuse.
  React.useEffect(() => {
    if (!session?.expires_at) return
    const expiresAt = session.expires_at
    const remaining = Date.parse(expiresAt) - Date.now()
    if (Number.isNaN(remaining)) return
    // setTimeout overflows past 2^31-1ms and fires immediately, so clamp and
    // re-check the wall clock on fire instead of trusting the timer.
    const delay = Math.min(Math.max(0, remaining), 2 ** 31 - 1)
    const timer = window.setTimeout(() => {
      if (phaseRef.current !== "ready") return
      if (Date.parse(expiresAt) - Date.now() <= 0) {
        changePhase("expired")
      }
    }, delay)
    return () => window.clearTimeout(timer)
  }, [session, changePhase])

  const options = React.useMemo(
    () => (session ? usable(session.rails) : []),
    [session, usable]
  )
  const active = options.find((option) => option.id === selected)
  const nmiOption = options.find((option) => option.driver === "collect_js")
  const savedMethods = React.useMemo(() => {
    if (!active || !isCardRail(active)) return []
    const seen = new Set<string>()
    return [...addedCards, ...(session?.saved_methods ?? [])].filter(
      (method) => {
        if (method.option_id !== active.id || seen.has(method.id)) return false
        seen.add(method.id)
        return true
      }
    )
  }, [active, addedCards, session])
  // Until the customer chooses, the most recent stored card stands selected:
  // paying again with what is on file is the common case, and it keeps entry
  // fields out of the way. Derived rather than stored so the default still
  // applies when the session arrives after first render.
  const [savedChoice, setSavedChoice] = React.useState<string>()
  const savedMethodID =
    savedChoice &&
    (savedChoice === NEW_CARD_VALUE ||
      savedMethods.some((method) => method.id === savedChoice))
      ? savedChoice
      : (savedMethods[0]?.id ?? NEW_CARD_VALUE)
  const usingSavedMethod =
    savedMethodID !== NEW_CARD_VALUE &&
    savedMethods.some((method) => method.id === savedMethodID)
  const cardIds = React.useMemo(
    () => ({
      number: `orck-${uid}-cc-number`,
      expiry: `orck-${uid}-cc-expiry`,
      cvv: `orck-${uid}-cc-cvv`,
    }),
    [uid]
  )
  const collect = useCollectJS({
    enabled:
      Boolean(nmiOption) &&
      (phase === "ready" ||
        (phase === "processing" && !awaitingPayment(session))),
    active: selected === nmiOption?.id,
    tokenizationKey: nmiOption?.public_config?.tokenization_key ?? "",
    scriptURL: nmiOption?.public_config?.tokenization_url ?? "",
    selectors: {
      number: `#${cardIds.number}`,
      expiry: `#${cardIds.expiry}`,
      cvv: `#${cardIds.cvv}`,
    },
  })

  const clearFailure = () => {
    setPayError(undefined)
    setFailure(undefined)
  }

  const showFailure = React.useCallback(
    (result: { failure?: PaymentFailure | null; failure_message?: string }) => {
      const next = result.failure ?? undefined
      setFailure(next)
      setPayError(next?.message || result.failure_message || DECLINED)
    },
    []
  )

  // Saves a newly entered card to the customer's account and selects it, so
  // a retry after a decline never enters or saves the card again.
  const saveNewCard = React.useCallback(
    async (rail: PaymentRailOption): Promise<PayRequest | null> => {
      if (rail.driver === "stripe_elements") {
        if (!client || !stripeCard.current) return null
        const id = await stripeCard.current.save()
        let saved: SavedPaymentMethod = {
          id,
          option_id: rail.id,
          rail: rail.rail,
        }
        try {
          const page = await client.listPaymentMethods({ limit: 100 })
          const method = page.data.find((item) => item.id === id)
          if (method) saved = savedFrom(method, rail)
        } catch {
          // The card is saved; its label arrives with the next session read.
        }
        billingContext?.notify({
          type: "payment_method.added",
          paymentMethodId: id,
        })
        setAddedCards((current) => [saved, ...current])
        setSavedChoice(id)
        setStripeKey((key) => key + 1)
        return { option_id: rail.id, payment_method_id: id }
      }
      const parsed = nmiBillingSchema.safeParse(cardBilling)
      if (!parsed.success) {
        setPayError(
          parsed.error.issues[0]?.message ?? "Check the billing details"
        )
        return null
      }
      const tokenized = await collect.tokenize()
      const display = collectCardDisplay(tokenized.card)
      if (!client) {
        return {
          option_id: rail.id,
          payment_token: tokenized.token,
          ...parsed.data,
          ...display,
        }
      }
      const method = await client.addPaymentMethod({
        provider: rail.psp_key ?? rail.rail,
        payment_token: tokenized.token,
        ...parsed.data,
        ...display,
      })
      billingContext?.notify({
        type: "payment_method.added",
        paymentMethodId: method.id,
      })
      const saved = savedFrom(method, rail)
      setAddedCards((current) => [
        {
          ...saved,
          brand: saved.brand ?? display.card_type,
          last_four: saved.last_four ?? display.last_four,
        },
        ...current,
      ])
      setSavedChoice(method.id)
      return { option_id: rail.id, payment_method_id: method.id }
    },
    [billingContext, cardBilling, client, collect]
  )

  const authenticate = React.useCallback(
    async (operationID: string) => {
      if (!client || !active) return
      setAuthenticating(true)
      setPayError(undefined)
      try {
        await authenticatePayment(client, operationID, railPsp(active))
      } catch (cause) {
        if (mounted.current)
          setPayError(describe(cause, "Card authentication was not completed."))
      } finally {
        if (mounted.current) setAuthenticating(false)
      }
    },
    [active, client]
  )

  const pay = React.useCallback(async () => {
    if (!session || !active || phase !== "ready") return
    clearFailure()
    changePhase("processing")
    let submitted = false
    try {
      let request: PayRequest = { option_id: active.id }
      if (active.driver === "redirect" && active.rail === "ccbill") {
        const parsed = ccbillBillingSchema.safeParse(billing)
        if (!parsed.success) {
          setPayError(
            parsed.error.issues[0]?.message ?? "Check the billing details"
          )
          changePhase("ready")
          return
        }
        request = { option_id: active.id, ...parsed.data }
      }
      if (isCardRail(active) && usingSavedMethod) {
        request = { option_id: active.id, payment_method_id: savedMethodID }
      } else if (isCardRail(active)) {
        let saved: PayRequest | null
        try {
          saved = await saveNewCard(active)
        } catch (cause) {
          if (!mounted.current || sourceRef.current !== source) return
          setPayError(describe(cause, "Card entry could not be completed."))
          changePhase("ready")
          return
        }
        if (!mounted.current || sourceRef.current !== source) return
        if (!saved) {
          changePhase("ready")
          return
        }
        request = saved
      }
      if (active.driver === "solana_pay") {
        // supportedOptions only offers Solana options with a bound token, so
        // the symbol the host bound is the one we pay with — never a default.
        request = {
          option_id: active.id,
          token_symbol: solanaToken(active)?.symbol,
        }
      }
      submitted = true
      attempted.current = true
      const result = await source.pay(request)
      if (!mounted.current || sourceRef.current !== source) return
      if (active.driver === "redirect" && result.redirect_url) {
        onCompleteRef.current?.(result)
        navigateTop(result.redirect_url)
        return
      }
      if (
        active.driver === "solana_pay" &&
        result.status === "requires_action" &&
        result.transaction_url
      ) {
        setSession((current) =>
          current
            ? {
                ...current,
                status: "requires_action",
                transaction_url: result.transaction_url,
              }
            : current
        )
        setSolanaURL(result.transaction_url)
        changePhase("ready")
        return
      }
      if (result.status === "succeeded") {
        changePhase("succeeded")
        onCompleteRef.current?.(result)
        return
      }
      if (result.status === "failed") {
        showFailure(result)
        changePhase("ready")
        return
      }
      if (result.status === "blocked") {
        setSession((current) =>
          current
            ? {
                ...current,
                status: "blocked",
                failure_message: result.failure_message,
              }
            : current
        )
        changePhase("blocked")
        return
      }
      if (result.status === "expired" || result.status === "canceled") {
        setSession((current) =>
          current ? { ...current, status: result.status } : current
        )
        changePhase("expired")
        return
      }
      if (result.status === "requires_action" && result.operation_id) {
        const operation = { id: result.operation_id, status: "pending" }
        setSession((current) =>
          current
            ? { ...current, status: "requires_action", operation }
            : current
        )
        changePhase("processing")
        await authenticate(result.operation_id)
        return
      }
      setSession((current) =>
        current ? { ...current, status: "processing" } : current
      )
      changePhase("processing")
    } catch (err) {
      if (!mounted.current || sourceRef.current !== source) return
      if (submitted) {
        setPayError(
          "The payment result is not confirmed yet. Keep this window open while we check its status."
        )
        setSession((current) =>
          current ? { ...current, status: "processing" } : current
        )
        changePhase("processing")
      } else {
        setPayError(describe(err, "Card entry could not be completed."))
        changePhase("ready")
      }
    }
  }, [
    active,
    authenticate,
    billing,
    changePhase,
    phase,
    saveNewCard,
    savedMethodID,
    session,
    showFailure,
    source,
    usingSavedMethod,
  ])

  // Solana has no submit button: selecting it creates one transfer request,
  // then the QR remains stable while the server watches its unique reference.
  React.useEffect(() => {
    if (
      !session ||
      active?.driver !== "solana_pay" ||
      phase !== "ready" ||
      solanaURL ||
      solanaStartedFor.current === session.id
    ) {
      return
    }
    solanaStartedFor.current = session.id
    void pay()
  }, [active?.driver, pay, phase, session, solanaURL])

  React.useEffect(() => {
    if (
      !session ||
      (!(phase === "processing" && awaitingPayment(session)) &&
        !(active?.driver === "solana_pay" && phase === "ready" && solanaURL))
    )
      return

    let cancelled = false
    let timer: number | undefined
    const schedule = () => {
      timer = window.setTimeout(() => void poll(), POLL_INTERVAL_MS)
    }
    const poll = async () => {
      try {
        const loaded = await source.getSession()
        if (cancelled || !mounted.current) return
        switch (loaded.status) {
          case "succeeded":
            setSession(loaded)
            changePhase("succeeded")
            onCompleteRef.current?.({
              status: "succeeded",
              payment_id: loaded.payment_id,
              subscription_id: loaded.subscription_id,
            })
            return
          case "failed":
            if (attempted.current) {
              // A definite decline of this panel's attempt: back to the
              // cards, with the reason, for another card and a new attempt.
              setSession({ ...loaded, status: "created", operation: null })
              showFailure(loaded)
              changePhase("ready")
              return
            }
            setSession(loaded)
            changePhase("failed")
            return
          case "blocked":
            setSession(loaded)
            changePhase("blocked")
            return
          case "expired":
          case "canceled":
            setSession(loaded)
            changePhase("expired")
            return
          case "requires_action":
            if (loaded.operation) {
              setSession(loaded)
              schedule()
              return
            }
            schedule()
            return
          default:
            // Ready-looking reads can race an accepted write. Keep the local
            // pending presentation and poll; never re-enable card entry.
            schedule()
        }
      } catch {
        if (!cancelled) schedule()
      }
    }

    schedule()
    return () => {
      cancelled = true
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [
    active?.driver,
    changePhase,
    phase,
    session,
    showFailure,
    solanaURL,
    source,
  ])

  const merchantName = session?.merchant.display_name ?? ""
  const solanaTokenSymbol =
    active?.rail === "solana" ? (solanaToken(active)?.symbol ?? "") : ""
  const processing = phase === "processing"
  const amount = session
    ? formatAmount(
        session.plan.unit_amount,
        session.plan.currency,
        session.plan.unit_decimals
      )
    : ""
  const renews = session?.plan.automatically_renews
    ? everyLabel(session.plan.period_hours, m)
    : null
  const payLabel =
    active && session
      ? active.driver === "redirect"
        ? `Continue to ${active.rail === "stripe" ? "Stripe" : "CCBill"}`
        : renews
          ? `Subscribe for ${amount} ${renews}`
          : `Pay ${amount}`
      : ""

  // A decline about a specific field is shown next to it while the new card
  // entry is open; otherwise under the card choice.
  const newCardOpen = !!active && isCardRail(active) && !usingSavedMethod
  const collectErrors: CollectFieldErrors = { ...collect.fieldErrors }
  let postalError: string | undefined
  let inlineFailure = false
  if (failure?.field && newCardOpen && active?.driver === "collect_js") {
    inlineFailure = true
    if (failure.field === "number") collectErrors.number = failure.message
    else if (failure.field === "expiry") collectErrors.expiry = failure.message
    else if (failure.field === "cvc") collectErrors.cvv = failure.message
    else if (failure.field === "postal_code") postalError = failure.message
    else inlineFailure = false
  }

  // Card errors sit with the cards; other rails keep the form-level line.
  const cardError =
    payError && active && isCardRail(active) && !inlineFailure
      ? payError
      : undefined
  const cardBody = (option: PaymentRailOption, isActive: boolean) => {
    const stripe = option.driver === "stripe_elements"
    const entryOpen = isActive && !usingSavedMethod
    return (
      <MethodBody hidden={!isActive}>
        {savedMethods.length > 0 ? (
          <SavedMethods
            methods={savedMethods}
            value={savedMethodID}
            onChange={(next) => {
              setSavedChoice(next)
              clearFailure()
            }}
            disabled={processing}
            idPrefix={`orck-${uid}-saved`}
          />
        ) : null}
        {stripe ? (
          entryOpen && client ? (
            <StripeCardEntry
              key={stripeKey}
              ref={stripeCard}
              psp={railPsp(option)}
              client={client}
              returnURL={cardSetupReturnURL}
              defaultCountry={cardBilling.country || undefined}
              onCompleteChange={setStripeComplete}
              unavailableMessage={m.t("paymentMethods.unavailable")}
              verificationPendingMessage={m.t(
                "paymentMethods.verificationPending"
              )}
            />
          ) : null
        ) : (
          // Card fields stay mounted once created so the Collect.js iframes
          // survive rail and card switches; hidden handles visibility.
          <div
            className={cn("grid gap-3", usingSavedMethod && "hidden")}
            aria-hidden={usingSavedMethod || undefined}
          >
            <CardBillingFields
              idPrefix={`orck-${uid}-card`}
              value={cardBilling}
              onChange={(next) => {
                setCardBilling(next)
                clearFailure()
              }}
              disabled={processing || !isActive || usingSavedMethod}
              postalError={isActive ? postalError : undefined}
            />
            <CardFields
              ids={cardIds}
              preview={collect.preview}
              error={isActive ? collect.loadError : undefined}
              fieldErrors={isActive ? collectErrors : undefined}
            />
          </div>
        )}
        {isActive && cardError ? (
          <p className="text-[13px] text-destructive" role="alert">
            {cardError}
          </p>
        ) : null}
        {newCardOpen && isActive && client ? (
          <p className="text-xs text-muted-foreground">
            {m.t("paymentMethods.saveNotice")}
          </p>
        ) : null}
      </MethodBody>
    )
  }

  const paymentColumn = session ? (
    <form
      id={`orck-${uid}-payment-form`}
      name="openrails-checkout"
      aria-label="Secure payment"
      autoComplete="on"
      noValidate
      className="grid content-start gap-4"
      onSubmit={(event) => {
        event.preventDefault()
        void pay()
      }}
    >
      <CompactSummary
        session={session}
        className={cn(
          layout === "auto" && "@3xl/orck:hidden",
          layout === "wide" && "@xl/orck:hidden"
        )}
      />
      <MethodList
        rails={options}
        selected={selected}
        onSelect={(optionID) => {
          const next = options.find((option) => option.id === optionID)
          if (next?.driver === "solana_pay" && !solanaURL) {
            // A failed request may be retried by returning to Solana.
            solanaStartedFor.current = undefined
          }
          setSelected(optionID)
          clearFailure()
        }}
        disabled={processing}
        idPrefix={`orck-${uid}`}
        renderBody={(option, isActive) => {
          if (isCardRail(option)) return cardBody(option, isActive)
          if (!isActive) return null
          if (option.driver === "solana_pay") {
            return (
              <MethodBody>
                <SolanaBody
                  amountLabel={solanaAmountLabel(
                    session,
                    solanaTokenSymbol,
                    solanaURL
                  )}
                  payload={solanaURL}
                  statusLine={
                    solanaURL
                      ? "Watching for payment…"
                      : payError
                        ? "Payment request unavailable"
                        : "Preparing payment…"
                  }
                />
              </MethodBody>
            )
          }
          if (option.driver === "redirect" && option.rail === "ccbill") {
            return (
              <MethodBody>
                <CCBillFields
                  idPrefix={`orck-${uid}-ccbill`}
                  value={billing}
                  onChange={(next) => {
                    setBilling(next)
                    setPayError(undefined)
                  }}
                  disabled={processing}
                  error={payError}
                />
                <p className="text-[13px] text-muted-foreground">
                  You’ll finish payment on CCBill’s secure page.
                </p>
              </MethodBody>
            )
          }
          return (
            <MethodBody>
              <p className="text-[13px] text-muted-foreground">
                You’ll finish payment on Stripe’s secure page.
              </p>
            </MethodBody>
          )
        }}
      />
      {payError && active && active.rail !== "ccbill" && !isCardRail(active) ? (
        <p className="text-[13px] text-destructive" role="alert">
          {payError}
        </p>
      ) : null}
      {active && (active.driver !== "solana_pay" || Boolean(payError)) ? (
        <PayButton
          label={active.driver === "solana_pay" ? "Try again" : payLabel}
          processing={processing}
          disabled={
            isCardRail(active) &&
            !usingSavedMethod &&
            (active.driver === "collect_js"
              ? !collect.ready || !collect.valid || Boolean(collect.loadError)
              : !stripeComplete)
          }
        />
      ) : null}
      {renews && active && isCardRail(active) ? (
        <p className="-mt-1 text-center text-xs text-muted-foreground">
          {`You agree to pay ${amount} ${renews} until you cancel. First charge today; cancel anytime.`}
        </p>
      ) : null}
      <TrustLine />
    </form>
  ) : null

  const pendingOperation =
    session?.status === "requires_action" ? session.operation : null

  return (
    <div
      className={cn("orck w-full text-sm leading-normal", className)}
      data-orck-theme={appearanceTheme(appearance)}
      style={appearanceStyle(appearance)}
    >
      <span aria-live="polite" className="sr-only">
        {phase === "loading"
          ? "Loading checkout"
          : phase === "processing"
            ? "Processing payment"
            : phase === "succeeded"
              ? "Payment complete"
              : ""}
      </span>
      {phase === "loading" ? (
        <LoadingSkeleton />
      ) : phase === "error" ? (
        <TerminalView
          merchantName={merchantName || "Checkout"}
          headline="Checkout couldn’t load"
          sub="Reload the page to try again."
        />
      ) : phase === "expired" ? (
        <TerminalView
          merchantName={merchantName}
          headline="This checkout link has expired"
          sub={merchantName ? `Ask ${merchantName} for a new link.` : undefined}
        />
      ) : phase === "failed" ? (
        <TerminalView
          merchantName={merchantName}
          headline="Payment failed"
          sub="No payment was completed. Start a new checkout to try again."
        />
      ) : phase === "blocked" ? (
        <TerminalView
          merchantName={merchantName}
          headline="This purchase isn’t available"
          sub={session?.failure_message}
        />
      ) : phase === "unavailable" ? (
        <TerminalView
          merchantName={merchantName}
          headline="Checkout isn’t available right now"
        />
      ) : phase === "processing" && awaitingPayment(session) ? (
        <div role="status" className="grid gap-3 py-6 text-center">
          <div className="font-semibold">
            {pendingOperation
              ? "Confirm with your bank"
              : "Confirming payment…"}
          </div>
          <p className="text-muted-foreground">
            {pendingOperation
              ? "Your bank needs to confirm this payment."
              : "We’re checking the payment status. Don’t start another payment."}
          </p>
          {pendingOperation && client && active && isCardRail(active) ? (
            <button
              type="button"
              className="mx-auto h-9 rounded-[9px] bg-primary px-4 text-sm font-semibold text-primary-foreground disabled:opacity-60"
              disabled={authenticating}
              onClick={() => void authenticate(pendingOperation.id)}
            >
              {authenticating ? "Waiting for your bank…" : "Confirm payment"}
            </button>
          ) : null}
          {payError ? (
            <p className="text-xs text-muted-foreground">{payError}</p>
          ) : null}
        </div>
      ) : phase === "succeeded" ? (
        <SucceededView
          merchantName={merchantName}
          embedded={completionMode === "embedded"}
        />
      ) : session ? (
        <div
          className={cn(
            "grid gap-10",
            layout === "auto" &&
              "@3xl/orck:grid-cols-[minmax(0,1fr)_minmax(0,1fr)] @3xl/orck:gap-16",
            layout === "wide" &&
              "@xl/orck:grid-cols-[minmax(0,1fr)_minmax(0,1fr)] @xl/orck:gap-12"
          )}
        >
          {layout !== "compact" ? (
            <div
              className={cn(
                "hidden",
                layout === "auto" && "@3xl/orck:block",
                layout === "wide" && "@xl/orck:block"
              )}
            >
              <OrderSummary session={session} />
            </div>
          ) : null}
          <div>{paymentColumn}</div>
        </div>
      ) : null}
    </div>
  )
}
