import type { z } from "zod"

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
  customerLinkSchema,
  networkLinkChallengeSchema,
  publicConfigSchema,
  listPageSchema,
  orderSchema,
  cartSchema,
  pageSchema,
  paymentMethodSchema,
  paymentSchema,
  productSchema,
  subscriptionSchema,
  subscriptionChangePreviewSchema,
  subscriptionChangeSchema,
  type Account,
  type CustomerLink,
  type NetworkLinkChallenge,
  type PublicConfig,
  type CurrencyScales,
  type ListPage,
  type NewCard,
  type NextAction,
  type Cart,
  type Order,
  type OrderPaymentInput,
  type Page,
  type Payment,
  type PaymentMethod,
  type Product,
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
  /** Sends no bearer: a public route. */
  anonymous?: boolean
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

/**
 * An order request's answer. `error` is set when the payment failed: the
 * order is open again and its `payment.last_payment_error` says why.
 */
export interface OrderResult {
  order: Order
  error?: BillingError
}

export function createBillingClient(options: BillingClientOptions = {}) {
  const base = (options.baseUrl ?? "/billing/v1").replace(/\/+$/, "")
  const doFetch =
    options.fetch ?? ((input: string, init: RequestInit) => fetch(input, init))
  const currencies = normalizeScales({
    ...OPENRAILS_CURRENCY_SCALES,
    ...options.currencies,
  })

  function url(path: string, query?: Query): string {
    const qs = new URLSearchParams()
    for (const [k, v] of Object.entries(query ?? {}))
      if (Array.isArray(v)) for (const item of v) qs.append(k, item)
      else if (v !== undefined && v !== "") qs.set(k, String(v))
    const q = qs.toString()
    return `${base}${path}${q ? `?${q}` : ""}`
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
        res = await doFetch(url(path, opts.query), init)
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
  const orderPage = listPageSchema(orderSchema)
  const cursorQuery = (opts: CursorOptions, limit: number) => ({
    limit: opts.limit ?? limit,
    cursor: opts.cursor ?? undefined,
  })
  const productPage = pageSchema(productSchema)

  // An order answer: the order, or a failed payment's 402 with it.
  async function orderResult(
    path: string,
    opts: RequestOptions
  ): Promise<OrderResult> {
    try {
      return { order: await json(orderSchema, path, opts) }
    } catch (cause) {
      if (isBillingError(cause) && cause.status === 402 && cause.order) {
        const order = orderSchema.safeParse(cause.order)
        if (order.success) return { order: order.data, error: cause }
      }
      throw cause
    }
  }

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
     * Reads the provider for a change awaiting the customer, after they
     * completed its `next_action` (the issuer's authentication), and answers
     * the change as it now stands.
     */
    confirmSubscriptionChange(
      subscriptionId: string
    ): Promise<SubscriptionChange> {
      return json(
        subscriptionChangeSchema,
        `/me/subscriptions/${id(subscriptionId)}/confirm`,
        { method: "POST", body: {} }
      )
    },

    /**
     * Moves the subscription to another price of its tier group, to other
     * seats of a per-seat price, or both. An upgrade and more seats charge
     * the saved card now; a downgrade and fewer seats apply at period end.
     * Reuse `idempotencyKey` until the change resolves (`processing`, a
     * `subscription_change_in_flight` refusal or a lost response) so the
     * stored result replays instead of charging twice. A Solana wallet next
     * action is completed with `signWalletAction`, then repeated with
     * `signature`; an `authenticate` one with `authenticateChange`.
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

    /** The customer's cart, priced now. */
    getCart(signal?: AbortSignal): Promise<Cart> {
      return json(cartSchema, "/me/cart", { signal })
    },

    /**
     * Puts a price in the cart or changes its line, and answers the cart.
     * `quantity` 0 removes the line; omitted, the price's default. `amount`
     * is the customer's choice on a price that takes one.
     */
    setCartLine(
      priceId: string,
      input: { quantity?: number; amount?: string } = {}
    ): Promise<Cart> {
      return json(cartSchema, `/me/cart/lines/${id(priceId)}`, {
        method: "PUT",
        body: input satisfies wire.SetCartLineParams,
      })
    },

    /** Empties the cart. */
    async clearCart(): Promise<void> {
      await send("/me/cart", { method: "DELETE" })
    },

    /**
     * Creates an order from `lines` or the cart (`cart: true`), and with
     * `payment` pays it in the same call; `expectedTotal` is the total the
     * customer saw. A failed payment resolves as payOrder's does.
     */
    createOrder(input: {
      lines?: { price_id: string; quantity?: number; amount?: string }[]
      cart?: boolean
      expectedTotal?: string
      payment?: OrderPaymentInput
      reusable?: boolean
      idempotencyKey: string
    }): Promise<OrderResult> {
      return orderResult("/me/orders", {
        method: "POST",
        body: {
          ...(input.cart ? { cart: true } : { lines: input.lines ?? [] }),
          ...(input.expectedTotal === undefined
            ? {}
            : { expected_total: input.expectedTotal }),
          ...(input.payment === undefined ? {} : { payment: input.payment }),
          ...(input.reusable === undefined ? {} : { reusable: input.reusable }),
        },
        headers: { "Idempotency-Key": input.idempotencyKey },
      })
    },

    /**
     * The customer's orders, newest first: `origin: "usage"` their usage
     * orders, `overdue` those still owed past their due date.
     */
    listOrders(
      opts: CursorOptions & {
        origin?: string
        status?: string
        subscriptionId?: string
        overdue?: boolean
      } = {}
    ): Promise<ListPage<Order>> {
      return json(orderPage, "/me/orders", {
        query: {
          ...cursorQuery(opts, 20),
          origin: opts.origin,
          status: opts.status,
          subscription_id: opts.subscriptionId,
          overdue: opts.overdue ? "true" : undefined,
        },
        signal: opts.signal,
      })
    },

    /** One of the customer's orders: the resumable handle of a purchase. */
    getOrder(orderId: string, signal?: AbortSignal): Promise<Order> {
      return json(orderSchema, `/me/orders/${id(orderId)}`, { signal })
    },

    /**
     * Pays an open order. A failed payment resolves with the order (open,
     * `payment.last_payment_error` saying why) and `error`, the 402.
     * `idempotencyKey` is minted when the pay button renders: a retry with it
     * answers what the first request answered.
     */
    payOrder(
      orderId: string,
      input: {
        payment: OrderPaymentInput
        expectedTotal: string
        reusable?: boolean
        idempotencyKey: string
      }
    ): Promise<OrderResult> {
      return orderResult(`/me/orders/${id(orderId)}/pay`, {
        method: "POST",
        body: {
          payment: input.payment,
          expected_total: input.expectedTotal,
          ...(input.reusable === undefined ? {} : { reusable: input.reusable }),
        },
        headers: { "Idempotency-Key": input.idempotencyKey },
      })
    },

    /** Reads the provider after the customer completed `next_action`. */
    confirmOrder(orderId: string): Promise<OrderResult> {
      return orderResult(`/me/orders/${id(orderId)}/confirm`, {
        method: "POST",
        body: {},
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
     * usage orders and every subscription in it without its own card.
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

    /**
     * The customer's summary: balance and amount owed per currency, the card
     * that pays each currency, and unread notices.
     */
    getAccount(signal?: AbortSignal): Promise<Account> {
      return json(accountSchema, "/me", { signal })
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
     * A challenge to link the customer to an openrails.dev account: its
     * `nonce` goes in the network sign-in's authorization request.
     */
    createNetworkLinkChallenge(): Promise<NetworkLinkChallenge> {
      return json(networkLinkChallengeSchema, "/me/network-link/challenges", {
        method: "POST",
        body: {},
      })
    },

    /**
     * Links the customer to the openrails.dev account whose fresh sign-in
     * answered the challenge (its ID token).
     */
    createNetworkLink(params: {
      challengeId: string
      idToken: string
    }): Promise<CustomerLink> {
      return json(customerLinkSchema, "/me/network-link", {
        method: "POST",
        body: { challenge_id: params.challengeId, id_token: params.idToken },
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

    /** Currency code (upper case) to native-unit decimals. */
    currencies,
  }
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
