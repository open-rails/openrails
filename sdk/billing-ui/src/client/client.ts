import type { z } from "zod"

import type { CheckoutSource } from "../source"
import { registerCheckout, type CheckoutSourceOptions } from "./checkout"
import {
  checkoutSessionSchema,
  payResultSchema,
  type PayRequest,
  type PayResult,
} from "../types"
import {
  BillingError,
  isBillingError,
  localError,
  readBillingError,
} from "./errors"
import { OPENRAILS_CURRENCY_SCALES } from "./generated/currencies"
import type * as wire from "./generated/wire"
import {
  accountSchema,
  checkoutSessionLinkSchema,
  publicConfigSchema,
  paymentAuthenticationSchema,
  invoiceSchema,
  listPageSchema,
  pageSchema,
  paymentMethodSchema,
  paymentSchema,
  productSchema,
  solanaTokensSchema,
  subscriptionSchema,
  subscriptionChangePreviewSchema,
  subscriptionChangeSchema,
  type Account,
  type CheckoutSessionLink,
  type PublicConfig,
  type PaymentAuthentication,
  type CurrencyScales,
  type Invoice,
  type ListPage,
  type NewCard,
  type NextAction,
  type Page,
  type Payment,
  type PaymentMethod,
  type Product,
  type SolanaToken,
  type Subscription,
  type SubscriptionChange,
  type SubscriptionChangePreview,
} from "./types"

export interface BillingClientOptions {
  /** OpenRails embedded mount. Default "/billing/v1". */
  baseUrl?: string
  /**
   * Transport. Pass an authenticating fetch (auth-ui's `client.authFetch`)
   * or combine the default with `getToken`.
   */
  fetch?: (input: string, init: RequestInit) => Promise<Response>
  /** Bearer for each request; omit when `fetch` authenticates. */
  getToken?: () =>
    string | null | undefined | Promise<string | null | undefined>
  /** Sent as Accept-Language. */
  language?: () => string | null | undefined
  /**
   * Extra or overriding currency scales (code -> native-unit decimals). The
   * pinned OpenRails registry is built in; `/me` amounts carry no scale.
   */
  currencies?: CurrencyScales
}

/**
 * A public catalog lookup: the products named by `keys` (at most 100 product
 * keys), in one page.
 */
export interface ProductListOptions {
  keys: string[]
  signal?: AbortSignal
}

export interface ListOptions {
  limit?: number
  offset?: number
  /** A cursor list's next page: the previous page's `next_cursor`. */
  cursor?: string
  signal?: AbortSignal
}

/**
 * A subscription change: another price of its tier group, other seats of a
 * per-seat price, or both. A quantity for a price that is not per seat is
 * refused (`quantity_not_allowed`).
 */
export interface SubscriptionChangeInput {
  priceId?: string
  quantity?: number
}

export interface CursorOptions {
  limit?: number
  cursor?: string | null
  signal?: AbortSignal
}

/**
 * Signs and submits a base64 transaction with the customer's wallet and
 * returns its signature. Throw `WalletRejectedError` when the user declines.
 */
export type SendSolanaTransaction = (
  transactionBase64: string
) => Promise<string>

export class WalletRejectedError extends Error {
  constructor(message = "The wallet request was declined.") {
    super(message)
    this.name = "WalletRejectedError"
  }
}

export const CANCEL_REASON_MIN = 4
export const CANCEL_REASON_MAX = 500

/** A next action the customer's wallet completes. */
export const isWalletAction = (next: NextAction | null | undefined): boolean =>
  next?.type === "solana_sign_transactions" && !!next.transactions?.length

/**
 * Signs and sends a `solana_sign_transactions` next action's transactions in
 * order and returns the last signature, the value the action is repeated
 * with. A declined prompt is a local `wallet_rejected` error.
 */
export async function signWalletAction(
  next: NextAction,
  sendTransaction: SendSolanaTransaction
): Promise<string> {
  let signature = ""
  for (const transaction of next.transactions ?? []) {
    try {
      signature = await sendTransaction(transaction)
    } catch (err) {
      if (isWalletRejection(err))
        throw localError("wallet_rejected", "The wallet request was declined.")
      throw err
    }
    if (!signature)
      throw localError(
        "wallet_no_signature",
        "The wallet returned no signature."
      )
  }
  if (!signature)
    throw localError("wallet_no_signature", "The wallet returned no signature.")
  return signature
}

