import { keepPreviousData, queryOptions } from "@tanstack/react-query"

import { collectCursorPages, selectedMerchant } from "@/lib/api/client"
import type { ListPage } from "@/lib/api/generated/wire"
import {
  getCatalogRevision,
  getCustomerProfile,
  getMerchantConfiguration,
  getPayment,
  getPaymentAttempt,
  getPrice,
  getPriceKeyHistory,
  getProduct,
  getRebillCycle,
  getSubscription,
  getUsageMeter,
  getUnreadCount,
  listApiKeys,
  listCatalogDrift,
  listCustomerPaymentMethods,
  listCustomers,
  listCustomerUsageRateOverrides,
  getFindingSummary,
  listFindings,
  listNotifications,
  listPaymentAttempts,
  listPSPs,
  listRails,
  previewPSPRouting,
  listPayments,
  listPrices,
  listProducts,
  listRebillCycles,
  listRepriceBatches,
  listReprices,
  listSubscriptions,
  listUsageMeterOverrides,
  listUsageMeters,
  listTeam,
  listTeamInvites,
  getConfig,
  listWebhooks,
  listWorkerHealth,
  PAGE_MAX,
  type AttemptFilters,
  type CycleFilters,
  type PaymentFilters,
  type SubscriptionFilters,
} from "@/lib/api/endpoints"
import {
  getDashboard,
  metricsQuery,
  type MetricsQuery,
} from "@/lib/api/metrics"

// Complete collections are explicit: selectors need every eligible record,
// while catalog screens fetch only their visible page.
export async function collectPages<T>(
  loadPage: (
    cursor: string | undefined,
    signal?: AbortSignal
  ) => Promise<ListPage<T>>,
  signal?: AbortSignal
): Promise<ListPage<T>> {
  const data = await collectCursorPages(
    (cursor) => loadPage(cursor, signal),
    signal
  )
  return { data, next_cursor: null }
}

type MerchantRoot = readonly ["merchant", string]

const currentMerchantRoot = (): MerchantRoot =>
  ["merchant", selectedMerchant() ?? "unselected"] as const

// One vocabulary of merchant-scoped keys, bound to a root that is either read
// live or pinned once. Nothing below reaches for the selected merchant itself.
const buildQueryKeys = (root: () => MerchantRoot) => ({
  merchant: () => root(),
  customers: () => [...root(), "customers"] as const,
  customer: (id: string) => [...root(), "customers", id] as const,
  customerUsageRates: (id: string) =>
    [...root(), "customers", id, "usage-rates"] as const,
  subscriptions: () => [...root(), "subscriptions"] as const,
  subscription: (id: string) => [...root(), "subscriptions", id] as const,
  payments: () => [...root(), "payments"] as const,
  payment: (id: string) => [...root(), "payments", id] as const,
  attempts: () => [...root(), "payment-attempts"] as const,
  cycles: () => [...root(), "rebill-cycles"] as const,
  catalog: () => [...root(), "catalog"] as const,
  catalogDrift: () => [...root(), "catalog", "drift"] as const,
  usageMeters: () => [...root(), "catalog", "meters"] as const,
  usageMeter: (key: string) => [...root(), "catalog", "meters", key] as const,
  settings: () => [...root(), "settings"] as const,
  team: () => [...root(), "team"] as const,
  alerts: () => [...root(), "alerts"] as const,
  ops: () => [...root(), "ops"] as const,
  dashboard: () => [...root(), "dashboard"] as const,
  notifications: () => [...root(), "notifications"] as const,
})

export type MerchantQueryKeys = ReturnType<typeof buildQueryKeys>

const queryErrorMeta = (errorAction?: string) =>
  errorAction ? { errorAction } : undefined

// Live keys: every call names whichever merchant is selected right now. That is
// what reads want, because a query key is built by the render that shows it.
export const queryKeys: MerchantQueryKeys = buildQueryKeys(currentMerchantRoot)

// Pinned keys: the selected merchant is read once, here. Keys derived from the
// result name that merchant forever, so work started under one merchant can
// never invalidate under another that the operator selected while it ran.
export const merchantQueryKeys = (): MerchantQueryKeys => {
  const root = currentMerchantRoot()
  return buildQueryKeys(() => root)
}

