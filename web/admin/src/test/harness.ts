// Test-only harness. Console tests drive the real api client, query cache and
// components against a stubbed fetch, so endpoint paths, bodies, idempotency
// headers and merchant scoping are exercised rather than mocked away.
import {
  QueryClient,
  QueryClientProvider,
  type DefaultOptions,
  type MutationOptions,
  type QueryKey,
} from "@tanstack/react-query"
import { createElement, type ReactNode } from "react"
import { renderToStaticMarkup } from "react-dom/server"
import { MemoryRouter } from "react-router-dom"
import { vi } from "vitest"

import { createAuthClient, type AuthClient } from "@openrails/auth-ui/client"

import {
  bindSession,
  loadBootstrap,
  setSelectedMerchant,
} from "@/lib/api/client"
import type {
  Invoice,
  Payment,
  PaymentMethod,
  Price,
  Product,
} from "@/lib/api/generated/wire"
import { queryKeys } from "@/lib/queries"

export const MAX_INT64 = "9223372036854775807"
export const MIN_INT64 = "-9223372036854775808"
export const UNSAFE = "9007199254740993" // 2^53 + 1: a Number cannot hold it

export interface Recorded {
  method: string
  path: string
  query: string
  body?: unknown
  headers: Headers
}
export type Reply =
  | ((request: Recorded) => unknown)
  | Record<string, unknown>
  | unknown[]
  | Response

const BOOTSTRAP = {
  api_base_url: "/v1",
  auth_base_url: "/auth/v1",
  nl_widgets_enabled: true,
  ask_enabled: true,
  catalog_copilot_enabled: true,
  catalog_drafting_enabled: false,
  extensions: {},
  issuer: null,
}

const memoryStorage = (): Storage => {
  const values = new Map<string, string>()
  return {
    get length() { return values.size },
    clear: () => values.clear(),
    getItem: (key) => values.get(key) ?? null,
    key: (index) => [...values.keys()][index] ?? null,
    removeItem: (key) => values.delete(key),
    setItem: (key, value) => void values.set(key, String(value)),
  }
}

// server stubs fetch and session storage and returns the recorded requests.
// Routes are keyed "METHOD /admin/..." (or just the path); anything
// unrouted answers {}, so a test spells out only what it asserts.
export async function server(routes: Record<string, Reply> = {}) {
  const requests: Recorded[] = []
  vi.stubGlobal("localStorage", memoryStorage())
  vi.stubGlobal("sessionStorage", memoryStorage())
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      if (url.endsWith("config.json")) return Response.json(BOOTSTRAP)
      const [target, query = ""] = url.split("?")
      const request: Recorded = {
        method: init.method ?? "GET",
        path: target.replace(/^\/v1|^\/auth\/v1/, ""),
        query,
        body: init.body ? JSON.parse(String(init.body)) : undefined,
        headers: new Headers(init.headers),
      }
      requests.push(request)
      const reply =
        routes[`${request.method} ${request.path}`] ?? routes[request.path]
      // A route may answer asynchronously (a pending response a test resolves).
      const value = await (typeof reply === "function" ? reply(request) : reply)
      return value instanceof Response ? value : Response.json(value ?? {})
    })
  )
  await loadBootstrap("/admin/config.json")
  session = createAuthClient({ baseUrl: BOOTSTRAP.auth_base_url, sessionHint: false })
  bindSession(session)
  await signIn("console-test")
  return requests
}

// session is the signed-in operator's auth-ui client the api client uses.
export let session: AuthClient

// accessToken is an unsigned access token for sub: auth-ui reads its claims,
// the stubbed server reads nothing.
export const accessToken = (sub: string, authTime = Math.floor(Date.now() / 1000)) => {
  const part = (value: object) => btoa(JSON.stringify(value)).replaceAll("=", "").replaceAll("+", "-").replaceAll("/", "_")
  return `${part({ alg: "none", typ: "access+jwt" })}.${part({ sub, auth_time: authTime, exp: authTime + 3600 })}.`
}

export const signIn = (sub: string) =>
  session.completeSignIn(async () => ({
    status: "complete",
    token_set: { access_token: accessToken(sub), token_type: "Bearer", expires_in: 3600 },
  }))

export const calls = (requests: Recorded[]) =>
  requests.map((request) => `${request.method} ${request.path}`)
export const selectMerchant = (merchant: string) => setSelectedMerchant(merchant)
export const client = (queries: DefaultOptions["queries"] = {}) =>
  new QueryClient({
    defaultOptions: {
      queries: { retry: false, retryOnMount: false, ...queries },
      mutations: { retry: false },
    },
  })
export const exec = <TData, TError, TInput, TContext>(
  queryClient: QueryClient, options: MutationOptions<TData, TError, TInput, TContext>, input: TInput
) => queryClient.getMutationCache().build(queryClient, options).execute(input)

// Static markup is enough for what these tests assert (amounts, permissions,
// links); the one workflow that needs a live DOM mounts it itself.
export const render = (node: ReactNode, queryClient = client()) =>
  renderToStaticMarkup(
    createElement(
      MemoryRouter,
      null,
      createElement(QueryClientProvider, { client: queryClient }, node)
    )
  )