type Query = Record<string, string | number | boolean | string[] | undefined>

interface RequestOptions {
  method?: string
  query?: Query
  body?: unknown
  signal?: AbortSignal
  headers?: Record<string, string>
  /** Sends no bearer: the hosted checkout session id is the credential. */
  anonymous?: boolean
  /** Replaces `baseUrl` for this request. */
  root?: string
}

/** Total wait a GET may spend on its one retry. */
export const RETRY_BUDGET_MS = 2_000
const RETRY_DELAY_MS = 300

// The wait before retrying a GET answered with res, or null if it is final.
function retryDelay(res: Response): number | null {
  const retryAfter = parseRetryAfter(res.headers.get("Retry-After"))
  switch (res.status) {
    case 429:
      return retryAfter
    case 503:
      return retryAfter ?? RETRY_DELAY_MS
    case 502:
    case 504:
      return RETRY_DELAY_MS
    default:
      return null
  }
}

function parseRetryAfter(value: string | null): number | null {
  if (!value) return null
  if (/^\d+$/.test(value.trim())) return Number(value.trim()) * 1000
  const at = Date.parse(value)
  return Number.isNaN(at) ? null : Math.max(0, at - Date.now())
}

function pause(ms: number, signal?: AbortSignal): Promise<number> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason)
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", abort)
      resolve(ms)
    }, ms)
    const abort = () => {
      clearTimeout(timer)
      reject(signal?.reason)
    }
    signal?.addEventListener("abort", abort, { once: true })
  })
}

