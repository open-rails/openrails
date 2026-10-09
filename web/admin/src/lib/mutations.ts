import { collectCursorPages, selectedMerchant } from "@/lib/api/client"
import type {
  CreatePriceParams,
  UpdateProductParams,
} from "@/lib/api/generated/wire"
import { mutationOptions, type QueryClient } from "@tanstack/react-query"

import { askCatalog } from "@/lib/api/copilot"
import {
  cancelReprice,
  cancelRepriceBatch,
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
  archivePSP,
  createPSP,
  deleteDefaultUsageRateCard,
  deleteCustomerUsageRateOverride,
  deleteWebhook,
  getCustomerSettings,
  getPriceByKey,
  getProduct,
  grantProductAccess,
  inviteTeamMember,
  listCustomers,
  listPayments,
  listSubscriptions,
  markNotificationsRead,
  applyMerchantSettings,
  putDefaultUsageRateCard,
  putCustomerUsageRateOverride,
  updatePSP,
  putUsageMeter,
  previewRepriceBatch,
  previewSubscriptionTierChange,
  applyCatalog,
  refreshCatalogDrift,
  refundPayment,
  removeTeamMember,
  createRepriceBatch,
  resolveFinding,
  resumeSubscription,
  revokeApiKey,
  revokeProductAccess,
  revokeTeamInvite,
  updateCustomerSettings,
  updatePrice,
  updateProduct,
  type DefaultUsageRateCardRequest,
  type CustomerUsageRateOverrideRequest,
  type PaymentFilters,
  type ProductGrant,
  type ProductRequest,
  type UsageMeterRequest,
  type SubscriptionFilters,
  type CreatePSPRequest,
  type UpdatePSPRequest,
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
  CreateOffChannelPaymentParams,
  Customer,
} from "@/lib/api/generated/wire"
import type {
  MerchantSettings,
  MerchantNotification,
  AdminSubscription,
} from "@/lib/api/types"
import { merchantQueryKeys } from "@/lib/queries"

const EXPORT_PAGE = 200
// MAX_BATCH_ITEMS is billing.MaxBatchItems: one batch write's bound.
const MAX_BATCH_ITEMS = 100

