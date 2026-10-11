// Shows an order and pays it, on the host's own page: a saved card, a new card
// in the PSP's own fields (charged and saved in one call), Apple Pay or Google
// Pay through a Stripe PSP (when asked), or a Solana wallet through a Solana
// Pay QR. A bank challenge runs in the page, then confirm.
// Driven only by the order API (GET, pay, confirm) and GET /config; never
// cancels.
import * as React from "react"

import {
  appearanceStyle,
  appearanceTheme,
  type CheckoutAppearance,
} from "./appearance"
import type { BillingClient, OrderResult } from "./client/client"
import { isBillingError } from "./client/errors"
import type { Order, OrderPaymentInput, PaymentMethod } from "./client/types"
import { PayButton, TrustLine } from "./components/pay-button"
import { SolanaBody } from "./components/qr"
import { SavedMethods, NEW_CARD_VALUE } from "./components/saved-methods"
import { LoadingSkeleton } from "./components/states"
import {
  StripeCardEntry,
  type StripeCardHandle,
} from "./components/stripe-card"
import { StripeWallets } from "./components/stripe-wallets"
import { useMessages } from "./i18n/context"
import { billingDetailsOf } from "./lib/billing"
import { amountToMinorUnits, formatAmount } from "./lib/money"
import { loadStripeFor } from "./lib/stripe"
import { cardSetupDriver, type PspConfig } from "./psp"
import { useConfig, useCurrencyScales } from "./react/config"
import { useBillingContext } from "./react/context"
import {
  TokenizedCardForm,
  type TokenizedCardData,
} from "./tokenized-card-form"
import type { SavedPaymentMethod } from "./types"
import { cn } from "cn"

export interface OrderCheckoutProps {
  orderId: string
  /** The client to pay through; default: the provider's. */
  client?: BillingClient
  /** Where a redirect PSP will return to; card PSPs need none. */
  returnUrl?: string
  /** Offer the customer's saved cards (GET /me/payment-methods). Default true. */
  savedPaymentMethods?: boolean
  /**
   * Offer a PSP whose card entry is server. Default true; orders take
   * tokens only, so such a PSP is offered for saved cards alone.
   */
  serverCardEntry?: boolean
  /**
   * Offer Apple Pay and Google Pay through a Stripe PSP's Express Checkout
   * Element. The page's host must be a payment-method domain registered on
   * that Stripe account. Default false.
   */
  wallets?: boolean
  /** Once the order is complete, or processing on an asynchronous rail. */
  onSettled?: (order: Order) => void
  appearance?: CheckoutAppearance
  className?: string
}

const settled = (order: Order) =>
  order.status === "complete" || order.status === "processing"

// How often a Solana order awaiting its wallet is read again.
const SOLANA_POLL_MS = 3000

