import type { z } from "zod"

import { BillingError, localError, readBillingError } from "./errors"
import { OPENRAILS_CURRENCY_SCALES } from "./generated/openrails-version"
import {
  billingStatusSchema,
  cardSetupSchema,
  paymentAuthenticationSchema,
  invoicePageSchema,
  invoiceSchema,
  pageSchema,
  paymentMethodSchema,
  paymentSchema,
  solanaCancelTxSchema,
  subscriptionSchema,
  type BillingStatus,
  type CardSetup,
  type PaymentAuthentication,
  type CurrencyScales,
  type Invoice,
  type NewCard,
  type Page,
  type Payment,
  type PaymentMethod,
  type Subscription,
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

export interface ListOptions {
  limit?: number
  offset?: number
  signal?: AbortSignal
}

export type SolanaCancelStage = "preparing" | "signing" | "confirming"

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

export const CANCEL_FEEDBACK_MIN = 4
export const CANCEL_FEEDBACK_MAX = 500

type Query = Record<string, string | number | undefined>

interface RequestOptions {
  method?: string
  query?: Query
  body?: unknown
  signal?: AbortSignal
  headers?: Record<string, string>
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
      if (v !== undefined && v !== "") qs.set(k, String(v))
    const q = qs.toString()
    return `${base}${path}${q ? `?${q}` : ""}`
  }

  async function send(
    path: string,
    opts: RequestOptions = {}
  ): Promise<Response> {
    const headers = new Headers({ Accept: "application/json", ...opts.headers })
    if (opts.body !== undefined) headers.set("Content-Type", "application/json")
    const token = await options.getToken?.()
    if (token) headers.set("Authorization", `Bearer ${token}`)
    const lang = options.language?.()
    if (lang) headers.set("Accept-Language", lang)
    const res = await doFetch(url(path, opts.query), {
      method: opts.method ?? "GET",
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      signal: opts.signal,
    })
    if (!res.ok) throw await readBillingError(res)
    return res
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
  const subscriptionPage = pageSchema(subscriptionSchema)
  const methodPage = pageSchema(paymentMethodSchema)
  const paymentPage = pageSchema(paymentSchema)

  return {
    baseUrl: base,

    listSubscriptions(
      opts: ListOptions & { status?: string } = {}
    ): Promise<Page<Subscription>> {
      return json(subscriptionPage, "/me/subscriptions", {
        query: {
          status: opts.status ?? "all",
          limit: opts.limit ?? 100,
          offset: opts.offset,
        },
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
     * Queues the cancellation (202). Access and resumability follow the
     * rail's cancel mode; re-read the subscription to observe the result.
     */
    async cancelSubscription(
      subscriptionId: string,
      input: { feedback: string }
    ): Promise<void> {
      await send(`/me/subscriptions/${id(subscriptionId)}/cancel`, {
        method: "POST",
        body: { feedback: input.feedback.trim() },
      })
    },

    /** Queues the resumption (202). */
    async resumeSubscription(subscriptionId: string): Promise<void> {
      await send(`/me/subscriptions/${id(subscriptionId)}/resume`, {
        method: "POST",
        body: {},
      })
    },

    async setSubscriptionPaymentMethod(
      subscriptionId: string,
      paymentMethodId: string
    ): Promise<void> {
      await send(`/me/subscriptions/${id(subscriptionId)}/payment-method`, {
        method: "PUT",
        body: { payment_method_id: paymentMethodId },
      })
    },

    /**
     * On-chain cancellation for the solana rail: the server prepares an
     * unsigned transaction, the wallet signs and submits it, the server
     * verifies the signature and records the cancel.
     */
    async cancelSubscriptionOnChain(
      subscriptionId: string,
      sendTransaction: SendSolanaTransaction,
      onStage?: (stage: SolanaCancelStage) => void
    ): Promise<void> {
      onStage?.("preparing")
      const { transaction } = await json(
        solanaCancelTxSchema,
        `/me/subscriptions/${id(subscriptionId)}/solana-cancel-tx`,
        { method: "POST", body: {} }
      )
      onStage?.("signing")
      let signature: string
      try {
        signature = await sendTransaction(transaction)
      } catch (err) {
        if (isWalletRejection(err))
          throw localError(
            "wallet_rejected",
            "The wallet request was declined."
          )
        throw err
      }
      if (!signature)
        throw localError(
          "wallet_no_signature",
          "The wallet returned no signature."
        )
      onStage?.("confirming")
      await send(`/me/subscriptions/${id(subscriptionId)}/solana-cancel`, {
        method: "POST",
        body: { signature },
      })
    },

    listPaymentMethods(opts: ListOptions = {}): Promise<Page<PaymentMethod>> {
      return json(methodPage, "/me/payment-methods", {
        query: { limit: opts.limit ?? 100, offset: opts.offset },
        signal: opts.signal,
      })
    },

    /** Stores a card tokenized in the page (`cardSetupDriver` "collect_js"). */
    addPaymentMethod(card: NewCard): Promise<PaymentMethod> {
      return json(paymentMethodSchema, "/me/payment-methods", {
        method: "POST",
        body: card,
      })
    },

    /**
     * Starts an in-page card setup with a PSP whose browser SDK collects the
     * card (see `cardSetupDriver`). `idempotencyKey` identifies this attempt.
     */
    createCardSetup(input: {
      pspId: string
      idempotencyKey: string
    }): Promise<CardSetup> {
      return json(cardSetupSchema, "/me/payment-methods/stripe-setup", {
        method: "POST",
        body: { psp_id: input.pspId, consent: true },
        headers: { "Idempotency-Key": input.idempotencyKey },
      })
    },

    getCardSetup(setupId: string, signal?: AbortSignal): Promise<CardSetup> {
      return json(
        cardSetupSchema,
        `/me/payment-methods/stripe-setup/${id(setupId)}`,
        { signal }
      )
    },

    /** Verifies the setup with the provider; `payment_method_id` once saved. */
    confirmCardSetup(setupId: string): Promise<CardSetup> {
      return json(
        cardSetupSchema,
        `/me/payment-methods/stripe-setup/${id(setupId)}/confirm`,
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

    /** Makes the method the default collector for one currency's invoices. */
    async setDefaultPaymentMethod(input: {
      currency: string
      paymentMethodId: string
    }): Promise<void> {
      await send("/me/collection-payment-method", {
        method: "PUT",
        body: {
          currency: input.currency.toUpperCase(),
          payment_method_id: input.paymentMethodId,
        },
      })
    },

    /** Charges and refunds, newest first. `rail` filters by rail. */
    listPayments(
      opts: ListOptions & { rail?: string } = {}
    ): Promise<Page<Payment>> {
      return json(paymentPage, "/me/payments", {
        query: {
          limit: opts.limit ?? 20,
          offset: opts.offset,
          type: opts.rail,
        },
        signal: opts.signal,
      })
    },

    async listInvoices(opts: ListOptions = {}): Promise<Page<Invoice>> {
      const page = await json(invoicePageSchema, "/me/invoices", {
        query: { limit: opts.limit ?? 20, offset: opts.offset },
        signal: opts.signal,
      })
      const offset = page.offset ?? opts.offset ?? 0
      return {
        data: page.invoices,
        total: page.total,
        limit: page.limit,
        offset,
        has_more:
          page.total != null
            ? offset + page.invoices.length < page.total
            : null,
      }
    },

    getInvoice(invoiceId: string, signal?: AbortSignal): Promise<Invoice> {
      return json(invoiceSchema, `/me/invoices/${id(invoiceId)}`, { signal })
    },

    getStatus(signal?: AbortSignal): Promise<BillingStatus> {
      return json(billingStatusSchema, "/me/status", { signal })
    },

    /** Currency code (upper case) to native-unit decimals. */
    currencies,
  }
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
