// Checkout — the entire flow as one self-contained component. Hosts render
// it inline on any page; CheckoutModal wraps it in a dialog; the hosted page
// is another thin host. Its only inputs are a CheckoutSource (data), an
// appearance (theming), and lifecycle callbacks. The container query is the
// mode switch: wide containers get the split layout, narrow ones the compact
// stack.
import * as React from "react"

import {
  appearanceStyle,
  appearanceTheme,
  type CheckoutAppearance,
} from "#orck/appearance"
import { CardBillingFields } from "#orck/components/billing-fields"
import { CardFields } from "#orck/components/card-fields"
import { CCBillFields } from "#orck/components/ccbill-fields"
import {
  ccbillBillingSchema,
  emptyCCBillBilling,
  type CCBillBilling,
} from "#orck/lib/ccbill"
import { MethodBody, MethodList } from "#orck/components/methods"
import { supportedOptions } from "#orck/lib/rail-meta"
import { PayButton, TrustLine } from "#orck/components/pay-button"
import { SolanaBody } from "#orck/components/qr"
import { NEW_CARD_VALUE, SavedMethods } from "#orck/components/saved-methods"
import {
  LoadingSkeleton,
  SucceededView,
  TerminalView,
} from "#orck/components/states"
import { CompactSummary, OrderSummary } from "#orck/components/summary"
import { collectExpiry, useCollectJS } from "#orck/lib/collect"
import {
  emptyNMIBilling,
  nmiBillingSchema,
  type NMIBilling,
} from "#orck/lib/billing"
import { formatMoney } from "#orck/lib/money"
import { cn } from "#orck/lib/utils"
import type { CheckoutSource } from "#orck/source"
import type {
  CheckoutPhase,
  CheckoutSession,
  PayRequest,
  PayResult,
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
  // Embedded hosts get the result via callback; page hosts also redirect.
  onComplete?: (result: PayResult) => void
  onPhaseChange?: (phase: CheckoutPhase) => void
  // Redirect hosts can announce the short return transition after success;
  // embedded/modal hosts stay put and use the settled confirmation copy.
  completionMode?: "embedded" | "redirect"
  className?: string
}

const SOLANA_POLL_INTERVAL_MS = 3_000

function solanaAmountLabel(
  session: CheckoutSession,
  tokenSymbol: string,
  transactionURL?: string
): string {
  if (transactionURL) {
    try {
      const amount = new URL(transactionURL).searchParams.get("amount")
      if (amount && /^\d+(?:\.\d+)?$/.test(amount))
        return `${amount} ${tokenSymbol}`
    } catch {
      // The wire schema already requires solana:, so retain the plan fallback.
    }
  }
  return `${(session.plan.unit_amount_micros / 1_000_000).toFixed(2)} ${tokenSymbol}`
}

