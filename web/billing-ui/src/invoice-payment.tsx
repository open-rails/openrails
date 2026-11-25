// Shows an invoice and pays it, on the host's own page: a saved card, a new card
// in the PSP's own fields (charged and saved in one call), Apple Pay or Google
// Pay through a Stripe PSP (when asked), a Solana wallet through a Solana
// Pay QR, or a PSP's own page. A bank challenge runs in the page, then
// confirm. A fallback PSP (`fallbacks`) waits until the PSPs it stands in for
// fail, then is offered as the way out of a failing card.
// Driven only by the invoice API (GET, pay, confirm), GET /config and GET
// /me/config; never cancels.
import * as React from "react"

import {
  appearanceStyle,
  appearanceTheme,
  type BillingAppearance,
} from "./appearance"
import type { BillingClient, InvoiceResult } from "./client/client"
import { BillingError, isBillingError } from "./client/errors"
import type {
  Invoice,
  InvoicePaymentAttempt,
  InvoicePaymentInput,
  PaymentMethod,
} from "./client/types"
import { PayButton, TrustLine } from "./components/pay-button"
import { Button } from "./components/ui/button"
import { SolanaBody } from "./components/qr"
import { SavedMethods, NEW_CARD_VALUE } from "./components/saved-methods"
import { LoadingSkeleton } from "./components/states"
import {
  StripeCardEntry,
  type StripeCardHandle,
} from "./components/stripe-card"
import { StripeWallets } from "./components/stripe-wallets"
import { useMessages } from "./i18n/context"
import { hiddenFallbacks, type PaymentFallbacks } from "./fallbacks"
import { billingDetailsOf } from "./lib/billing"
import { amountToMinorUnits, formatAmount } from "./lib/money"
import { loadStripeFor } from "./lib/stripe"
import { cardSetupDriver, type PspConfig } from "./psp"
import { useConfig, useCurrencyScales, useCustomerConfig } from "./react/config"
import { useBillingContext } from "./react/context"
import {
  TokenizedCardForm,
  type TokenizedCardData,
} from "./tokenized-card-form"
import { PlatformCardForm } from "./components/platform-card-form"
import type { SavedPaymentMethod } from "./types"
import { cn } from "cn"

export interface InvoicePaymentProps {
  invoiceId: string
  /** The client to pay through; default: the provider's. */
  client?: BillingClient
  /** Where a redirect PSP will return to; card PSPs need none. */
  returnUrl?: string
  /** Offer the customer's saved cards (GET /me/payment-methods). Default true. */
  savedPaymentMethods?: boolean
  /**
   * Offer a PSP whose card entry is server. Default true; invoices take
   * tokens only, so such a PSP is offered for saved cards alone.
   */
  serverCardEntry?: boolean
  /**
   * Offer Apple Pay and Google Pay through a Stripe PSP's Express Checkout
   * Element. The page's host must be a payment-method domain registered on
   * that Stripe account. Default false.
   */
  wallets?: boolean
  /**
   * PSPs kept back until the PSPs they stand in for are not offered, or have
   * failed `afterFailures` times on the invoice; then offered under "Card
   * payment isn't working?". `{ ccbill: { for: ["mobius"], afterFailures: 2 } }`.
   */
  fallbacks?: PaymentFallbacks
  /** Once the invoice is complete, or processing on an asynchronous rail. */
  onSettled?: (invoice: Invoice) => void
  appearance?: BillingAppearance
  className?: string
}

const platformCard = (psp: PspConfig) =>
  psp.custodian === "platform" && psp.rail === "nmi" &&
  psp.flow === "tokenize" && !psp.status && !!psp.config?.public_api_key

const settled = (invoice: Invoice) =>
  invoice.status === "complete" || invoice.status === "processing"

// pageURL is where a hosted payment page returns the customer: this page,
// without its query or fragment, which may hold what a URL must not carry.
const pageURL = () => window.location.origin + window.location.pathname

// A Solana invoice awaiting its wallet is read every SOLANA_READ_MS: OpenRails
// watches the chain itself. Confirm, a purchase request limited per minute,
// asks it to read the chain at once, only every SOLANA_CONFIRM_MS.
const SOLANA_READ_MS = 3000
const SOLANA_CONFIRM_MS = 30000
// A pay whose answer never arrived may still have been taken: these answers
// and a lost one are read back under the same key.
const UNANSWERED = new Set(["idempotency_key_in_use", "idempotency_key_reused", "invoice_payment_in_progress"])
const unanswered = (cause: unknown) =>
  !isBillingError(cause) || cause.type === "local" || cause.status >= 500 || UNANSWERED.has(cause.code)
