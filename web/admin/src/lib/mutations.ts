import { mutationOptions, type QueryClient } from "@tanstack/react-query"

import {
  askCatalogCopilot,
  confirmCopilotDraft,
  type CreatePriceDraft,
} from "@/lib/api/copilot"
import {
  activatePrice,
  activateProduct,
  cancelReprice,
  cancelSubscription,
  changeTeamRole,
  changeSubscriptionPaymentMethod,
  changeSubscriptionTier,
  createApiKey,
  createOffChannelPayment,
  createPrice,
  createProduct,
  createWebhook,
  rotateWebhookURL,
  deactivatePrice,
  deactivateProduct,
  archivePaymentProviderAccount,
  deleteDefaultUsageRateCard,
  deleteCustomerUsageRateOverride,
  deleteWebhook,
  getCreditLimit,
  getPriceByKey,
  getProduct,
  getTrustLevel,
  grantEntitlement,
  grantProductAccess,
  inviteTeamMember,
  listCustomers,
  listPayments,
  listSubscriptions,
  markNotificationRead,
  putMerchantSettings,
  putDefaultUsageRateCard,
  putCustomerUsageRateOverride,
  putPaymentProvider,
  putUsageMeter,
  previewRepriceAllPriorVersions,
  previewSubscriptionTierChange,
  publishCatalog,
  refreshCatalogDrift,
  refundPayment,
  removeTeamMember,
  repriceAllPriorVersions,
  resolveFinding,
  resumeSubscription,
  revokeApiKey,
  revokeEntitlement,
  revokeProductAccess,
  revokeTeamInvite,
  setCreditLimit,
  updateProduct,
  type OffChannelPaymentRequest,
  type DefaultUsageRateCardRequest,
  type CustomerUsageRateOverrideRequest,
  type PaymentFilters,
  type PriceRequest,
  type ProductRequest,
  type UsageMeterRequest,
  type SubscriptionFilters,
  type UpsertProviderRequest,
  type WebhookRequest,
} from "@/lib/api/endpoints"
import {
  askMetrics,
  generateWidget,
  type MetricsQuery,
  putDashboard,
  type Widget,
} from "@/lib/api/metrics"
import type {
  CustomerSummary,
  MerchantSettings,
  MerchantNotification,
  PaymentObject,
  AdminSubscription,
} from "@/lib/api/types"
import { merchantQueryKeys } from "@/lib/queries"

const EXPORT_PAGE = 200

const collectAllPages = async <T>(
  listPage: (
    limit: number,
    offset: number
  ) => Promise<{
    data: T[]
    total: number
  }>
) => {
  const rows: T[] = []
  let offset = 0

  for (;;) {
    const page = await listPage(EXPORT_PAGE, offset)
    rows.push(...page.data)
    if (rows.length >= page.total || page.data.length === 0) return rows
    offset += EXPORT_PAGE
  }
}

const invalidateExact = (
  queryClient: QueryClient,
  queryKey: readonly unknown[]
) => queryClient.invalidateQueries({ queryKey, exact: true })

const invalidateExactOnSuccess =
  (queryClient: QueryClient, queryKey: readonly unknown[]) => () =>
    invalidateExact(queryClient, queryKey)

const invalidateTreeOnSuccess =
  (queryClient: QueryClient, queryKey: readonly unknown[]) => () =>
    queryClient.invalidateQueries({ queryKey })

const updateNotificationReadCache = (
  queryClient: QueryClient,
  notificationsKey: readonly unknown[],
  unreadKey: readonly unknown[],
  readIds: string[]
) => {
  if (readIds.length === 0) return
  const ids = new Set(readIds)
  const readAt = new Date().toISOString()
  queryClient.setQueryData<{ data: MerchantNotification[] | null }>(
    notificationsKey,
    (current) =>
      current
        ? {
            ...current,
            data: (current.data ?? []).map((notification) =>
              ids.has(notification.id)
                ? { ...notification, read_at: readAt }
                : notification
            ),
          }
        : current
  )
  queryClient.setQueryData<{ unread: number }>(unreadKey, (current) =>
    current ? { unread: Math.max(0, current.unread - readIds.length) } : current
  )
}

