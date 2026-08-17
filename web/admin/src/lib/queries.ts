import { keepPreviousData, queryOptions } from "@tanstack/react-query"

import { getTokens } from "@/lib/api/client"
import {
  getCustomerPaymentMethods,
  getCustomerProfile,
  getMerchantSettings,
  getPayment,
  getPrice,
  getPriceKeyHistory,
  getProduct,
  getSubscription,
  getUsageMeter,
  getUnreadCount,
  dryRunCheckoutRouting,
  listAlertRules,
  listAlertTemplates,
  listApiKeys,
  listCatalogDrift,
  listCustomers,
  listFindings,
  listNotifications,
  listPaymentProviders,
  listPayments,
  listPrices,
  listProducts,
  listRepairAlerts,
  listRepriceBatchesByKey,
  listReprices,
  listSubscriptions,
  listUsageMeterOverrides,
  listUsageMeters,
  listTeam,
  listTeamInvites,
  listWebhooks,
  listWorkerHealth,
  type PaymentFilters,
  type RepriceFilters,
  type SubscriptionFilters,
} from "@/lib/api/endpoints"
import {
  getDashboard,
  metricsQuery,
  type MetricsQuery,
} from "@/lib/api/metrics"

const merchantRoot = () =>
  ["merchant", getTokens()?.merchant ?? "unselected"] as const

const queryErrorMeta = (errorAction?: string) =>
  errorAction ? { errorAction } : undefined

export const queryKeys = {
  merchant: merchantRoot,
  customers: () => [...merchantRoot(), "customers"] as const,
  customer: (id: string) => [...merchantRoot(), "customers", id] as const,
  subscriptions: () => [...merchantRoot(), "subscriptions"] as const,
  subscription: (id: string) =>
    [...merchantRoot(), "subscriptions", id] as const,
  payments: () => [...merchantRoot(), "payments"] as const,
  payment: (id: string) => [...merchantRoot(), "payments", id] as const,
  catalog: () => [...merchantRoot(), "catalog"] as const,
  catalogDrift: () => [...merchantRoot(), "catalog", "drift"] as const,
  usageMeters: () => [...merchantRoot(), "catalog", "meters"] as const,
  usageMeter: (key: string) =>
    [...merchantRoot(), "catalog", "meters", key] as const,
  settings: () => [...merchantRoot(), "settings"] as const,
  team: () => [...merchantRoot(), "team"] as const,
  alerts: () => [...merchantRoot(), "alerts"] as const,
  ops: () => [...merchantRoot(), "ops"] as const,
  dashboard: () => [...merchantRoot(), "dashboard"] as const,
  notifications: () => [...merchantRoot(), "notifications"] as const,
}