const collectAllCursorPages = async <T>(
  listPage: (
    limit: number,
    cursor?: string
  ) => Promise<{ data: T[]; next_cursor: string | null }>
) => {
  const rows: T[] = []
  let cursor: string | undefined
  for (;;) {
    const page = await listPage(EXPORT_PAGE, cursor)
    rows.push(...page.data)
    if (!page.next_cursor || page.data.length === 0) return rows
    cursor = page.next_cursor
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
  queryClient.setQueryData<{ unread_count: number }>(unreadKey, (current) =>
    current
      ? { unread_count: Math.max(0, current.unread_count - readIds.length) }
      : current
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
      mutationFn: (id: string) => markNotificationsRead([id]),
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
        const batches = []
        for (let i = 0; i < ids.length; i += MAX_BATCH_ITEMS)
          batches.push(ids.slice(i, i + MAX_BATCH_ITEMS))
        const results = await Promise.allSettled(
          batches.map((batch) => markNotificationsRead(batch))
        )
        return results.flatMap((result) =>
          result.status === "fulfilled"
            ? Object.entries(result.value.notifications).flatMap(
                ([id, note]) => (note ? [id] : [])
              )
            : []
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
        const result = await listCustomers(term, 1, "")
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
        collectAllCursorPages<Customer>((limit, cursor) =>
          listCustomers(q, limit, cursor ?? "")
        ),
    })
  },
  exportSubscriptions: () => {
    const keys = merchantQueryKeys()
    const subscriptionsKey = keys.subscriptions()
    return mutationOptions({
      mutationKey: [...subscriptionsKey, "export"],
      mutationFn: (filters: SubscriptionFilters) =>
        collectAllCursorPages<AdminSubscription>((limit, cursor) =>
          listSubscriptions(filters, limit, cursor)
        ),
    })
  },
  exportPayments: () => {
    const keys = merchantQueryKeys()
    const paymentsKey = keys.payments()
    return mutationOptions({
      mutationKey: [...paymentsKey, "export"],
      mutationFn: (filters: PaymentFilters) =>
        collectCursorPages((cursor) =>
          listPayments(filters, { limit: EXPORT_PAGE, cursor })
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
  grantCustomerProductAccess: (
    queryClient: QueryClient,
    customerId: string
  ) => {
    const keys = merchantQueryKeys()
    const customerKey = keys.customer(customerId)
    return mutationOptions({
      mutationKey: [...customerKey, "product-access", "grant"],
      mutationFn: (grant: ProductGrant) => grantProductAccess(customerId, grant),
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
      mutationFn: (payment: CreateOffChannelPaymentParams) =>
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
      mutationFn: (question: string) => askCatalog(question),
    })
  },
  loadCatalogPriceDraft: () => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "copilot", "load-price-draft"],
      mutationFn: async ({
        productKey,
        priceKey,
      }: {
        productKey: string
        priceKey: string
      }) => {
        const price = await getPriceByKey(productKey, priceKey)
        const product = await getProduct(price.product_id)
        return { price, productName: product.display_name, productKey: product.key }
      },
    })
  },
  createCatalogDraftPrice: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "copilot", "create-price"],
      mutationFn: (price: CreatePriceParams) => createPrice(price),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  applyCatalog: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "apply"],
      mutationFn: (document: string) => applyCatalog(document),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
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
        product: UpdateProductParams
      }) => updateProduct(id, product),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  setProductActive: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "products", "set-active"],
      mutationFn: ({ id, active }: { id: string; active: boolean }) =>
        updateProduct(id, { archived: !active }),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  createPrice: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "prices", "create"],
      mutationFn: (price: CreatePriceParams) => createPrice(price),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  setPriceActive: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "prices", "set-active"],
      mutationFn: ({ id, active }: { id: string; active: boolean }) =>
        updatePrice(id, { archived: !active }),
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
      mutationFn: ({
        productKey,
        priceKey,
      }: {
        productKey: string
        priceKey: string
      }) => previewRepriceBatch(productKey, priceKey),
    })
  },
  changePrice: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "prices", "change"],
      mutationFn: async ({
        price,
        migration,
      }: {
        price: CreatePriceParams
        migration?: {
          productKey: string
          priceKey: string
          effectiveAt: string
        }
      }) => {
        const created = await createPrice(price)
        if (migration) {
          await createRepriceBatch(
            migration.productKey,
            migration.priceKey,
            migration.effectiveAt
          )
        }
        return created
      },
      // The price can be created before scheduling fails. Always refresh so
      // the UI reflects that partial server-side success.
      onSettled: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  cancelRepriceBatch: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.catalog(), "reprice-batches", "cancel"],
      mutationFn: (batchId: string) => cancelRepriceBatch(batchId),
      onSuccess: invalidateTreeOnSuccess(queryClient, keys.catalog()),
    })
  },
  updateMerchantSettings: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "update"],
      mutationFn: ({ revision, settings }: { revision: string; settings: MerchantSettings }) =>
        applyMerchantSettings(revision, settings),
      // A refused revision means the form is stale: reload either way.
      onSettled: invalidateExactOnSuccess(queryClient, keys.settings()),
    })
  },
  createPSP: (queryClient: QueryClient) => {
    const merchant = selectedMerchant()
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "psps", "create"],
      retry: false,
      gcTime: 0,
      mutationFn: (psp: CreatePSPRequest) => {
        if (selectedMerchant() !== merchant) throw new Error("Merchant changed; reopen this form before saving")
        return createPSP(psp)
      },
      onSuccess: invalidateExactOnSuccess(queryClient, [...keys.settings(), "psps"]),
    })
  },
  updatePSP: (queryClient: QueryClient) => {
    const merchant = selectedMerchant()
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "psps", "update"],
      retry: false,
      gcTime: 0,
      mutationFn: ({ id, psp }: { id: string; psp: UpdatePSPRequest }) => {
        if (selectedMerchant() !== merchant) throw new Error("Merchant changed; reopen this form before saving")
        return updatePSP(id, psp)
      },
      onSuccess: invalidateExactOnSuccess(queryClient, [...keys.settings(), "psps"]),
    })
  },
  archivePSP: (queryClient: QueryClient) => {
    const keys = merchantQueryKeys()
    return mutationOptions({
      mutationKey: [...keys.settings(), "psps", "archive"],
      mutationFn: ({ id, allowLast }: { id: string; allowLast?: boolean }) =>
        archivePSP(id, allowLast),
      onSuccess: invalidateExactOnSuccess(queryClient, [...keys.settings(), "psps"]),
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
      }) =>
        updateCustomerSettings([
          { customer_id: customerId, credit_limits: [{ currency, amount }] },
        ]),
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
        const settings = await getCustomerSettings(customerId)
        if (!settings) throw new Error("No customer has this ID")
        const code = currency.toUpperCase()
        return {
          currency: code,
          creditLimit:
            settings.credit_limits.find((l) => l.currency === code)?.amount ??
            "0",
          trustLevel:
            settings.trust_levels.find((l) => l.currency === code)
              ?.trust_level ?? "",
        }
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
