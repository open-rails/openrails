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

import { loadBootstrap, setTokens } from "@/lib/api/client"
import type { CatalogPrice, CatalogProduct } from "@/lib/api/types"
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
  auth_base_url: "/auth",
  nl_widgets_enabled: true,
  ask_enabled: true,
  catalog_copilot_enabled: true,
  catalog_drafting_enabled: false,
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
// Routes are keyed "METHOD /merchant/..." (or just the path); anything
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
        path: target.replace(/^\/v1|^\/auth/, ""),
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
  await loadBootstrap()
  return requests
}

export const calls = (requests: Recorded[]) =>
  requests.map((request) => `${request.method} ${request.path}`)
export const selectMerchant = (merchant: string) =>
  setTokens({ access_token: "console-test", merchant })
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
export const aProduct = (id: string, tierRank = 0, overrides: Partial<CatalogProduct> = {}): CatalogProduct => ({
  id, key: id, display_name: id, description: "", tier_group: "plans",
  tier_rank: tierRank, archived: false, created_at: WHEN, updated_at: WHEN,
  ...overrides,
})
export const aPrice = (id: string, productId: string, overrides: Partial<CatalogPrice> = {}): CatalogPrice => ({
  id, key: id, product_id: productId, archived: false, currency: "USD",
  unit_amount: "20000000", auto_renew: true, created_at: WHEN, updated_at: WHEN,
  ...overrides,
})

// The cache keys a console screen can hold at once. Seeding all of them and
// reading back which ones a mutation invalidated asserts the whole blast
// radius — what it refreshed and what it left alone — in one comparison.
const CACHE_KEYS = (): Record<string, QueryKey> => ({
  customers: queryKeys.customers(), customer: queryKeys.customer("cus_1"),
  "customer.rates": queryKeys.customerUsageRates("cus_1"),
  subscriptions: queryKeys.subscriptions(), subscription: queryKeys.subscription("sub_1"),
  payments: queryKeys.payments(), payment: queryKeys.payment("pay_1"),
  catalog: queryKeys.catalog(), drift: queryKeys.catalogDrift(),
  meters: queryKeys.usageMeters(), meter: queryKeys.usageMeter("tokens"),
  settings: queryKeys.settings(), providers: [...queryKeys.settings(), "payment-providers"],
  team: queryKeys.team(), alerts: queryKeys.alerts(), ops: queryKeys.ops(),
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