export const adminQueries = {
  customers: (q: string, limit: number, offset: number) =>
    queryOptions({
      queryKey: [...queryKeys.customers(), { q, limit, offset }],
      queryFn: ({ signal }) => listCustomers(q, limit, offset, signal),
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
  subscriptions: (
    filters: SubscriptionFilters,
    limit: number,
    offset: number
  ) =>
    queryOptions({
      queryKey: [...queryKeys.subscriptions(), { filters, limit, offset }],
      queryFn: ({ signal }) =>
        listSubscriptions(filters, limit, offset, signal),
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
          0,
          signal
        ),
      enabled: Boolean(id),
    }),
  customerPaymentMethods: (customerId?: string) =>
    queryOptions({
      queryKey: [
        ...queryKeys.customer(customerId ?? "unselected"),
        "payment-methods",
      ],
      queryFn: ({ signal }) => getCustomerPaymentMethods(customerId!, signal),
      enabled: Boolean(customerId),
    }),
  payments: (filters: PaymentFilters, limit: number, offset: number) =>
    queryOptions({
      queryKey: [...queryKeys.payments(), { filters, limit, offset }],
      queryFn: ({ signal }) => listPayments(filters, limit, offset, signal),
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
  products: (
    options: { limit?: number; offset?: number; errorAction?: string } = {}
  ) => {
    const { limit = 1000, offset = 0, errorAction } = options
    return queryOptions({
      queryKey: [
        ...queryKeys.catalog(),
        "products",
        { limit, offset, activeOnly: undefined },
      ],
      queryFn: ({ signal }) => listProducts(limit, offset, undefined, signal),
      meta: queryErrorMeta(errorAction),
    })
  },
  prices: (options: { productId?: string; errorAction?: string } = {}) => {
    const { productId, errorAction } = options
    return queryOptions({
      queryKey: [...queryKeys.catalog(), "prices", { productId }],
      queryFn: ({ signal }) => listPrices(productId, signal),
      meta: queryErrorMeta(errorAction),
    })
  },
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
  priceHistory: (priceKey?: string) =>
    queryOptions({
      queryKey: [
        ...queryKeys.catalog(),
        "prices",
        "history",
        priceKey ?? "unselected",
      ],
      queryFn: ({ signal }) => getPriceKeyHistory(priceKey!, signal),
      enabled: Boolean(priceKey),
    }),
  repriceBatches: (priceKey?: string, limit = 5) =>
    queryOptions({
      queryKey: [
        ...queryKeys.catalog(),
        "reprice-batches",
        { priceKey, limit },
      ],
      queryFn: ({ signal }) =>
        listRepriceBatchesByKey(priceKey!, limit, signal),
      enabled: Boolean(priceKey),
    }),
  reprices: (filters?: RepriceFilters, limit = 1000) =>
    queryOptions({
      queryKey: [...queryKeys.catalog(), "reprices", { filters, limit }],
      queryFn: ({ signal }) => listReprices(filters!, limit, 0, signal),
      enabled: Boolean(filters),
    }),
  catalogDrift: (limit = 200, offset = 0) =>
    queryOptions({
      queryKey: [...queryKeys.catalogDrift(), { limit, offset }],
      queryFn: ({ signal }) => listCatalogDrift(limit, offset, signal),
      meta: { errorAction: "Load drift" },
    }),
  checkoutRouting: (priceId: string) =>
    queryOptions({
      queryKey: [...queryKeys.catalog(), "prices", priceId, "routing"],
      queryFn: ({ signal }) =>
        dryRunCheckoutRouting({ price_id: priceId }, signal),
      enabled: Boolean(priceId),
      meta: { errorAction: "Check checkout readiness" },
    }),
  usageMeters: (limit = 200, offset = 0) =>
    queryOptions({
      queryKey: [...queryKeys.usageMeters(), { limit, offset }],
      queryFn: ({ signal }) => listUsageMeters(limit, offset, signal),
      placeholderData: keepPreviousData,
      meta: { errorAction: "Load usage meters" },
    }),
  usageMeter: (key: string) =>
    queryOptions({
      queryKey: queryKeys.usageMeter(key),
      queryFn: ({ signal }) => getUsageMeter(key, signal),
      enabled: Boolean(key),
      meta: { errorAction: "Load usage meter" },
    }),
  usageMeterOverrides: (key: string, limit = 200, offset = 0) =>
    queryOptions({
      queryKey: [...queryKeys.usageMeter(key), "overrides", { limit, offset }],
      queryFn: ({ signal }) =>
        listUsageMeterOverrides(key, limit, offset, signal),
      enabled: Boolean(key),
      placeholderData: keepPreviousData,
      meta: { errorAction: "Load negotiated usage rates" },
    }),
  findings: () =>
    queryOptions({
      queryKey: [...queryKeys.ops(), "findings", { limit: 100, offset: 0 }],
      queryFn: ({ signal }) => listFindings({}, 100, 0, signal),
      meta: { errorAction: "Load findings" },
    }),
  repairAlerts: () =>
    queryOptions({
      queryKey: [...queryKeys.ops(), "repair-alerts", { limit: 50, offset: 0 }],
      queryFn: ({ signal }) => listRepairAlerts(50, 0, signal),
      meta: { errorAction: "Load repair alerts" },
    }),
  workerHealth: () =>
    queryOptions({
      queryKey: [...queryKeys.ops(), "worker-health"],
      queryFn: ({ signal }) => listWorkerHealth(signal),
      staleTime: 10_000,
      meta: { errorAction: "Load worker health" },
    }),
  merchantSettings: (errorAction?: string) =>
    queryOptions({
      queryKey: queryKeys.settings(),
      queryFn: ({ signal }) => getMerchantSettings(signal),
      meta: queryErrorMeta(errorAction),
    }),
  paymentProviders: () =>
    queryOptions({
      queryKey: [...queryKeys.settings(), "payment-providers"],
      queryFn: ({ signal }) => listPaymentProviders(signal),
      meta: { errorAction: "Load payment providers" },
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
  alertTemplates: () =>
    queryOptions({
      queryKey: [...queryKeys.alerts(), "templates"],
      queryFn: ({ signal }) => listAlertTemplates(signal),
      staleTime: 5 * 60_000,
      meta: { errorAction: "Load alert templates" },
    }),
  alertRules: () =>
    queryOptions({
      queryKey: [...queryKeys.alerts(), "rules"],
      queryFn: ({ signal }) => listAlertRules(signal),
      meta: { errorAction: "Load alert rules" },
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
      placeholderData: keepPreviousData,
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