export function Checkout({
  source,
  appearance,
  layout = "auto",
  onComplete,
  onPhaseChange,
  completionMode = "embedded",
  className,
}: CheckoutProps) {
  const uid = React.useId().replace(/[^a-zA-Z0-9-]/g, "")
  const [phase, setPhase] = React.useState<CheckoutPhase>("loading")
  const [session, setSession] = React.useState<CheckoutSession | null>(null)
  const [selected, setSelected] = React.useState<string>("")
  const [payError, setPayError] = React.useState<string>()
  const [billing, setBilling] =
    React.useState<CCBillBilling>(emptyCCBillBilling)
  const [cardBilling, setCardBilling] =
    React.useState<NMIBilling>(emptyNMIBilling)
  const [solanaURL, setSolanaURL] = React.useState<string>()
  const mounted = React.useRef(true)
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

  // Load the session once per source; map its status straight to a phase.
  React.useEffect(() => {
    let cancelled = false
    solanaStartedFor.current = undefined
    // eslint-disable-next-line react-hooks/set-state-in-effect -- deliberate: a new source restarts the flow from loading
    changePhase("loading")
    setSolanaURL(undefined)
    source
      .getSession()
      .then((loaded) => {
        if (cancelled || !mounted.current) return
        setSession(loaded)
        setSolanaURL(loaded.transaction_url)
        const options = supportedOptions(loaded.rails)
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
  }, [source, changePhase])

  // Expiry is enforced client-side too, so the page never invites a payment
  // the backend will refuse.
  React.useEffect(() => {
    if (!session) return
    const remaining = Date.parse(session.expires_at) - Date.now()
    if (Number.isNaN(remaining)) return
    // setTimeout overflows past 2^31-1ms and fires immediately, so clamp and
    // re-check the wall clock on fire instead of trusting the timer.
    const delay = Math.min(Math.max(0, remaining), 2 ** 31 - 1)
    const timer = window.setTimeout(() => {
      if (phaseRef.current !== "ready") return
      if (Date.parse(session.expires_at) - Date.now() <= 0) {
        changePhase("expired")
      }
    }, delay)
    return () => window.clearTimeout(timer)
  }, [session, changePhase])

  const options = React.useMemo(
    () => (session ? supportedOptions(session.rails) : []),
    [session]
  )
  const active = options.find((option) => option.id === selected)
  const nmiOption = options.find((option) => option.driver === "collect_js")
  const savedMethods = React.useMemo(
    () =>
      (session?.saved_methods ?? []).filter(
        (method) => method.option_id === nmiOption?.id
      ),
    [nmiOption?.id, session]
  )
  // Until the customer chooses, the first stored card stands selected: paying
  // again with what is on file is the common case, and it keeps entry fields
  // out of the way. Derived rather than stored so the default still applies
  // when the session arrives after first render.
  const [savedChoice, setSavedChoice] = React.useState<string>()
  const savedMethodID = savedChoice ?? savedMethods[0]?.id ?? NEW_CARD_VALUE
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
    enabled: Boolean(nmiOption),
    active: selected === nmiOption?.id,
    tokenizationKey: nmiOption?.public_config?.tokenization_key ?? "",
    scriptURL: nmiOption?.public_config?.tokenization_url ?? "",
    selectors: {
      number: `#${cardIds.number}`,
      expiry: `#${cardIds.expiry}`,
      cvv: `#${cardIds.cvv}`,
    },
  })

  const pay = React.useCallback(async () => {
    if (!session || !active || phase !== "ready") return
    setPayError(undefined)
    changePhase("processing")
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
      if (active.driver === "collect_js" && usingSavedMethod) {
        request = { option_id: active.id, payment_method_id: savedMethodID }
      } else if (active.driver === "collect_js") {
        const parsed = nmiBillingSchema.safeParse(cardBilling)
        if (!parsed.success) {
          setPayError(
            parsed.error.issues[0]?.message ?? "Check the billing details"
          )
          changePhase("ready")
          return
        }
        const tokenized = await collect.tokenize()
        if (!mounted.current) return
        request = {
          option_id: active.id,
          payment_token: tokenized.token,
          ...parsed.data,
        }
        const lastFour = tokenized.card?.number?.replace(/\D/g, "").slice(-4)
        if (lastFour) request.last_four = lastFour
        if (tokenized.card?.type) request.card_type = tokenized.card.type
        const expiry = collectExpiry(tokenized.card?.exp)
        if (expiry) request.expiry_date = expiry
      }
      if (active.driver === "solana_pay") {
        const tokenSymbol =
          active.public_config?.token_symbol?.trim().toUpperCase() || "USDC"
        request = {
          option_id: active.id,
          token_symbol: tokenSymbol,
        }
      }
      const result = await source.pay(request)
      if (!mounted.current) return
      if (active.driver === "redirect" && result.redirect_url) {
        onCompleteRef.current?.(result)
        const target = window.top ?? window
        target.location.assign(result.redirect_url)
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
        setPayError(
          result.failure_message ??
            "Payment failed. Check the details and try again."
        )
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
      setPayError("Checkout did not complete. Try again.")
      changePhase("ready")
    } catch (err) {
      if (!mounted.current) return
      setPayError(
        err instanceof Error ? err.message : "Payment failed. Try again."
      )
      changePhase("ready")
    }
  }, [
    active,
    billing,
    cardBilling,
    changePhase,
    collect,
    phase,
    savedMethodID,
    session,
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
      active?.driver !== "solana_pay" ||
      phase !== "ready" ||
      !solanaURL
    ) {
      return
    }

    let cancelled = false
    let timer: number | undefined
    const schedule = () => {
      timer = window.setTimeout(() => void poll(), SOLANA_POLL_INTERVAL_MS)
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
          default:
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
  }, [active?.driver, changePhase, phase, session, solanaURL, source])

  const merchantName = session?.merchant.display_name ?? ""
  const solanaTokenSymbol =
    active?.rail === "solana"
      ? active.public_config?.token_symbol?.trim().toUpperCase() || "USDC"
      : "USDC"
  const processing = phase === "processing"
  const payLabel =
    active && session
      ? active.driver === "redirect"
        ? `Continue to ${active.rail === "stripe" ? "Stripe" : "CCBill"}`
        : `Pay ${formatMoney(session.plan.unit_amount_micros, session.plan.currency)}`
      : ""

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
          setPayError(undefined)
        }}
        disabled={processing}
        idPrefix={`orck-${uid}`}
        renderBody={(option, isActive) => {
          if (option.driver === "collect_js") {
            // Card fields stay mounted once created so the Collect.js
            // iframes survive rail switches; hidden handles visibility.
            return (
              <MethodBody hidden={!isActive}>
                {savedMethods.length > 0 ? (
                  <SavedMethods
                    methods={savedMethods}
                    value={savedMethodID}
                    onChange={(next) => {
                      setSavedChoice(next)
                      setPayError(undefined)
                    }}
                    disabled={processing}
                    idPrefix={`orck-${uid}-saved`}
                  />
                ) : null}
                <div
                  className={cn("grid gap-3", usingSavedMethod && "hidden")}
                  aria-hidden={usingSavedMethod || undefined}
                >
                  <CardBillingFields
                    idPrefix={`orck-${uid}-card`}
                    value={cardBilling}
                    onChange={(next) => {
                      setCardBilling(next)
                      setPayError(undefined)
                    }}
                    disabled={processing || !isActive || usingSavedMethod}
                  />
                  <CardFields
                    ids={cardIds}
                    preview={collect.preview}
                    error={isActive ? collect.loadError : undefined}
                  />
                </div>
              </MethodBody>
            )
          }
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
                <p className="text-muted-foreground text-[13px]">
                  You’ll finish payment on CCBill’s secure page.
                </p>
              </MethodBody>
            )
          }
          return (
            <MethodBody>
              <p className="text-muted-foreground text-[13px]">
                You’ll finish payment on Stripe’s secure page.
              </p>
            </MethodBody>
          )
        }}
      />
      {payError && active && active.rail !== "ccbill" ? (
        <p className="text-destructive text-[13px]" role="alert">
          {payError}
        </p>
      ) : null}
      {active && (active.driver !== "solana_pay" || Boolean(payError)) ? (
        <PayButton
          label={active.driver === "solana_pay" ? "Try again" : payLabel}
          processing={processing}
          disabled={
            active.driver === "collect_js" &&
            !usingSavedMethod &&
            (!collect.ready || Boolean(collect.loadError))
          }
        />
      ) : null}
      <TrustLine />
    </form>
  ) : null

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