// Every factory pins the selected merchant once, up front, with
// merchantQueryKeys(). Callbacks fire after the request returns, by which time
// the operator may have switched merchants, so a key built inside onSuccess /
// onSettled from the live queryKeys would name the wrong merchant: the
// initiating merchant's screens stay stale and an untouched merchant's cache is
// invalidated. Importing the live queryKeys here is blocked by lint.
export const adminMutations = {
  markNotificationRead: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const notificationsKey = keys.notifications()
    const unreadKey = [...notificationsKey, "unread-count"] as const
    return mutationOptions({
      mutationKey: [...notificationsKey, "mark-read"],
      mutationFn: (id: string) => markNotificationRead(id),
      onSuccess: (_result, id) =>
        updateNotificationReadCache(queryClient, notificationsKey, unreadKey, [
          id,
        ]),
    })
  },
  markNotificationsRead: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const notificationsKey = keys.notifications()
    const unreadKey = [...notificationsKey, "unread-count"] as const
    return mutationOptions({
      mutationKey: [...notificationsKey, "mark-all-read"],
      mutationFn: async (ids: string[]) => {
        const results = await Promise.allSettled(
          ids.map((id) => markNotificationRead(id))
        )
        return results.flatMap((result, index) =>
          result.status === "fulfilled" ? [ids[index]] : []
        )
      },
      onSuccess: (readIds) =>
        updateNotificationReadCache(
          queryClient,
          notificationsKey,
          unreadKey,
          readIds
        ),
    })
  },
  saveDashboard: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const dashboardKey = keys.dashboard()
    return mutationOptions({
      mutationKey: [...dashboardKey, "save"],
      mutationFn: (widgets: Widget[]) => putDashboard(widgets),
      onSuccess: (saved) => queryClient.setQueryData(dashboardKey, saved),
    })
  },
  askMetrics: () => {
    const keys = merchantQueryKeys()
    const dashboardKey = keys.dashboard()
    return mutationOptions({
      mutationKey: [...dashboardKey, "metrics", "ask"],
      mutationFn: (question: string) => askMetrics(question),
    })
  },
  generateDashboardWidget: () => {
    const keys = merchantQueryKeys()
    const dashboardKey = keys.dashboard()
    return mutationOptions({
      mutationKey: [...dashboardKey, "widgets", "generate"],
      mutationFn: ({
        prompt,
        baseQuery,
      }: {
        prompt: string
        baseQuery?: MetricsQuery
      }) => generateWidget(prompt, baseQuery),
    })
  },
  findCustomer: () => {
    const keys = merchantQueryKeys()
    const customersKey = keys.customers()
    return mutationOptions({
      mutationKey: [...customersKey, "find"],
      mutationFn: async (term: string) => {
        const result = await listCustomers(term, 1, 0)
        return result.data[0]
      },
    })
  },
  exportCustomers: () => {
    const keys = merchantQueryKeys()
    const customersKey = keys.customers()
    return mutationOptions({
      mutationKey: [...customersKey, "export"],
      mutationFn: (q: string) =>
        collectAllPages<CustomerSummary>((limit, offset) =>
          listCustomers(q, limit, offset)
        ),
    })
  },
  exportSubscriptions: () => {
    const keys = merchantQueryKeys()
    const subscriptionsKey = keys.subscriptions()
    return mutationOptions({
      mutationKey: [...subscriptionsKey, "export"],
      mutationFn: (filters: SubscriptionFilters) =>
        collectAllPages<AdminSubscription>((limit, offset) =>
          listSubscriptions(filters, limit, offset)
        ),
    })
  },
  exportPayments: () => {
    const keys = merchantQueryKeys()
    const paymentsKey = keys.payments()
    return mutationOptions({
      mutationKey: [...paymentsKey, "export"],
      mutationFn: (filters: PaymentFilters) =>
        collectAllPages<PaymentObject>((limit, offset) =>
          listPayments(filters, limit, offset)
        ),
    })
  },
  resolveFinding: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const opsKey = keys.ops()
    return mutationOptions({
      mutationKey: [...opsKey, "findings", "resolve"],
      mutationFn: ({
        id,
        outcome,
        notes,
      }: {
        id: string
        outcome: "approve" | "ignore"
        notes: string
      }) => resolveFinding(id, outcome, notes),
      onSuccess: invalidateTreeOnSuccess(queryClient, opsKey),
    })
  },
  refundPayment: (
    queryClient: QueryClient,
    paymentId: string,
    customerId?: string,
    subscriptionId?: string
  ) => {
    const keys = merchantQueryKeys()
    const paymentsKey = keys.payments()
    const customerKey = customerId ? keys.customer(customerId) : undefined
    const subscriptionKey = subscriptionId
      ? keys.subscription(subscriptionId)
      : undefined
    return mutationOptions({
      mutationKey: [...paymentsKey, paymentId, "refund"],
      mutationFn: ({
        amount,
        reason,
        revokeAccess,
      }: {
        amount: string
        reason: string
        revokeAccess: boolean
      }) => refundPayment(paymentId, amount, reason, revokeAccess),
      onSuccess: () =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: paymentsKey }),
          ...(customerKey
            ? [queryClient.invalidateQueries({ queryKey: customerKey })]
            : []),
          ...(subscriptionKey
            ? [queryClient.invalidateQueries({ queryKey: subscriptionKey })]
            : []),
        ]),
    })
  },
  cancelSubscription: (
    queryClient: QueryClient,
    subscriptionId: string,
    customerId?: string
  ) => {
    const keys = merchantQueryKeys()
    const subscriptionsKey = keys.subscriptions()
    const customerKey = customerId ? keys.customer(customerId) : undefined
    return mutationOptions({
      mutationKey: [...subscriptionsKey, subscriptionId, "cancel"],
      mutationFn: ({
        reason,
        revokeAccess,
      }: {
        reason: string
        revokeAccess: boolean
      }) => cancelSubscription(subscriptionId, reason, revokeAccess),
      onSuccess: () =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: subscriptionsKey }),
          ...(customerKey
            ? [queryClient.invalidateQueries({ queryKey: customerKey })]
            : []),
        ]),
    })
  },
  resumeSubscription: (
    queryClient: QueryClient,
    subscriptionId: string,
    customerId?: string
  ) => {
    const keys = merchantQueryKeys()
    const subscriptionsKey = keys.subscriptions()
    const customerKey = customerId ? keys.customer(customerId) : undefined
    return mutationOptions({
      mutationKey: [...subscriptionsKey, subscriptionId, "resume"],
      mutationFn: () => resumeSubscription(subscriptionId),
      onSuccess: () =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: subscriptionsKey }),
          ...(customerKey
            ? [queryClient.invalidateQueries({ queryKey: customerKey })]
            : []),
        ]),
    })
  },
  changeSubscriptionPaymentMethod: (
    queryClient: QueryClient,
    subscriptionId: string,
    customerId?: string
  ) => {
    const keys = merchantQueryKeys()
    const subscriptionsKey = keys.subscriptions()
    const customerKey = customerId ? keys.customer(customerId) : undefined
    return mutationOptions({
      mutationKey: [...subscriptionsKey, subscriptionId, "payment-method"],
      mutationFn: (paymentMethodId: string) =>
        changeSubscriptionPaymentMethod(subscriptionId, paymentMethodId),
      onSuccess: () =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: subscriptionsKey }),
          ...(customerKey
            ? [queryClient.invalidateQueries({ queryKey: customerKey })]
            : []),
        ]),
    })
  },
  previewSubscriptionTierChange: (subscriptionId: string) => {
    const keys = merchantQueryKeys()
    const subscriptionsKey = keys.subscriptions()
    return mutationOptions({
      mutationKey: [
        ...subscriptionsKey,
        subscriptionId,
        "change-tier",
        "preview",
      ],
      mutationFn: (priceId: string) =>
        previewSubscriptionTierChange(subscriptionId, priceId),
    })
  },
  changeSubscriptionTier: (
    queryClient: QueryClient,
    subscriptionId: string,
    customerId?: string
  ) => {
    const keys = merchantQueryKeys()
    const subscriptionsKey = keys.subscriptions()
    const customerKey = customerId ? keys.customer(customerId) : undefined
    const paymentsKey = keys.payments()
    return mutationOptions({
      mutationKey: [...subscriptionsKey, subscriptionId, "change-tier"],
      mutationFn: (change: { priceId: string; idempotencyKey: string }) =>
        changeSubscriptionTier(
          subscriptionId,
          change.priceId,
          change.idempotencyKey
        ),
      onSuccess: () =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: subscriptionsKey }),
          queryClient.invalidateQueries({ queryKey: paymentsKey }),
          ...(customerKey
            ? [queryClient.invalidateQueries({ queryKey: customerKey })]
            : []),
        ]),
    })
  },
  cancelSubscriptionReprice: (
    queryClient: QueryClient,
    subscriptionId: string
  ) => {
    const keys = merchantQueryKeys()
    const subscriptionsKey = keys.subscriptions()
    const catalogKey = keys.catalog()
    return mutationOptions({
      mutationKey: [...subscriptionsKey, subscriptionId, "reprices", "cancel"],
      mutationFn: (repriceId: string) => cancelReprice(repriceId),
      onSuccess: () =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: subscriptionsKey }),
          queryClient.invalidateQueries({ queryKey: catalogKey }),
        ]),
    })
  },
  grantCustomerEntitlement: (queryClient: QueryClient, customerId: string) => {
    const keys = merchantQueryKeys()
    const customerKey = keys.customer(customerId)
    return mutationOptions({
      mutationKey: [...customerKey, "entitlements", "grant"],
      mutationFn: ({
        entitlement,
        hours,
      }: {
        entitlement: string
        hours?: number
      }) => grantEntitlement(customerId, entitlement, hours),
      onSuccess: invalidateTreeOnSuccess(queryClient, customerKey),
    })
  },
  revokeCustomerEntitlement: (queryClient: QueryClient, customerId: string) => {
    const keys = merchantQueryKeys()
    const customerKey = keys.customer(customerId)
    return mutationOptions({
      mutationKey: [...customerKey, "entitlements", "revoke"],
      mutationFn: (entitlementId: string) =>
        revokeEntitlement(customerId, entitlementId),
      onSuccess: invalidateTreeOnSuccess(queryClient, customerKey),
    })
  },
  grantCustomerProductAccess: (
    queryClient: QueryClient,
    customerId: string
  ) => {
    const keys = merchantQueryKeys()
    const customerKey = keys.customer(customerId)
    return mutationOptions({
      mutationKey: [...customerKey, "product-access", "grant"],
      mutationFn: ({
        productId,
        endsAt,
      }: {
        productId: string
        endsAt?: string
      }) => grantProductAccess(customerId, productId, endsAt),
      onSuccess: invalidateTreeOnSuccess(queryClient, customerKey),
    })
  },
  revokeCustomerProductAccess: (
    queryClient: QueryClient,
    customerId: string
  ) => {
    const keys = merchantQueryKeys()
    const customerKey = keys.customer(customerId)
    return mutationOptions({
      mutationKey: [...customerKey, "product-access", "revoke"],
      mutationFn: (grantId: string) => revokeProductAccess(customerId, grantId),
      onSuccess: invalidateTreeOnSuccess(queryClient, customerKey),
    })
  },
  recordCustomerOffChannelPayment: (
    queryClient: QueryClient,
    customerId: string
  ) => {
    const keys = merchantQueryKeys()
    const customerKey = keys.customer(customerId)
    const paymentsKey = keys.payments()
    return mutationOptions({
      mutationKey: [...customerKey, "payments", "off-channel"],
      mutationFn: (payment: OffChannelPaymentRequest) =>
        createOffChannelPayment(customerId, payment),
      onSuccess: () =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: customerKey }),
          queryClient.invalidateQueries({ queryKey: paymentsKey }),
        ]),
    })
  },
  askCatalogCopilot: () => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "copilot", "ask"],
      mutationFn: (question: string) => askCatalogCopilot(question),
    })
  },
  loadCatalogPriceDraft: () => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "copilot", "load-price-draft"],
      mutationFn: async (priceKey: string) => {
        const price = await getPriceByKey(priceKey)
        const product = await getProduct(price.product_id)
        return { price, productName: product.display_name }
      },
    })
  },
  createCatalogDraftPrice: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "copilot", "create-price"],
      mutationFn: async ({
        draftId,
        price,
      }: {
        draftId: string
        price: CreatePriceDraft
      }) => {
        const created = await createPrice(price)
        void confirmCopilotDraft(draftId, "catalog_diff", price.key).catch(
          () => {}
        )
        return created
      },
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  publishCatalog: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const catalogKey = keys.catalog()
    return mutationOptions({
      mutationKey: [...catalogKey, "publish"],
      mutationFn: ({
        manifest,
        planOnly,
      }: {
        manifest: unknown
        planOnly: boolean
      }) =>
        publishCatalog(
          manifest,
          planOnly ? { plan_only: true } : { insert: true, overwrite: true }
        ),
      onSuccess: (_result, { planOnly }) => {
        if (!planOnly) {
          return queryClient.invalidateQueries({
            queryKey: catalogKey,
          })
        }
      },
    })
  },
  refreshCatalogDrift: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalogDrift(), "refresh"],
      mutationFn: () => refreshCatalogDrift(),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalogDrift()),
    })
  },
  createProduct: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "products", "create"],
      mutationFn: (product: ProductRequest) => createProduct(product),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  updateProduct: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "products", "update"],
      mutationFn: ({
        id,
        product,
      }: {
        id: string
        product: Partial<ProductRequest> & { set_entitlements?: boolean }
      }) => updateProduct(id, product),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  setProductActive: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "products", "set-active"],
      mutationFn: ({ id, active }: { id: string; active: boolean }) =>
        active ? activateProduct(id) : deactivateProduct(id),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  createPrice: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "prices", "create"],
      mutationFn: (price: PriceRequest) => createPrice(price),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  setPriceActive: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "prices", "set-active"],
      mutationFn: ({ id, active }: { id: string; active: boolean }) =>
        active ? activatePrice(id) : deactivatePrice(id),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  putUsageMeter: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const metersKey = keys.usageMeters()
    return mutationOptions({
      mutationKey: [...metersKey, "put"],
      mutationFn: ({ key, meter }: { key: string; meter: UsageMeterRequest }) =>
        putUsageMeter(key, meter),
      onSuccess: (_result, { key }) =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: metersKey }),
          queryClient.invalidateQueries({
            queryKey: keys.usageMeter(key),
          }),
        ]),
    })
  },
  putDefaultUsageRateCard: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const metersKey = keys.usageMeters()
    return mutationOptions({
      mutationKey: [...metersKey, "rate-card", "put"],
      mutationFn: ({
        key,
        rateCard,
      }: {
        key: string
        rateCard: DefaultUsageRateCardRequest
      }) => putDefaultUsageRateCard(key, rateCard),
      onSuccess: (_result, { key }) =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: metersKey }),
          queryClient.invalidateQueries({
            queryKey: keys.usageMeter(key),
          }),
        ]),
    })
  },
  deleteDefaultUsageRateCard: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const metersKey = keys.usageMeters()
    return mutationOptions({
      mutationKey: [...metersKey, "rate-card", "delete"],
      mutationFn: (key: string) => deleteDefaultUsageRateCard(key),
      onSuccess: (_result, key) =>
        Promise.all([
          queryClient.invalidateQueries({ queryKey: metersKey }),
          queryClient.invalidateQueries({
            queryKey: keys.usageMeter(key),
          }),
        ]),
    })
  },
  putCustomerUsageRateOverride: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const metersKey = keys.usageMeters()
    return mutationOptions({
      mutationKey: [...metersKey, "customer-override", "put"],
      mutationFn: ({
        customerId,
        meterKey,
        override,
      }: {
        customerId: string
        meterKey: string
        override: CustomerUsageRateOverrideRequest
      }) => putCustomerUsageRateOverride(customerId, meterKey, override),
      onSuccess: (_result, { customerId, meterKey }) =>
        Promise.all([
          queryClient.invalidateQueries({
            queryKey: keys.customer(customerId),
          }),
          queryClient.invalidateQueries({ queryKey: metersKey }),
          queryClient.invalidateQueries({
            queryKey: keys.usageMeter(meterKey),
          }),
          queryClient.invalidateQueries({
            queryKey: keys.dashboard(),
          }),
        ]),
    })
  },
  deleteCustomerUsageRateOverride: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    const metersKey = keys.usageMeters()
    return mutationOptions({
      mutationKey: [...metersKey, "customer-override", "delete"],
      mutationFn: ({
        customerId,
        meterKey,
      }: {
        customerId: string
        meterKey: string
      }) => deleteCustomerUsageRateOverride(customerId, meterKey),
      onSuccess: (_result, { customerId, meterKey }) =>
        Promise.all([
          queryClient.invalidateQueries({
            queryKey: keys.customer(customerId),
          }),
          queryClient.invalidateQueries({ queryKey: metersKey }),
          queryClient.invalidateQueries({
            queryKey: keys.usageMeter(meterKey),
          }),
          queryClient.invalidateQueries({
            queryKey: keys.dashboard(),
          }),
        ]),
    })
  },
  previewPriceChange: () => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "prices", "preview-change"],
      mutationFn: (priceKey: string) =>
        previewRepriceAllPriorVersions(priceKey),
    })
  },
  changePrice: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "prices", "change"],
      mutationFn: async ({
        price,
        migration,
        copilotDraftId,
      }: {
        price: PriceRequest
        migration?: { priceKey: string; effectiveAt: string }
        copilotDraftId?: string
      }) => {
        const created = await createPrice(price)
        if (migration) {
          await repriceAllPriorVersions(
            migration.priceKey,
            migration.effectiveAt
          )
        }
        if (copilotDraftId) {
          void confirmCopilotDraft(
            copilotDraftId,
            "price_change",
            price.key
          ).catch(() => {})
        }
        return created
      },
      // The price can be created before scheduling fails. Always refresh so
      // the UI reflects that partial server-side success.
      onSettled: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  cancelReprices: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "reprices", "cancel"],
      mutationFn: (repriceIds: string[]) =>
        Promise.all(repriceIds.map((id) => cancelReprice(id))),
      // A batch can be partially canceled before one request fails.
      onSettled: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  updateMerchantSettings: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "update"],
      mutationFn: (settings: MerchantSettings) => putMerchantSettings(settings),
      onSuccess: invalidateExactOnSuccess(queryClient, keys.settings()),
    })
  },
  savePaymentProvider: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "payment-providers", "save"],
      mutationFn: ({
        rail,
        provider,
      }: {
        rail: string
        provider: UpsertProviderRequest
      }) => putPaymentProvider(rail, provider),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...keys.settings(),
        "payment-providers",
      ]),
    })
  },
  archivePaymentProvider: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "payment-providers", "archive"],
      mutationFn: ({
        rail,
        id,
        allowLast,
      }: {
        rail: string
        id: string
        allowLast?: boolean
      }) => archivePaymentProviderAccount(rail, id, allowLast),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...keys.settings(),
        "payment-providers",
      ]),
    })
  },
  createApiKey: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "api-keys", "create"],
      mutationFn: ({ name, role }: { name: string; role: string }) =>
        createApiKey(name, role),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...keys.settings(),
        "api-keys",
      ]),
    })
  },
  revokeApiKey: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "api-keys", "revoke"],
      mutationFn: (id: string) => revokeApiKey(id),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...keys.settings(),
        "api-keys",
      ]),
    })
  },
  inviteTeamMember: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.team(), "invite"],
      mutationFn: ({ email, role }: { email: string; role: string }) =>
        inviteTeamMember(email, role),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.team()),
    })
  },
  revokeTeamInvite: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.team(), "invites", "revoke"],
      mutationFn: (id: string) => revokeTeamInvite(id),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.team()),
    })
  },
  changeTeamRole: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.team(), "role"],
      mutationFn: ({ userId, role }: { userId: string; role: string }) =>
        changeTeamRole(userId, role),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.team()),
    })
  },
  removeTeamMember: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.team(), "remove"],
      mutationFn: (userId: string) => removeTeamMember(userId),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.team()),
    })
  },
  setCreditLimit: () => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "customer-controls", "credit-limit"],
      mutationFn: ({
        customerId,
        currency,
        amount,
      }: {
        customerId: string
        currency: string
        amount: string
      }) => setCreditLimit(customerId, currency, amount),
    })
  },
  lookupCustomerControls: () => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "customer-controls", "lookup"],
      mutationFn: async ({
        customerId,
        currency,
      }: {
        customerId: string
        currency: string
      }) => {
        const [credit, trust] = await Promise.all([
          getCreditLimit(customerId, currency),
          getTrustLevel(customerId, currency),
        ])
        return { credit, trust }
      },
    })
  },
  createWebhook: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.alerts(), "webhooks", "create"],
      mutationFn: (webhook: WebhookRequest) => createWebhook(webhook),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...keys.alerts(),
        "webhooks",
      ]),
    })
  },
  rotateWebhookURL: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.alerts(), "webhooks", "rotate"],
      mutationFn: ({ id, url }: { id: string; url: string }) =>
        rotateWebhookURL(id, url),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...keys.alerts(),
        "webhooks",
      ]),
    })
  },
  deleteWebhook: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.alerts(), "webhooks", "delete"],
      mutationFn: (id: string) => deleteWebhook(id),
      onSuccess: invalidateExactOnSuccess(queryClient, [
        ...keys.alerts(),
        "webhooks",
      ]),
    })
  },
}