export function createBillingClient(options: BillingClientOptions = {}) {
  const base = (options.baseUrl ?? "/billing/v1").replace(/\/+$/, "")
  const doFetch =
    options.fetch ?? ((input: string, init: RequestInit) => fetch(input, init))
  const currencies = normalizeScales({
    ...OPENRAILS_CURRENCY_SCALES,
    ...options.currencies,
  })

  function url(path: string, query?: Query, root = base): string {
    const qs = new URLSearchParams()
    for (const [k, v] of Object.entries(query ?? {}))
      if (Array.isArray(v)) for (const item of v) qs.append(k, item)
      else if (v !== undefined && v !== "") qs.set(k, String(v))
    const q = qs.toString()
    return `${root}${path}${q ? `?${q}` : ""}`
  }

  async function send(
    path: string,
    opts: RequestOptions = {}
  ): Promise<Response> {
    const headers = new Headers({ Accept: "application/json", ...opts.headers })
    if (opts.body !== undefined) headers.set("Content-Type", "application/json")
    const token = opts.anonymous ? undefined : await options.getToken?.()
    if (token) headers.set("Authorization", `Bearer ${token}`)
    const lang = options.language?.()
    if (lang) headers.set("Accept-Language", lang)
    const method = opts.method ?? "GET"
    const init = {
      method,
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      signal: opts.signal,
    }
    // Only a GET is retried: once, on a network error or 502/503/504 (and a
    // 429 that names its wait), within RETRY_BUDGET_MS. Writes never are.
    const retries = method === "GET" ? 1 : 0
    let waited = 0
    for (let attempt = 0; ; attempt++) {
      let res: Response
      try {
        res = await doFetch(url(path, opts.query, opts.root), init)
      } catch (cause) {
        const delay = RETRY_DELAY_MS
        if (
          attempt >= retries ||
          opts.signal?.aborted ||
          !(cause instanceof TypeError) ||
          waited + delay > RETRY_BUDGET_MS
        )
          throw cause
        waited += await pause(delay, opts.signal)
        continue
      }
      if (res.ok) return res
      const delay = retryDelay(res)
      if (
        attempt >= retries ||
        delay === null ||
        waited + delay > RETRY_BUDGET_MS
      )
        throw await readBillingError(res)
      await res.body?.cancel().catch(() => undefined)
      waited += await pause(delay, opts.signal)
    }
  }

  async function json<S extends z.ZodType>(
    schema: S,
    path: string,
    opts?: RequestOptions
  ): Promise<z.output<S>> {
    const res = await send(path, opts)
    let body: unknown
    try {
      body = await res.json()
    } catch {
      throw localError("invalid_response", `${path}: body is not JSON`)
    }
    const parsed = schema.safeParse(body)
    if (!parsed.success)
      throw localError(
        "invalid_response",
        `${path}: ${parsed.error.issues[0]?.path.join(".")} ${parsed.error.issues[0]?.message}`
      )
    return parsed.data
  }

  const id = (value: string) => encodeURIComponent(value)
  const subscriptionPage = listPageSchema(subscriptionSchema)
  const methodPage = listPageSchema(paymentMethodSchema)
  const paymentPage = listPageSchema(paymentSchema)
  const invoicePage = listPageSchema(invoiceSchema)
  const cursorQuery = (opts: CursorOptions, limit: number) => ({
    limit: opts.limit ?? limit,
    cursor: opts.cursor ?? undefined,
  })
  const productPage = pageSchema(productSchema)

  const client = {
    baseUrl: base,

    /** One page of the customer's subscriptions, newest first. */
    listSubscriptions(
      opts: CursorOptions & { status?: string } = {}
    ): Promise<ListPage<Subscription>> {
      return json(subscriptionPage, "/me/subscriptions", {
        query: { status: opts.status ?? "all", ...cursorQuery(opts, 100) },
        signal: opts.signal,
      })
    },

    /** Includes `recovery` (list rows do not). */
    getSubscription(
      subscriptionId: string,
      signal?: AbortSignal
    ): Promise<Subscription> {
      return json(
        subscriptionSchema,
        `/me/subscriptions/${id(subscriptionId)}`,
        {
          signal,
        }
      )
    },

    /**
     * Cancels at period end and answers the subscription. When the rail needs
     * the customer's step (`next_action`, a Solana wallet signature), the
     * subscription is unchanged: complete it with `signWalletAction` and
     * repeat with `signature`.
     */
    cancelSubscription(
      subscriptionId: string,
      input: { reason: string; signature?: string }
    ): Promise<Subscription> {
      return json(
        subscriptionSchema,
        `/me/subscriptions/${id(subscriptionId)}/cancel`,
        {
          method: "POST",
          body: {
            reason: input.reason.trim(),
            signature: input.signature,
          } satisfies wire.CustomerCancelSubscriptionParams,
        }
      )
    },

    /** Undoes a scheduled cancel before the period ends. */
    resumeSubscription(subscriptionId: string): Promise<Subscription> {
      return json(
        subscriptionSchema,
        `/me/subscriptions/${id(subscriptionId)}/resume`,
        { method: "POST", body: {} }
      )
    },

    /** Gives the subscription its own card; `null` makes it follow the default. */
    setSubscriptionPaymentMethod(
      subscriptionId: string,
      paymentMethodId: string | null
    ): Promise<Subscription> {
      return json(
        subscriptionSchema,
        `/me/subscriptions/${id(subscriptionId)}/payment-method`,
        {
          method: "PUT",
          body: {
            payment_method_id: paymentMethodId,
          } satisfies wire.SetSubscriptionPaymentMethodParams,
        }
      )
    },

    /** What `changeSubscription` would charge now and at the next renewal. */
    previewSubscriptionChange(
      subscriptionId: string,
      change: SubscriptionChangeInput,
      signal?: AbortSignal
    ): Promise<SubscriptionChangePreview> {
      return json(
        subscriptionChangePreviewSchema,
        `/me/subscriptions/${id(subscriptionId)}/change/preview`,
        {
          method: "POST",
          body: {
            price_id: change.priceId,
            quantity: change.quantity,
          } satisfies wire.PreviewSubscriptionChangeParams,
          signal,
        }
      )
    },

    /**
     * Moves the subscription to another price of its tier group, to other
     * seats of a per-seat price, or both. An upgrade and more seats charge
     * the saved card now; a downgrade and fewer seats apply at period end.
     * Reuse `idempotencyKey` until the change resolves (`processing`, a
     * `subscription_change_in_flight` refusal or a lost response) so the
     * stored result replays instead of charging twice. A Solana wallet next
     * action is completed with `signWalletAction`, then repeated with `signature`.
     */
    changeSubscription(
      subscriptionId: string,
      input: SubscriptionChangeInput & {
        idempotencyKey: string
        signature?: string
      }
    ): Promise<SubscriptionChange> {
      return json(
        subscriptionChangeSchema,
        `/me/subscriptions/${id(subscriptionId)}/change`,
        {
          method: "POST",
          body: {
            price_id: input.priceId,
            quantity: input.quantity,
            signature: input.signature,
          } satisfies wire.CustomerChangeSubscriptionParams,
          headers: { "Idempotency-Key": input.idempotencyKey },
        }
      )
    },

    listPaymentMethods(
      opts: CursorOptions = {}
    ): Promise<ListPage<PaymentMethod>> {
      return json(methodPage, "/me/payment-methods", {
        query: cursorQuery(opts, 100),
        signal: opts.signal,
      })
    },

    /**
     * Saves a card entered in the page with the PSP `psp_id` in one call: the
     * token of its own fields, or the card itself ("card"). A Stripe card the
     * bank wants authenticated answers `requires_action`.
     */
    addPaymentMethod(card: NewCard): Promise<PaymentMethod> {
      return json(paymentMethodSchema, "/me/payment-methods", {
        method: "POST",
        body: card,
      })
    },

    /**
     * Finishes saving a card the bank asked to authenticate, after the PSP's
     * script answered its `next_action`.
     */
    confirmPaymentMethod(paymentMethodId: string): Promise<PaymentMethod> {
      return json(
        paymentMethodSchema,
        `/me/payment-methods/${id(paymentMethodId)}/confirm`,
        { method: "POST", body: {} }
      )
    },

    /** The provider challenge a pending payment operation is waiting on. */
    getPaymentAuthentication(
      operationId: string
    ): Promise<PaymentAuthentication> {
      return json(
        paymentAuthenticationSchema,
        `/me/payment-operations/${id(operationId)}/authentication`
      )
    },

    async confirmPaymentAuthentication(operationId: string): Promise<void> {
      await send(
        `/me/payment-operations/${id(operationId)}/authentication/confirm`,
        { method: "POST", body: {} }
      )
    },

    /** `pending` when the provider is still converging (202). */
    async removePaymentMethod(
      paymentMethodId: string
    ): Promise<"removed" | "pending"> {
      const res = await send(`/me/payment-methods/${id(paymentMethodId)}`, {
        method: "DELETE",
      })
      return res.status === 202 ? "pending" : "removed"
    },

    /**
     * Makes the card the default for one currency: it pays the currency's
     * invoices and every subscription in it without its own card.
     */
    async setDefaultPaymentMethod(input: {
      currency: string
      paymentMethodId: string
    }): Promise<void> {
      await send(
        `/me/default-payment-methods/${id(input.currency.toUpperCase())}`,
        {
          method: "PUT",
          body: {
            payment_method_id: input.paymentMethodId,
          } satisfies wire.SetDefaultPaymentMethodParams,
        }
      )
    },

    /** Charges and refunds, newest first. `rail` filters by rail. */
    listPayments(
      opts: CursorOptions & { rail?: string } = {}
    ): Promise<ListPage<Payment>> {
      return json(paymentPage, "/me/payments", {
        query: { ...cursorQuery(opts, 20), rail: opts.rail },
        signal: opts.signal,
      })
    },

    listInvoices(opts: CursorOptions = {}): Promise<ListPage<Invoice>> {
      return json(invoicePage, "/me/invoices", {
        query: cursorQuery(opts, 20),
        signal: opts.signal,
      })
    },

    /**
     * The customer's summary: balance and amount owed per currency, the card
     * that pays each currency, and unread notices.
     */
    getAccount(signal?: AbortSignal): Promise<Account> {
      return json(accountSchema, "/me", { signal })
    },

    getInvoice(invoiceId: string, signal?: AbortSignal): Promise<Invoice> {
      return json(invoiceSchema, `/me/invoices/${id(invoiceId)}`, { signal })
    },

    /**
     * The products on sale named by `keys`, each with its current prices.
     * No keys name no products: nothing is fetched.
     */
    async listProducts(opts: ProductListOptions): Promise<Page<Product>> {
      if (opts.keys.length === 0) return { data: [], next_cursor: null }
      return json(productPage, "/catalog/products", {
        query: { keys: opts.keys },
        signal: opts.signal,
      })
    },

    /**
     * The deployment's public configuration: what the mount serves, the
     * currency registry and the merchant's payment setup. React components
     * share one copy through `useConfig()`.
     */
    getConfig(signal?: AbortSignal): Promise<PublicConfig> {
      return json(publicConfigSchema, "/config", { signal, anonymous: true })
    },

    /**
     * Accepted tokens with live prices. `priceId` adds each token's `quote`;
     * `wallet` adds its `balance`.
     */
    async listSolanaTokens(
      opts: {
        priceId?: string
        wallet?: string
        signal?: AbortSignal
      } = {}
    ): Promise<SolanaToken[]> {
      const { tokens } = await json(solanaTokensSchema, "/solana/tokens", {
        query: {
          price_id: opts.priceId,
          wallet: opts.wallet,
        },
        signal: opts.signal,
      })
      return tokens
    },

    /** Currency code (upper case) to native-unit decimals. */
    currencies,
  }
  // The purchase calls stay billing-ui's own: apps buy through <BuyButton>.
  registerCheckout(client, {
    /**
     * Mints a hosted checkout session for the signed-in customer and one
     * price; BuyButton renders it.
     * Select a price by `priceId`, or by both `productKey` and `priceKey`.
     * `successUrl` brings the buyer back from a redirect rail; it must be on
     * one of this app's return origins.
     */
    createCheckoutSession(input: {
      priceKey?: string
      productKey?: string
      priceId?: string
      /** Customer-selected deposit in the price currency’s native units. */
      amount?: string
      /** Defaults to true for recurring prices; false buys the initial term only. */
      autoRenew?: boolean
      successUrl?: string
    }): Promise<CheckoutSessionLink> {
      if (
        !input.priceKey === !input.priceId ||
        !!input.priceKey !== !!input.productKey
      )
        return Promise.reject(
          localError(
            "invalid_request",
            "Pass priceId, or both productKey and priceKey."
          )
        )
      return json(checkoutSessionLinkSchema, "/me/checkout-sessions", {
        method: "POST",
        body: {
          price_key: input.priceKey,
          product_key: input.productKey,
          price_id: input.priceId,
          auto_renew: input.autoRenew,
          amount: input.amount,
          success_url: input.successUrl,
        } satisfies wire.MintCheckoutSessionParams,
      })
    },

    /**
     * Reads and pays one hosted checkout session. By default the id is the
     * only credential: no bearer is sent. With `customerBase` the session's
     * customer reads and pays it on that customer surface, signed in. A
     * refusal the buyer can answer (an invalid card form, too many attempts)
     * resolves as `failed`; an expired or unknown session as `expired`.
     */
    checkoutSource(
      sessionId: string,
      opts: CheckoutSourceOptions = {}
    ): CheckoutSource {
      const path = `/checkout-sessions/${id(sessionId)}`
      const customer = opts.customerBase?.replace(/\/+$/, "")
      const via: RequestOptions =
        customer === undefined ? { anonymous: true } : { root: customer }
      return {
        getSession: () => json(checkoutSessionSchema, path, via),
        async pay(request: PayRequest): Promise<PayResult> {
          try {
            return await json(payResultSchema, `${path}/pay`, {
              ...via,
              method: "POST",
              body: request satisfies Omit<wire.PayCheckoutSessionParams, "billing_details">,
            })
          } catch (err) {
            if (!isBillingError(err)) throw err
            switch (err.status) {
              case 404:
              case 410:
                return { status: "expired" }
              case 403:
                return { status: "blocked", failure_message: err.message }
              case 422:
              case 429:
                return { status: "failed", failure_message: err.message }
            }
            throw err
          }
        },
      }
    },
  })
  return client
}

function normalizeScales(scales: CurrencyScales): CurrencyScales {
  return Object.fromEntries(
    Object.entries(scales).map(([code, decimals]) => [
      code.toUpperCase(),
      decimals,
    ])
  )
}

export type BillingClient = ReturnType<typeof createBillingClient>

export const isWalletRejection = (error: unknown): boolean =>
  error instanceof WalletRejectedError ||
  (error instanceof Error && error.name === "WalletRejectedError") ||
  (error instanceof BillingError && error.code === "wallet_rejected")