/** Pays one order. Requires `BillingProvider`. */
export function OrderCheckout({
  orderId,
  client: given,
  returnUrl,
  savedPaymentMethods = true,
  serverCardEntry = true,
  wallets = false,
  onSettled,
  appearance,
  className,
}: OrderCheckoutProps) {
  const m = useMessages()
  const context = useBillingContext()
  const client = given ?? context.client
  const { config } = useConfig()
  const scales = useCurrencyScales()
  const [order, setOrder] = React.useState<Order>()
  const [methods, setMethods] = React.useState<PaymentMethod[]>([])
  const [choice, setChoice] = React.useState<string>(NEW_CARD_VALUE)
  const [pspID, setPspID] = React.useState<string>()
  const [error, setError] = React.useState<string>()
  const [busy, setBusy] = React.useState(false)
  const [walletShown, setWalletShown] = React.useState(false)
  // One key per pay attempt, minted at render: a retry of the same request
  // answers what the first did. A failed answer mints the next.
  const [key, setKey] = React.useState(() => crypto.randomUUID())
  const stripeCard = React.useRef<StripeCardHandle>(null)
  const settledRef = React.useRef(onSettled)
  React.useEffect(() => {
    settledRef.current = onSettled
  })

  React.useEffect(() => {
    const abort = new AbortController()
    client.getOrder(orderId, abort.signal).then(setOrder, (cause: unknown) => {
      if (!abort.signal.aborted) setError(m.error(cause))
    })
    return () => abort.abort()
  }, [client, orderId, m])

  React.useEffect(() => {
    if (!savedPaymentMethods) return
    const abort = new AbortController()
    client.listPaymentMethods({ signal: abort.signal }).then(
      (page) => setMethods(page.data),
      () => undefined
    )
    return () => abort.abort()
  }, [client, savedPaymentMethods])

  React.useEffect(() => {
    if (order && settled(order)) settledRef.current?.(order)
  }, [order])

  // A Solana order awaits its wallet: confirm asks OpenRails to read the
  // chain, until the transfer lands.
  const awaitingWallet =
    order?.payment.status === "requires_action" &&
    order.payment.next_action?.type === "solana_pay"
  React.useEffect(() => {
    if (!awaitingWallet || !order) return
    const timer = window.setInterval(() => {
      client.confirmOrder(order.id).then(
        (result) => setOrder(result.order),
        () => undefined
      )
    }, SOLANA_POLL_MS)
    return () => window.clearInterval(timer)
  }, [awaitingWallet, client, order])

  const options = React.useMemo(() => order?.payment_options ?? [], [order])
  const psps = React.useMemo(
    () =>
      (config?.payment?.psps ?? []).filter((psp) =>
        options.some((option) => option.psp_id === psp.psp_id)
      ),
    [config, options]
  )
  // New cards: PSPs whose own fields make a token, where the order takes
  // one (a renewal takes saved cards only).
  const cardPsps = psps.filter((psp) => {
    const driver = cardSetupDriver(psp)
    return (
      (driver === "collect_js" || driver === "stripe_elements") &&
      options.some(
        (option) =>
          option.psp_id === psp.psp_id && option.accepts.includes("new_card")
      )
    )
  })
  const usable = psps.filter(
    (psp) => serverCardEntry || cardSetupDriver(psp) !== "card"
  )
  const saved: SavedPaymentMethod[] = methods
    .filter(
      (method) =>
        (method.status ?? "active") === "active" &&
        method.reusable !== false &&
        method.health?.active !== false &&
        usable.some((psp) => psp.psp_id === method.psp_id)
    )
    .map((method) => ({
      id: method.id,
      option_id: method.psp_id ?? "",
      rail: method.rail ?? "",
      card: method.card ?? undefined,
    }))
  const psp: PspConfig | undefined =
    cardPsps.find((item) => item.psp_id === pspID) ?? cardPsps[0]
  const solana = options.find((option) => option.accepts.includes("solana_pay"))
  // A PSP with no fields in this page takes the order on its hosted page.
  const hosted = psps.filter(
    (item) =>
      cardSetupDriver(item) === null &&
      options.some(
        (option) =>
          option.psp_id === item.psp_id && option.accepts.includes("redirect")
      )
  )
  const tokens = solana ? (config?.payment?.solana?.tokens ?? []) : []
  const walletPsp = wallets
    ? cardPsps.find(
        (item) =>
          cardSetupDriver(item) === "stripe_elements" &&
          !!item.config?.publishable_key
      )
    : undefined

  if (!order)
    return error ? (
      <p role="alert" className="text-sm text-destructive">
        {error}
      </p>
    ) : (
      <LoadingSkeleton />
    )

  const decimals = scales[order.currency.toUpperCase()] ?? 2
  const money = (amount: string) =>
    formatAmount(amount, order.currency, decimals)
  const currency = config?.currencies?.find(
    (item) => item.code.toUpperCase() === order.currency.toUpperCase()
  )
  // Stripe takes the total in minor units; a total that is not a whole
  // number of them is offered no wallet.
  const walletAmount =
    walletPsp && currency?.minor_decimals != null
      ? amountToMinorUnits(
          order.total,
          currency.decimals,
          currency.minor_decimals
        )
      : null

  // answer applies an order request's answer: settle, run the bank's
  // challenge, or show why the payment failed.
  const answer = async (result: OrderResult): Promise<boolean> => {
    setOrder(result.order)
    if (result.error) {
      setError(
        result.order.payment.last_payment_error?.message ?? result.error.message
      )
      setKey(crypto.randomUUID())
      return false
    }
    const next = result.order.payment.next_action
    if (result.order.payment.status !== "requires_action" || !next) return true
    if (next.type === "redirect_to_url" && next.url) {
      window.location.assign(next.url)
      return true
    }
    const secret = next.payload?.client_secret
    const challenger = config?.payment?.psps.find(
      (item) => item.psp_id === next.psp_id
    )
    const publishable = challenger?.config?.publishable_key
    if (next.type !== "authenticate" || !secret || !publishable) {
      setError(m.t("paymentMethods.unavailable"))
      return false
    }
    if (stripeCard.current && next.psp_id === psp?.psp_id)
      await stripeCard.current.handleNextAction(secret)
    else {
      const stripe = await loadStripeFor(publishable)
      const answered = await stripe.handleNextAction({ clientSecret: secret })
      if (answered.error) throw new Error(answered.error.message)
    }
    return answer(await client.confirmOrder(order.id))
  }

  // pay answers whether the payment went through (or awaits its rail).
  const pay = async (
    payment: () => Promise<OrderPaymentInput>
  ): Promise<boolean> => {
    if (busy) return false
    setBusy(true)
    setError(undefined)
    try {
      return await answer(
        await client.payOrder(order.id, {
          payment: await payment(),
          expectedTotal: order.total,
          idempotencyKey: key,
        })
      )
    } catch (cause) {
      setError(
        !isBillingError(cause) && cause instanceof Error && cause.message
          ? cause.message
          : m.error(cause)
      )
      setKey(crypto.randomUUID())
      return false
    } finally {
      setBusy(false)
    }
  }

  // A payment the bank asks the customer to authenticate before they came
  // here (a renewal charged while they were away).
  const authenticate = async () => {
    if (busy) return
    setBusy(true)
    setError(undefined)
    try {
      await answer({ order })
    } catch (cause) {
      setError(
        !isBillingError(cause) && cause instanceof Error && cause.message
          ? cause.message
          : m.error(cause)
      )
    } finally {
      setBusy(false)
    }
  }
  const awaitingAuthentication =
    order.status === "open" &&
    order.payment.status === "requires_action" &&
    order.payment.next_action?.type === "authenticate"

  const payLabel = `Pay ${money(order.total)}`
  const open =
    order.status === "open" &&
    order.payment.status === "requires_payment_method"

  return (
    <div
      className={cn("orck grid gap-5", className)}
      data-orck-theme={appearanceTheme(appearance)}
      style={appearanceStyle(appearance)}
    >
      <div className="grid">
        {order.lines.map((line) => (
          <div
            key={line.id}
            className="flex items-start justify-between gap-4 py-2 text-[13.5px]"
          >
            <span>
              {line.description}
              {line.quantity && line.quantity > 1 ? ` × ${line.quantity}` : ""}
            </span>
            <span className="whitespace-nowrap tabular-nums">
              {money(line.amount)}
            </span>
          </div>
        ))}
        <div className="flex items-start justify-between gap-4 border-t border-border pt-3 text-[13.5px] font-semibold">
          <span>Total</span>
          <span className="whitespace-nowrap tabular-nums">
            {money(order.total)}
          </span>
        </div>
      </div>

      {awaitingWallet ? (
        <SolanaBody
          amountLabel={`${order.payment.next_action?.payload?.token_amount ?? ""} ${order.payment.next_action?.payload?.token_symbol ?? ""}`.trim()}
          payload={order.payment.next_action?.url ?? undefined}
          statusLine="Waiting for your wallet's payment"
        />
      ) : order.status === "complete" ? (
        <p role="status" className="text-sm font-semibold">
          Payment complete
        </p>
      ) : order.status === "processing" ? (
        <p role="status" className="text-sm text-muted-foreground">
          Your payment is processing.
        </p>
      ) : order.status !== "open" ? (
        <p role="status" className="text-sm text-muted-foreground">
          This order can no longer be paid.
        </p>
      ) : awaitingAuthentication ? (
        <form
          className="grid gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            void authenticate()
          }}
        >
          <p className="text-sm text-muted-foreground">
            Your bank asks you to confirm this payment.
          </p>
          <PayButton label="Confirm payment" processing={busy} />
          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          ) : null}
        </form>
      ) : (
        <div className="grid gap-4">
          {walletPsp && walletAmount ? (
            <StripeWallets
              publishableKey={walletPsp.config!.publishable_key!}
              amount={walletAmount}
              currency={order.currency}
              onAvailable={setWalletShown}
              onPaymentMethod={(method) =>
                pay(async () => ({ token: method, psp_id: walletPsp.psp_id }))
              }
            />
          ) : null}
          {walletShown && walletPsp && walletAmount ? (
            <p className="text-center text-[13px] text-muted-foreground">
              or pay another way
            </p>
          ) : null}
          {saved.length > 0 ? (
            <SavedMethods
              methods={saved}
              value={choice}
              onChange={setChoice}
              disabled={busy || !open}
              idPrefix={`orck-order-${order.id}`}
            />
          ) : null}
          {choice !== NEW_CARD_VALUE ? (
            <form
              className="grid gap-4"
              onSubmit={(event) => {
                event.preventDefault()
                void pay(async () => ({ payment_method_id: choice }))
              }}
            >
              <PayButton label={payLabel} processing={busy} disabled={!open} />
            </form>
          ) : psp ? (
            <>
              {cardPsps.length > 1 ? (
                <select
                  aria-label="Pay with"
                  className="h-9 rounded-md border border-border bg-transparent px-2 text-sm"
                  value={psp.psp_id}
                  onChange={(event) => setPspID(event.target.value)}
                >
                  {cardPsps.map((item) => (
                    <option key={item.psp_id} value={item.psp_id}>
                      {item.display_name}
                    </option>
                  ))}
                </select>
              ) : null}
              {cardSetupDriver(psp) === "collect_js" ? (
                <TokenizedCardForm
                  key={psp.psp_id}
                  tokenizationKey={psp.config!.tokenization_key!}
                  tokenizationURL={psp.config!.tokenization_url!}
                  submitLabel={payLabel}
                  disabled={busy || !open}
                  appearance={appearance}
                  onTokenized={async (card: TokenizedCardData) => {
                    await pay(async () => ({
                      token: card.payment_token,
                      psp_id: psp.psp_id,
                      billing_details: billingDetailsOf(card),
                    }))
                  }}
                />
              ) : (
                <form
                  className="grid gap-4"
                  onSubmit={(event) => {
                    event.preventDefault()
                    void pay(async () => ({
                      token: await stripeCard.current!.tokenize(),
                      psp_id: psp.psp_id,
                    }))
                  }}
                >
                  <StripeCardEntry
                    key={psp.psp_id}
                    ref={stripeCard}
                    psp={psp}
                    client={client}
                    unavailableMessage={m.t("paymentMethods.unavailable")}
                    verificationPendingMessage={m.t(
                      "paymentMethods.verificationPending"
                    )}
                  />
                  <PayButton
                    label={payLabel}
                    processing={busy}
                    disabled={!open}
                  />
                  <TrustLine />
                </form>
              )}
            </>
          ) : solana || hosted.length > 0 ? null : (
            <p role="alert" className="text-sm text-destructive">
              {m.t("paymentMethods.unavailable")}
            </p>
          )}
          {hosted.map((item) => (
            <form
              key={item.psp_id}
              onSubmit={(event) => {
                event.preventDefault()
                void pay(async () => ({
                  return_url: returnUrl ?? window.location.href,
                  psp_id: item.psp_id,
                }))
              }}
            >
              <PayButton
                label={`${payLabel} with ${item.display_name}`}
                processing={busy}
                disabled={!open}
              />
            </form>
          ))}
          {solana && tokens.length > 0 ? (
            <div className="grid gap-2 border-t border-border pt-4">
              {tokens.map((token) => (
                <button
                  key={token.symbol}
                  type="button"
                  className="h-9 rounded-md border border-border text-sm"
                  disabled={busy || !open}
                  onClick={() =>
                    void pay(async () => ({
                      token_symbol: token.symbol,
                      psp_id: solana.psp_id,
                    }))
                  }
                >
                  Pay with {token.symbol} on Solana
                </button>
              ))}
            </div>
          ) : null}
          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          ) : null}
        </div>
      )}
    </div>
  )
}