export const adminQueries = {
  customers: (search: string, limit: number, cursor: string) =>
    queryOptions({
      queryKey: [...queryKeys.customers(), { search, limit, cursor }],
      queryFn: ({ signal }) => listCustomers(search, limit, cursor, signal),
      placeholderData: keepPreviousData,
      meta: { errorAction: "Load customers" },
    }),
  customer: (id: string) =>
    queryOptions({
      queryKey: queryKeys.customer(id),
      queryFn: ({ signal }) => getCustomerProfile(id, signal),
      enabled: Boolean(id),
      meta: { errorAction: "Load customer" },
    }),
  customerUsageRates: (id: string) =>
    queryOptions({
      queryKey: queryKeys.customerUsageRates(id),
      queryFn: ({ signal }) =>
        collectPages(
          (cursor, signal) =>
            listCustomerUsageRateOverrides(id, cursor, signal),
          signal
        ),
      enabled: Boolean(id),
      meta: { errorAction: "Load negotiated usage rates" },
    }),
  subscriptions: (
    filters: SubscriptionFilters,
    limit: number,
    cursor: string
  ) =>
    queryOptions({
      queryKey: [...queryKeys.subscriptions(), { filters, limit, cursor }],
      queryFn: ({ signal }) =>
        listSubscriptions(filters, limit, cursor || undefined, signal),
      placeholderData: keepPreviousData,
      meta: { errorAction: "Load subscriptions" },
    }),
  subscription: (id: string) =>
    queryOptions({
      queryKey: queryKeys.subscription(id),
      queryFn: ({ signal }) => getSubscription(id, signal),
      enabled: Boolean(id),
      meta: { errorAction: "Load subscription" },
    }),
  subscriptionReprices: (id: string) =>
    queryOptions({
      queryKey: [...queryKeys.subscription(id), "reprices", "scheduled"],
      queryFn: ({ signal }) =>
        listReprices(
          { subscription_id: id, status: "scheduled" },
          100,
          undefined,
          signal
        ),
      enabled: Boolean(id),
    }),
  // Every saved method: pickers must offer all of them.
  customerPaymentMethods: (customerId?: string) =>
    queryOptions({
      queryKey: [
        ...queryKeys.customer(customerId ?? "unselected"),
        "payment-methods",
      ],
      queryFn: ({ signal }) =>
        collectCursorPages(
          (cursor) =>
            listCustomerPaymentMethods(
              customerId!,
              { limit: 100, cursor },
              signal
            ),
          signal
        ),
      enabled: Boolean(customerId),
    }),
  payments: (filters: PaymentFilters, limit: number, cursor?: string) =>
    queryOptions({
      queryKey: [...queryKeys.payments(), { filters, limit, cursor }],
      queryFn: ({ signal }) => listPayments(filters, { limit, cursor }, signal),
      placeholderData: keepPreviousData,
      meta: { errorAction: "Load payments" },
    }),
  payment: (id: string) =>
    queryOptions({
      queryKey: queryKeys.payment(id),
      queryFn: ({ signal }) => getPayment(id, signal),
      enabled: Boolean(id),
      meta: { errorAction: "Load payment" },
    }),
  attempts: (filters: AttemptFilters, limit: number, cursor?: string) =>
    queryOptions({
      queryKey: [...queryKeys.attempts(), { filters, limit, cursor }],
      queryFn: ({ signal }) =>
        listPaymentAttempts(filters, { limit, cursor }, signal),
      placeholderData: keepPreviousData,
      meta: { errorAction: "Load payment attempts" },
    }),
  attempt: (id: string) =>
    queryOptions({
      queryKey: [...queryKeys.attempts(), id],
      queryFn: ({ signal }) => getPaymentAttempt(id, signal),
      enabled: Boolean(id),
      meta: { errorAction: "Load payment attempt" },
    }),
  cycles: (filters: CycleFilters, limit: number, cursor?: string) =>
    queryOptions({
      queryKey: [...queryKeys.cycles(), { filters, limit, cursor }],
      queryFn: ({ signal }) =>
        listRebillCycles(filters, { limit, cursor }, signal),
      placeholderData: keepPreviousData,
      meta: { errorAction: "Load rebill cycles" },
    }),
  cycle: (id: string) =>
    queryOptions({
      queryKey: [...queryKeys.cycles(), id],
      queryFn: ({ signal }) => getRebillCycle(id, signal),
      enabled: Boolean(id),
      meta: { errorAction: "Load rebill cycle" },
    }),
  products: (
    options: { limit?: number; cursor?: string; errorAction?: string } = {}
  ) => {
    const { limit = 100, cursor, errorAction } = options
    return queryOptions({
      queryKey: [...queryKeys.catalog(), "products", { limit, cursor }],
      queryFn: ({ signal }) => listProducts(limit, cursor, undefined, signal),
      placeholderData: keepPreviousData,
      meta: queryErrorMeta(errorAction),
    })
  },
  allProducts: (options: { errorAction?: string } = {}) =>
    queryOptions({
      queryKey: [...queryKeys.catalog(), "products", "all"],
      queryFn: ({ signal }) =>
        collectPages(
          (cursor, signal) => listProducts(PAGE_MAX, cursor, undefined, signal),
          signal
        ),
      meta: queryErrorMeta(options.errorAction),
    }),
  prices: (
    options: {
      productId?: string
      limit?: number
      cursor?: string
      errorAction?: string
    } = {}
  ) => {
    const { productId, limit = 100, cursor, errorAction } = options
    return queryOptions({
      queryKey: [
        ...queryKeys.catalog(),
        "prices",
        { productId, limit, cursor },
      ],
      queryFn: ({ signal }) => listPrices(limit, cursor, productId, signal),
      placeholderData: keepPreviousData,
      meta: queryErrorMeta(errorAction),
    })
  },
  allPrices: (options: { productId?: string; errorAction?: string } = {}) =>
    queryOptions({
      queryKey: [
        ...queryKeys.catalog(),
        "prices",
        "all",
        { productId: options.productId },
      ],
      queryFn: ({ signal }) =>
        collectPages(
          (cursor, signal) =>
            listPrices(PAGE_MAX, cursor, options.productId, signal),
          signal
        ),
      meta: queryErrorMeta(options.errorAction),
    }),
  price: (
    id: string,
    options: { verify?: boolean; errorAction?: string } = {}
  ) => {
    const { verify = false, errorAction } = options
    return queryOptions({
      queryKey: [...queryKeys.catalog(), "prices", id, { verify }],
      queryFn: ({ signal }) => getPrice(id, verify, signal),
      enabled: Boolean(id),
      placeholderData: keepPreviousData,
      meta: queryErrorMeta(errorAction),
    })
  },
  product: (id?: string) =>
    queryOptions({
      queryKey: [...queryKeys.catalog(), "products", id ?? "unselected"],
      queryFn: ({ signal }) => getProduct(id!, signal),
      enabled: Boolean(id),
    }),
  priceHistory: (productKey?: string, priceKey?: string) =>
    queryOptions({
      queryKey: [
        ...queryKeys.catalog(),
        "prices",
        "history",
        { productKey, priceKey },
      ],
      queryFn: ({ signal }) =>
        getPriceKeyHistory(productKey!, priceKey!, signal),
      enabled: Boolean(productKey && priceKey),
    }),
  repriceBatches: (productKey?: string, priceKey?: string, limit = 5) =>
    queryOptions({
      queryKey: [
        ...queryKeys.catalog(),
        "reprice-batches",
        { productKey, priceKey, limit },
      ],
      queryFn: ({ signal }) =>
        listRepriceBatches(productKey!, priceKey!, limit, signal),
      enabled: Boolean(productKey && priceKey),
    }),
  catalogRevision: () =>
    queryOptions({
      queryKey: [...queryKeys.catalog(), "revision"],
      queryFn: () => getCatalogRevision(),
    }),
  catalogDrift: (limit = 200, cursor?: string) =>
    queryOptions({
      queryKey: [...queryKeys.catalogDrift(), { limit, cursor }],
      queryFn: ({ signal }) => listCatalogDrift(limit, cursor, signal),
      meta: { errorAction: "Load drift" },
    }),
  checkoutRouting: (priceId: string) =>
    queryOptions({
      queryKey: [...queryKeys.catalog(), "prices", priceId, "routing"],
      queryFn: ({ signal }) => previewPSPRouting({ price_id: priceId }, signal),
      enabled: Boolean(priceId),
      meta: { errorAction: "Check checkout readiness" },
    }),
  usageMeters: (limit = 200, cursor?: string) =>
    queryOptions({
      queryKey: [...queryKeys.usageMeters(), { limit, cursor }],
      queryFn: ({ signal }) => listUsageMeters(limit, cursor, signal),
      placeholderData: keepPreviousData,
      meta: { errorAction: "Load usage meters" },
    }),
  allUsageMeters: () =>
    queryOptions({
      queryKey: [...queryKeys.usageMeters(), "all"],
      queryFn: ({ signal }) =>
        collectPages(
          (cursor, signal) => listUsageMeters(PAGE_MAX, cursor, signal),
          signal
        ),
      meta: { errorAction: "Load usage meters" },
    }),
  usageMeter: (key: string) =>
    queryOptions({
      queryKey: queryKeys.usageMeter(key),
      queryFn: ({ signal }) => getUsageMeter(key, signal),
      enabled: Boolean(key),
      meta: { errorAction: "Load usage meter" },
    }),
  usageMeterOverrides: (key: string, limit = 200, cursor?: string) =>
    queryOptions({
      queryKey: [...queryKeys.usageMeter(key), "overrides", { limit, cursor }],
      queryFn: ({ signal }) =>
        listUsageMeterOverrides(key, limit, cursor, signal),
      enabled: Boolean(key),
      placeholderData: keepPreviousData,
      meta: { errorAction: "Load negotiated usage rates" },
    }),
  findings: () =>
    queryOptions({
      queryKey: [...queryKeys.ops(), "findings", { limit: 100 }],
      queryFn: ({ signal }) => listFindings({}, 100, signal),
      meta: { errorAction: "Load findings" },
    }),
  findingSummary: () =>
    queryOptions({
      queryKey: [...queryKeys.ops(), "findings", "summary"],
      queryFn: ({ signal }) => getFindingSummary(signal),
      meta: { errorAction: "Load the findings summary" },
    }),
  workerHealth: () =>
    queryOptions({
      queryKey: [...queryKeys.ops(), "worker-health"],
      queryFn: ({ signal }) => listWorkerHealth(signal),
      staleTime: 10_000,
      meta: { errorAction: "Load worker health" },
    }),
  merchantConfiguration: (errorAction?: string) =>
    queryOptions({
      queryKey: queryKeys.settings(),
      queryFn: ({ signal }) => getMerchantConfiguration(signal),
      meta: queryErrorMeta(errorAction),
    }),
  psps: () =>
    queryOptions({
      queryKey: [...queryKeys.settings(), "psps"],
      queryFn: ({ signal }) => listPSPs(signal),
      meta: { errorAction: "Load PSPs" },
    }),
  rails: () =>
    queryOptions({
      queryKey: [...queryKeys.settings(), "rails"],
      queryFn: ({ signal }) => listRails(signal),
      staleTime: Infinity,
      meta: { errorAction: "Load rails" },
    }),
  apiKeys: () =>
    queryOptions({
      queryKey: [...queryKeys.settings(), "api-keys"],
      queryFn: ({ signal }) => listApiKeys(signal),
      meta: { errorAction: "Load API keys" },
    }),
  team: () =>
    queryOptions({
      queryKey: [...queryKeys.team(), "members"],
      queryFn: ({ signal }) => listTeam(signal),
      meta: { errorAction: "Load team" },
    }),
  teamInvites: () =>
    queryOptions({
      queryKey: [...queryKeys.team(), "invites"],
      queryFn: ({ signal }) => listTeamInvites(signal),
      meta: { errorAction: "Load invites" },
    }),
  config: () =>
    queryOptions({
      queryKey: ["config"],
      queryFn: ({ signal }) => getConfig(signal),
      staleTime: 5 * 60_000,
      meta: { errorAction: "Load configuration" },
    }),
  webhooks: () =>
    queryOptions({
      queryKey: [...queryKeys.alerts(), "webhooks"],
      queryFn: ({ signal }) => listWebhooks(signal),
      meta: { errorAction: "Load webhooks" },
    }),
  dashboard: () =>
    queryOptions({
      queryKey: queryKeys.dashboard(),
      queryFn: ({ signal }) => getDashboard(signal),
      meta: { errorAction: "Load dashboard" },
    }),
  widgetMetrics: (query?: MetricsQuery) =>
    queryOptions({
      queryKey: [...queryKeys.dashboard(), "metrics", query ?? "unselected"],
      queryFn: ({ signal }) => metricsQuery(query!, signal),
      enabled: Boolean(query),
    }),
  notifications: (enabled: boolean) =>
    queryOptions({
      queryKey: queryKeys.notifications(),
      queryFn: ({ signal }) => listNotifications(undefined, signal),
      enabled,
    }),
  unreadNotifications: () =>
    queryOptions({
      queryKey: [...queryKeys.notifications(), "unread-count"],
      queryFn: ({ signal }) => getUnreadCount(signal),
      refetchInterval: 30_000,
      retry: false,
    }),
}