// An attempt that ended without the payment: the next one takes a new key.
const ENDED = new Set(["failed", "expired", "canceled"])
// How often an invoice whose payment OpenRails is still confirming is read
// again; a lost card answer takes minutes to settle from the provider.
const PROCESSING_POLL_MS = 10000

/** Pays one invoice. Requires `BillingProvider`. */
export function InvoicePayment({
  invoiceId,
  client: given,
  returnUrl,
  savedPaymentMethods = true,
  serverCardEntry = true,
  wallets = false,
  fallbacks,
  onSettled,
  appearance,
  className,
}: InvoicePaymentProps) {
  const m = useMessages()
  const context = useBillingContext()
  const client = given ?? context.client
  const { config } = useConfig()
  const {
    config: merchant,
    error: merchantError,
    refetch: refetchMerchant,
  } = useCustomerConfig()
  const scales = useCurrencyScales()
  const [invoice, setInvoice] = React.useState<Invoice>()
  const [methods, setMethods] = React.useState<PaymentMethod[]>([])
  const [choice, setChoice] = React.useState<string>(NEW_CARD_VALUE)
  const [pspID, setPspID] = React.useState<string>()
  const [error, setError] = React.useState<string>()
  const [busy, setBusy] = React.useState(false)
  const [pendingSaved, setPendingSaved] = React.useState<string>()
  const [walletShown, setWalletShown] = React.useState(false)
  // One key per payment, minted at render. It is kept until the payment it
  // names has an answer: a retry after a lost answer is answered as the
  // first request was, never charged twice. A decline or a refusal mints
  // the next.
  const [key, setKey] = React.useState(() => crypto.randomUUID())
  const stripeCard = React.useRef<StripeCardHandle>(null)
  const settledRef = React.useRef(onSettled)
  React.useEffect(() => {
    settledRef.current = onSettled
  })

  React.useEffect(() => {
    const abort = new AbortController()
    client.getInvoice(invoiceId, abort.signal).then(setInvoice, (cause: unknown) => {
      if (!abort.signal.aborted) setError(m.error(cause))
    })
    return () => abort.abort()
  }, [client, invoiceId, m])

  React.useEffect(() => {
    if (!savedPaymentMethods || invoice?.collection_owner !== "engine") return
    const abort = new AbortController()
    client.listPaymentMethods({ signal: abort.signal }).then(
      (page) => setMethods(page.data),
      () => undefined
    )
    return () => abort.abort()
  }, [client, savedPaymentMethods, invoice?.collection_owner])

  React.useEffect(() => {
    if (invoice && settled(invoice)) settledRef.current?.(invoice)
  }, [invoice])

  // A Solana invoice awaits its wallet until the transfer lands: read it, and
  // now and then ask OpenRails to confirm it.
  const awaitingWallet =
    invoice?.collection_owner === "engine" &&
    invoice?.payment.status === "requires_action" &&
    invoice.payment.next_action?.type === "solana_pay"
  const awaitedInvoice = awaitingWallet ? invoice?.id : undefined
  React.useEffect(() => {
    if (!awaitedInvoice) return
    const abort = new AbortController()
    const confirmEvery = SOLANA_CONFIRM_MS / SOLANA_READ_MS
    // A slow answer never replaces a newer one. Each tab reads on its own:
    // two tabs confirm four times a minute, still under the limit.
    let reads = 0
    let shown = 0
    const timer = window.setInterval(() => {
      const read = ++reads
      const next =
        read % confirmEvery === 0
          ? client.confirmInvoice(awaitedInvoice).then((result) => result.invoice)
          : client.getInvoice(awaitedInvoice, abort.signal)
      next.then(
        (current) => {
          if (abort.signal.aborted || read < shown) return
          shown = read
          setInvoice(current)
        },
        () => undefined
      )
    }, SOLANA_READ_MS)
    return () => {
      window.clearInterval(timer)
      abort.abort()
    }
  }, [awaitedInvoice, client])

  // A processing invoice is confirmed later: show the decline or the receipt
  // once OpenRails has it.
  const confirming =
    invoice?.collection_owner === "engine" && invoice.status === "processing"
  React.useEffect(() => {
    if (!confirming) return
    const abort = new AbortController()
    const timer = window.setInterval(() => {
      client.getInvoice(invoiceId, abort.signal).then(
        (next) => {
          if (next.status === "processing") return
          setInvoice(next)
          // Declined after all: say why, and pay again under a new key.
          if (next.status === "open") {
            const failure = next.payment.last_payment_error?.message
            if (failure) setError(failure)
            setKey(crypto.randomUUID())
          }
        },
        () => undefined
      )
    }, PROCESSING_POLL_MS)
    return () => {
      window.clearInterval(timer)
      abort.abort()
    }
  }, [confirming, client, invoiceId])

  // What the invoice offers here, less the fallbacks still kept back.
  const offered = React.useMemo(
    () => (invoice?.collection_owner === "engine" ? invoice.payment_options : []),
    [invoice]
  )
  const known = React.useMemo(
    () =>
      (merchant?.payment.psps ?? []).filter((psp) =>
        offered.some((option) => option.psp_id === psp.psp_id)
      ),
    [merchant, offered]
  )
  const hidden = hiddenFallbacks(fallbacks, offered, known)
  const options = offered.filter((option) => !hidden.has(option.psp_id))
  const psps = known.filter((psp) => !hidden.has(psp.psp_id))
  // New cards: PSPs whose own fields make a token, where the invoice takes
  // one (a renewal takes saved cards only).
  const cardPsps = psps.filter((psp) => {
    const driver = cardSetupDriver(psp)
    return (
      (driver === "collect_js" || driver === "stripe_elements" || platformCard(psp)) &&
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
        usable.some((psp) =>
          psp.psp_id === method.psp_id || options.some((option) =>
            option.psp_id === psp.psp_id &&
            option.saved_payment_method_ids?.includes(method.id)
          )
        )
    )
    .map((method) => ({
      id: method.id,
      psp_id: method.psp_id ?? "",
      rail: method.rail ?? "",
      card: method.card ?? undefined,
    }))
  const psp: PspConfig | undefined =
    cardPsps.find((item) => item.psp_id === pspID) ?? cardPsps[0]
  const solana = options.find((option) => option.accepts.includes("solana_pay"))
  // A PSP with no fields in this page takes the invoice on its hosted page.
  const redirects = psps.filter(
    (item) =>
      cardSetupDriver(item) === null &&
      options.some(
        (option) =>
          option.psp_id === item.psp_id && option.accepts.includes("redirect")
      )
  )
  const fallsBack = (item: PspConfig) =>
    !!fallbacks &&
    Object.keys(fallbacks).some(
      (key) => key.toLowerCase() === item.key.toLowerCase()
    )
  const hosted = redirects.filter((item) => !fallsBack(item))
  const revealed = redirects.filter(fallsBack)
  const recurring = invoice?.lines.some((line) => !!(line.billing_interval_hours || line.billing_interval_months)) ?? false
  const tokens = solana
    ? (merchant?.payment.solana?.tokens ?? []).filter(
        (token) => !recurring || token.recurring_eligible === true
      )
    : []
  const walletPsp = wallets
    ? cardPsps.find(
        (item) =>
          cardSetupDriver(item) === "stripe_elements" &&
          !!item.config?.publishable_key
      )
    : undefined

  if (!invoice)
    return error ? (
      <p role="alert" className="text-sm text-destructive">
        {error}
      </p>
    ) : (
      <LoadingSkeleton />
    )

  const decimals = scales[invoice.currency.toUpperCase()] ?? 2
  const money = (amount: string) =>
    formatAmount(amount, invoice.currency, decimals)
  const currency = config?.currencies?.find(
    (item) => item.code.toUpperCase() === invoice.currency.toUpperCase()
  )
  // Stripe takes the total in minor units; a total that is not a whole
  // number of them is offered no wallet.
  const walletAmount =
    walletPsp && currency?.minor_decimals != null
      ? amountToMinorUnits(
          invoice.total,
          currency.decimals,
          currency.minor_decimals
        )
      : null

  // answer applies an invoice request's answer: settle, run the bank's
  // challenge, or show why the payment failed.
  const answer = async (result: InvoiceResult): Promise<boolean> => {
    setInvoice(result.invoice)
    if (result.error) {
      setError(
        result.invoice.payment.last_payment_error?.message ?? result.error.message
      )
      setKey(crypto.randomUUID())
      return false
    }
    const next = result.invoice.payment.next_action
    if (result.invoice.collection_owner !== "engine" || result.invoice.payment.status !== "requires_action" || !next) return true
    // The page shows the wallet's request and reads the invoice until it lands.
    if (next.type === "solana_pay") return true
    if (next.type === "redirect_to_url" && next.url) {
      window.location.assign(next.url)
      return true
    }
    const secret = next.payload?.client_secret
    const challenger = merchant?.payment.psps.find(
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
    let confirmed: InvoiceResult
    try {
      confirmed = await client.confirmInvoice(invoice.id)
    } catch (cause) {
      // The challenge passed and only its confirmation was lost: the same
      // payment, under the same key, read again.
      const current = await client.getInvoice(invoice.id).catch(() => undefined)
      if (current) setInvoice(current)
      setError(message(cause))
      return false
    }
    return answer(confirmed)
  }

  const message = (cause: unknown) =>
    !isBillingError(cause) && cause instanceof Error && cause.message
      ? cause.message
      : m.error(cause)

  // recover reads back what a pay under key did when its answer was lost:
  // the invoice as the pay would have answered it, or undefined when nothing
  // was taken under the key or it cannot be read now, and the key stays for
  // the retry.
  const recover = async (): Promise<InvoiceResult | undefined> => {
    let attempt: InvoicePaymentAttempt
    let current: Invoice
    try {
      attempt = await client.getInvoicePaymentAttempt(invoice.id, key)
      current = await client.getInvoice(invoice.id)
    } catch {
      return undefined
    }
    if (!ENDED.has(attempt.state)) return { invoice: current }
    const failure = current.payment.last_payment_error?.message ?? m.t("errors.generic")
    return { invoice: current, error: new BillingError(402, { type: "card_error", code: "payment_failed", message: failure }) }
  }

  // pay answers whether the payment went through (or awaits its rail).
  const pay = async (
    payment: () => Promise<InvoicePaymentInput>,
    preserveUnconfirmed = false,
    reusable?: boolean
  ): Promise<boolean> => {
    if (
      busy ||
      invoice.collection_owner !== "engine" ||
      (pendingSaved && !preserveUnconfirmed)
    )
      return false
    setBusy(true)
    setError(undefined)
    try {
      let result: InvoiceResult | undefined
      try {
        const input = await payment()
        result = await client.payInvoice(invoice.id, {
          payment: input,
          expectedTotal: invoice.total,
          idempotencyKey: key,
          ...(reusable === undefined ? {} : { reusable }),
        })
      } catch (cause) {
        if (isBillingError(cause) && !unanswered(cause)) {
          // Refused before any attempt: corrected, it is a new payment.
          setError(message(cause))
          setKey(crypto.randomUUID())
          return false
        }
        // Lost, or never sent: read back what the key took, if anything.
        result = await recover()
        if (!result) {
          setError(message(cause))
          if (preserveUnconfirmed) throw cause
          return false
        }
      }
      try {
        return await answer(result)
      } catch (cause) {
        // The bank's challenge failed: pay again, as a new payment.
        setError(message(cause))
        setKey(crypto.randomUUID())
        return false
      }
    } finally {
      setBusy(false)
    }
  }

  // A payment the bank asks the customer to authenticate before they came
  // here (a renewal charged while they were away).
  const authenticate = async () => {
    if (busy || invoice.collection_owner !== "engine") return
    setBusy(true)
    setError(undefined)
    try {
      await answer({ invoice })
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
    invoice.collection_owner === "engine" &&
    invoice.status === "open" &&
    invoice.payment.status === "requires_action" &&
    invoice.payment.next_action?.type === "authenticate"
  // Back from a PSP's page before it reported the payment.
  const awaitingRedirect =
    invoice.collection_owner === "engine" &&
    invoice.status === "open" &&
    invoice.payment.status === "requires_action" &&
    invoice.payment.next_action?.type === "redirect_to_url" &&
    !!invoice.payment.next_action.url
  const redirectPsp = merchant?.payment.psps.find(
    (item) => item.psp_id === invoice.payment.next_action?.psp_id
  )

  // A usage invoice is paid for what it still owes; a written-off one too.
  const payLabel = recurring ? "Subscribe" : "Pay"
  const profile = merchant?.merchant
  const terms =
    profile?.terms_url || profile?.privacy_url ? (
      <>
        By {recurring ? "subscribing" : "paying"}, you agree to our{" "}
        {profile.terms_url ? (
          <a href={profile.terms_url} target="_blank" rel="noreferrer" className="underline">
            Terms of Service
          </a>
        ) : null}
        {profile.terms_url && profile.privacy_url ? " and " : null}
        {profile.privacy_url ? (
          <a href={profile.privacy_url} target="_blank" rel="noreferrer" className="underline">
            Privacy Policy
          </a>
        ) : null}
        .
      </>
    ) : undefined
  const open =
    invoice.collection_owner === "engine" &&
    (invoice.status === "open" ||
      (invoice.status === "uncollectible" && !!invoice.usage)) &&
    invoice.payment.status === "requires_payment_method"

  return (
    <div
      className={cn("orck grid gap-5", className)}
      data-orck-theme={appearanceTheme(appearance)}
      style={appearanceStyle(appearance)}
    >
      <div className="grid">
        {invoice.lines.map((line) => (
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
            {money(invoice.total)}
          </span>
        </div>
        {invoice.usage && invoice.usage.amount_paid !== "0" && (
          <div className="flex items-start justify-between gap-4 pt-1 text-[13.5px]">
            <span>Due</span>
            <span className="whitespace-nowrap tabular-nums">
              {money(invoice.usage.amount_due)}
            </span>
          </div>
        )}
      </div>

      {awaitingWallet ? (
        <SolanaBody
          amountLabel={`${invoice.payment.next_action?.payload?.token_amount ?? ""} ${invoice.payment.next_action?.payload?.token_symbol ?? ""}`.trim()}
          payload={invoice.payment.next_action?.url ?? undefined}
          statusLine="Waiting for your wallet's payment"
        />
      ) : invoice.status === "complete" ? (
        <p role="status" className="text-sm font-semibold">
          Payment complete
        </p>
      ) : invoice.collection_owner !== "engine" ? (
        <p role="status" className="text-sm text-muted-foreground">
          This bill is collected by the payment provider. No payment is needed here.
        </p>
      ) : invoice.status === "processing" ? (
        <p role="status" className="text-sm text-muted-foreground">
          We're confirming this payment with the payment provider. You won't be
          charged twice, and this page updates once the provider answers.
        </p>
      ) : invoice.status !== "open" ? (
        <p role="status" className="text-sm text-muted-foreground">
          This invoice can no longer be paid.
        </p>
      ) : awaitingRedirect ? (
        <form
          className="grid gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            window.location.assign(invoice.payment.next_action!.url!)
          }}
        >
          <p className="text-sm text-muted-foreground">
            Your payment is waiting on{" "}
            {redirectPsp?.display_name ?? "the payment page"}.
          </p>
          <PayButton
            label={`Continue to ${redirectPsp?.display_name ?? "the payment page"}`}
            processing={false}
          />
        </form>
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
          {!pendingSaved && walletPsp && walletAmount ? (
            <StripeWallets
              publishableKey={walletPsp.config!.publishable_key!}
              amount={walletAmount}
              currency={invoice.currency}
              onAvailable={setWalletShown}
              onPaymentMethod={(method) =>
                pay(async () => ({ token: method, psp_id: walletPsp.psp_id }))
              }
            />
          ) : null}
          {!pendingSaved && walletShown && walletPsp && walletAmount ? (
            <p className="text-center text-[13px] text-muted-foreground">
              or pay another way
            </p>
          ) : null}
          {saved.length > 0 ? (
            <SavedMethods
              methods={saved}
              value={choice}
              onChange={setChoice}
              disabled={busy || !open || !!pendingSaved}
              idPrefix={`orck-invoice-${invoice.id}`}
            />
          ) : null}
          {choice !== NEW_CARD_VALUE ? (
            <form
              className="grid gap-4"
              onSubmit={(event) => {
                event.preventDefault()
                const method = pendingSaved ?? choice
                const platform = options.some((option) =>
                  option.saved_payment_method_ids?.includes(method)
                )
                void pay(async () => ({ payment_method_id: method }), platform)
                  .then(() => setPendingSaved(undefined))
                  .catch(() => setPendingSaved(method))
              }}
            >
              <PayButton
                label={pendingSaved ? "Retry payment" : payLabel}
                processing={busy}
                disabled={!open}
              />
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
              {platformCard(psp) ? (
                <PlatformCardForm
                  key={psp.psp_id}
                  publicKey={psp.config!.public_api_key!}
                  submitLabel={payLabel}
                  disabled={busy || !open}
                  onTokenized={(card) => pay(async () => ({
                    token: card.payment_token,
                    psp_id: psp.psp_id,
                    billing_details: billingDetailsOf(card),
                  }), true)}
                />
              ) : cardSetupDriver(psp) === "collect_js" ? (
                <TokenizedCardForm
                  key={psp.psp_id}
                  tokenizationKey={psp.config!.tokenization_key!}
                  tokenizationURL={psp.config!.tokenization_url!}
                  submitLabel={payLabel}
                  disabled={busy || !open}
                  appearance={appearance}
                  defaultCountry={merchant?.client_country || undefined}
                  saveOptInMerchant={recurring ? undefined : profile?.display_name}
                  terms={terms}
                  statementDescriptor={profile?.statement_descriptor ?? undefined}
                  // A refused payment leaves the form open for another card.
                  onTokenized={(card: TokenizedCardData) =>
                    pay(
                      async () => ({
                        token: card.payment_token,
                        psp_id: psp.psp_id,
                        billing_details: billingDetailsOf(card),
                      }),
                      false,
                      card.reusable
                    )
                  }
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
          ) : solana || hosted.length > 0 || revealed.length > 0 ? null : !merchant ? (
            // The merchant's payment setup is still loading, or failed to:
            // never read that as card entry being unavailable.
            merchantError ? (
              <div role="alert" className="grid gap-2 text-sm">
                <p className="text-destructive">{m.error(merchantError)}</p>
                <Button type="button" variant="outline" size="sm" onClick={refetchMerchant}>
                  {m.t("common.retry")}
                </Button>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">{m.t("common.loading")}</p>
            )
          ) : (
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
                  return_url: returnUrl ?? pageURL(),
                  psp_id: item.psp_id,
                }))
              }}
            >
              <PayButton
                label={`${payLabel} with ${item.display_name}`}
                processing={busy}
                disabled={!open || !!pendingSaved}
              />
            </form>
          ))}
          {revealed.length > 0 ? (
            <div className="grid gap-2 border-t border-border pt-4">
              <p className="text-[13px] text-muted-foreground">
                Card payment isn&apos;t working?
              </p>
              {revealed.map((item) => {
                // A CCBill page names the customer; one it cannot name yet
                // says what to add first.
                const requires =
                  options.find((option) => option.psp_id === item.psp_id)
                    ?.requires ?? []
                const blocked = requires.length > 0
                const reason = `${item.psp_id}-requires`
                return (
                  <React.Fragment key={item.psp_id}>
                    <button
                      type="button"
                      className="h-9 rounded-md border border-border text-sm disabled:cursor-not-allowed disabled:opacity-60"
                      disabled={busy || !open || !!pendingSaved || blocked}
                      aria-describedby={blocked ? reason : undefined}
                      onClick={() =>
                        void pay(async () => ({
                          return_url: returnUrl ?? pageURL(),
                          psp_id: item.psp_id,
                        }))
                      }
                    >
                      Pay with {item.display_name}
                    </button>
                    {blocked ? (
                      <p id={reason} className="text-[13px] text-muted-foreground">
                        {m.t(
                          requires.length > 1
                            ? "errors.customer_email_required"
                            : requires[0] === "username"
                              ? "errors.ccbill_username_required"
                              : "errors.ccbill_email_required"
                        )}
                      </p>
                    ) : null}
                  </React.Fragment>
                )
              })}
            </div>
          ) : null}
          {solana && tokens.length > 0 ? (
            <div className="grid gap-2 border-t border-border pt-4">
              {tokens.map((token) => (
                <button
                  key={token.symbol}
                  type="button"
                  className="h-9 rounded-md border border-border text-sm"
                  disabled={busy || !open || !!pendingSaved}
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