const WHEN = "2026-09-18T00:00:00Z"
export const aProduct = (id: string, tierRank = 0, overrides: Partial<Product> = {}): Product => ({
  id, key: id, revision: 0, display_name: id, description: "", entitlements: [], tier_group: "plans", ownership: null,
  tier_rank: tierRank, archived: false, prices: [], created_at: WHEN, updated_at: WHEN,
  ...overrides,
})
export const aPrice = (id: string, productId: string, overrides: Partial<Price> = {}): Price => ({
  id, key: id, revision: 0, product_id: productId, archived: false, currency: "USD",
  unit_amount: "20000000", access_duration_hours: 720, billing_interval_hours: 720, trial_unit_amount: null, trial_duration_hours: null,
  quantity: null, psps: {}, pending_manual_actions: [], created_at: WHEN, updated_at: WHEN,
  ...overrides,
})

export const aPayment = (id: string, overrides: Partial<Payment> = {}): Payment => ({
  id, kind: "charge", status: "succeeded", amount: "20000000", amount_refunded: "0",
  currency: "USD", customer_id: "cus_1", subscription_id: null, order_id: null, invoice_id: null, price_id: "price_1",
  price: null, product: null, channel: "rail", rail: "nmi", psp_id: "psp_1",
  transaction_id: `txn_${id}`, card: null, failure: null, refunded_payment_id: null,
  reason: null, refunds: [], quantity: null, created_at: WHEN,
  ...overrides,
})
export const aPaymentMethod = (id: string, overrides: Partial<PaymentMethod> = {}): PaymentMethod => ({
  id, customer_id: "cus_1", rail: "nmi", psp_id: "psp_1",
  card: { brand: "visa", last4: "4242", exp_month: 12, exp_year: 2030 },
  billing_details: null, subscriptions: [], collection_currencies: [], created_at: WHEN,
  health: { expiry_status: "valid", last_charged_at: null, last_charge_outcome: null, active: true },
  ...overrides,
})
export const anInvoice = (id: string, overrides: Partial<Invoice> = {}): Invoice => ({
  id, customer_id: "cus_1", currency: "USD", invoice_number: null,
  period_starts_at: "2026-09-01T00:00:00Z", period_ends_at: "2026-10-01T00:00:00Z",
  usage_total: "0", deposits_total: "0", owed_accrued: "0", owed_paid: "0", closing_balance: "0",
  subtotal_amount: "0", total_amount: "0", amount_paid: "0", amount_due: "0",
  line_items: [], money_movements: null, po_number: null, tax: null, billing_contacts: [],
  memo: null, status: "open", collection_method: "charge_automatically", issued_at: null,
  due_at: null, delinquent: false, paid_at: null, voided_at: null, uncollectible_at: null, finalized_at: null,
  external_invoice_id: null, collection_failure_count: 0, collection_failed_at: null,
  next_collection_attempt_at: null, last_collection_failure_code: null, recovery: null,
  available_actions: [], created_at: WHEN,
  ...overrides,
})

// cursorPages serves rows as a cursor list of `size`-row pages; the cursor is
// the next row's index.
export const cursorPages = <T>(rows: T[], size: number) => (request: Recorded) => {
  const start = Number(new URLSearchParams(request.query).get("cursor") ?? 0)
  const next = start + size
  return { data: rows.slice(start, next), next_cursor: next < rows.length ? String(next) : null }
}

// The cache keys a console screen can hold at once. Seeding all of them and
// reading back which ones a mutation invalidated asserts the whole blast
// radius — what it refreshed and what it left alone — in one comparison.
const CACHE_KEYS = (): Record<string, QueryKey> => ({
  customers: queryKeys.customers(), customer: queryKeys.customer("cus_1"),
  "customer.rates": queryKeys.customerUsageRates("cus_1"),
  subscriptions: queryKeys.subscriptions(), subscription: queryKeys.subscription("sub_1"),
  payments: queryKeys.payments(), payment: queryKeys.payment("pay_1"),
  attempts: queryKeys.attempts(), cycles: queryKeys.cycles(),
  catalog: queryKeys.catalog(), drift: queryKeys.catalogDrift(),
  meters: queryKeys.usageMeters(), meter: queryKeys.usageMeter("tokens"),
  settings: queryKeys.settings(), psps: [...queryKeys.settings(), "psps"],
  alerts: queryKeys.alerts(), ops: queryKeys.ops(),
  dashboard: queryKeys.dashboard(), notifications: queryKeys.notifications(),
})

export function seedCache(queryClient: QueryClient, merchant: string) {
  selectMerchant(merchant)
  return Object.entries(CACHE_KEYS()).map(([name, key]): [string, QueryKey] => {
    queryClient.setQueryData(key, {})
    return [`${merchant}:${name}`, key]
  })
}

export const invalidated = (queryClient: QueryClient, seeded: [string, QueryKey][]) =>
  seeded
    .filter(([, key]) => queryClient.getQueryState(key)?.isInvalidated === true)
    .map(([name]) => name)
    .sort()
