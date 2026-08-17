// Per-endpoint client functions for /v1/merchant/*. Shapes match the Go
// handlers exactly — see src/lib/api/types.ts.
import { api, type ItemsEnvelope, type ListEnvelope } from "./client"
import type {
  AdminSubscription,
  AlertChannelRef,
  AlertDeliveryResult,
  AlertRule,
  AlertSeverity,
  AlertTemplate,
  AlertTemplateInfo,
  CatalogDriftEvent,
  CatalogDriftReport,
  CatalogPrice,
  CatalogProduct,
  CheckoutRoutingDecision,
  CustomerBillingProfile,
  CustomerSummary,
  Finding,
  FindingsListResponse,
  MerchantAPIKey,
  MerchantNotification,
  MerchantSettings,
  MerchantWebhook,
  MintedAPIKey,
  PaymentMethodResponse,
  PaymentObject,
  PaymentProviderConfig,
  PaymentProviderDefinition,
  PriceKeyHistoryEntry,
  RawEntitlement,
  RepairAlert,
  RepriceBatch,
  RepriceBatchResult,
  RepricePreviewResult,
  RepriceStatus,
  SubscriptionReprice,
  TierChangePreview,
  TierChangeResult,
  TeamInvite,
  TeamInviteResult,
  TeamMember,
  UsageAllowance,
  UsageMeter,
  UsageMeterOverride,
  UsageMeterPage,
  UsageRatePrice,
  WebhookFormat,
  WorkerHealth,
} from "./types"

// --- Customers ---

export const listCustomers = (
  q: string,
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<ListEnvelope<CustomerSummary>>("/merchant/customers", {
    query: { q, limit, offset },
    signal,
  })

export const getCustomerProfile = (customerId: string, signal?: AbortSignal) =>
  api<CustomerBillingProfile>(`/merchant/customers/${customerId}`, { signal })

export const getCustomerPaymentMethods = (
  customerId: string,
  signal?: AbortSignal
) =>
  api<{ object: "list"; data: PaymentMethodResponse[] }>(
    `/merchant/customers/${customerId}/payment-methods`,
    { signal }
  )

export const grantEntitlement = (
  customerId: string,
  entitlement: string,
  hours?: number
) =>
  api<RawEntitlement>(`/merchant/customers/${customerId}/entitlements`, {
    method: "POST",
    body: hours ? { entitlement, hours } : { entitlement },
  })

export const revokeEntitlement = (customerId: string, entitlementId: string) =>
  api<{ message: string }>(
    `/merchant/customers/${customerId}/entitlements/${entitlementId}`,
    {
      method: "DELETE",
    }
  )

export const grantProductAccess = (
  customerId: string,
  productId: string,
  endsAt?: string
) =>
  api<unknown>(`/merchant/customers/${customerId}/product-access`, {
    method: "POST",
    body: endsAt
      ? { product_id: productId, ends_at: endsAt }
      : { product_id: productId },
  })

export const revokeProductAccess = (customerId: string, grantId: string) =>
  api<{ message: string }>(
    `/merchant/customers/${customerId}/product-access/${grantId}`,
    {
      method: "DELETE",
    }
  )

export interface OffChannelPaymentRequest {
  price_id: string
  transaction_id: string
  amount?: number
  currency?: string
  purchased_at?: string
  discount_code?: string
  discount_reason?: string
}

export const createOffChannelPayment = (
  customerId: string,
  body: OffChannelPaymentRequest
) =>
  api<{ payment_id: string; status?: string; entitlements?: string[] }>(
    `/merchant/customers/${customerId}/payments/off-channel`,
    { method: "POST", body }
  )

// --- Subscriptions ---

export interface SubscriptionFilters {
  status?: string
  rail?: string
  user_id?: string
  price_id?: string
  sort_by?: string
  sort_order?: string
}

export const listSubscriptions = (
  filters: SubscriptionFilters,
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<ListEnvelope<AdminSubscription>>("/merchant/subscriptions", {
    query: { ...filters, limit, offset },
    signal,
  })

export const getSubscription = (id: string, signal?: AbortSignal) =>
  api<AdminSubscription>(`/merchant/subscriptions/${id}`, { signal })

export const cancelSubscription = (
  id: string,
  reason: string,
  revokeAccess: boolean
) =>
  api<{ message: string }>(`/merchant/subscriptions/${id}/cancel`, {
    method: "POST",
    body: { reason, revoke_access: revokeAccess },
  })

export const resumeSubscription = (id: string) =>
  api<{ status: string }>(`/merchant/subscriptions/${id}/resume`, {
    method: "POST",
    body: {},
  })

export const changeSubscriptionPaymentMethod = (
  id: string,
  paymentMethodId: string
) =>
  api<{ success: boolean; message: string }>(
    `/merchant/subscriptions/${id}/payment-method`,
    {
      method: "PUT",
      body: { payment_method_id: paymentMethodId },
    }
  )

export const previewSubscriptionTierChange = (id: string, priceId: string) =>
  api<TierChangePreview>(`/merchant/subscriptions/${id}/change-tier/preview`, {
    method: "POST",
    body: { price_id: priceId },
  })

export const changeSubscriptionTier = (id: string, priceId: string) =>
  api<TierChangeResult>(`/merchant/subscriptions/${id}/change-tier`, {
    method: "POST",
    body: { price_id: priceId },
  })

// --- Payments ---

export interface PaymentFilters {
  user_id?: string
  rail?: string
  subscription_id?: string
  transaction_id?: string
  refunds_only?: boolean
  sort_by?: string
  sort_order?: string
}

export const listPayments = (
  filters: PaymentFilters,
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<ListEnvelope<PaymentObject>>("/merchant/payments", {
    query: { ...filters, limit, offset },
    signal,
  })

export const getPayment = (id: string, signal?: AbortSignal) =>
  api<PaymentObject>(`/merchant/payments/${id}`, { signal })

export const refundPayment = (
  id: string,
  amount: number,
  reason: string,
  revokeAccess: boolean
) =>
  api<PaymentObject>(`/merchant/payments/${id}/refunds`, {
    method: "POST",
    headers: { "Idempotency-Key": crypto.randomUUID() },
    body: { amount, reason: reason || undefined, revoke_access: revokeAccess },
  })

// Rails whose refunds route through a provider API today (admin_payments.go).
export const REFUNDABLE_RAILS = ["nmi", "ccbill", "stripe"]

// --- Catalog ---

export const listProducts = (
  limit: number,
  offset: number,
  activeOnly?: boolean,
  signal?: AbortSignal
) =>
  api<ItemsEnvelope<CatalogProduct>>("/merchant/catalog/products", {
    query: { limit, offset, active_only: activeOnly },
    signal,
  })

export const getProduct = (id: string, signal?: AbortSignal) =>
  api<CatalogProduct>(`/merchant/catalog/products/${id}`, { signal })

export interface ProductRequest {
  key: string
  display_name: string
  description: string
  tier_group?: string
  tier_rank?: number
  entitlements_spec?: Record<string, number | null>
}

export const createProduct = (body: ProductRequest) =>
  api<CatalogProduct>("/merchant/catalog/products", { method: "POST", body })

export const updateProduct = (
  id: string,
  body: Partial<ProductRequest> & { set_entitlements?: boolean }
) =>
  api<CatalogProduct>(`/merchant/catalog/products/${id}`, {
    method: "PATCH",
    body,
  })

export const activateProduct = (id: string) =>
  api<CatalogProduct>(`/merchant/catalog/products/${id}/activate`, {
    method: "POST",
  })

export const deactivateProduct = (id: string) =>
  api<CatalogProduct>(`/merchant/catalog/products/${id}/deactivate`, {
    method: "POST",
  })

export interface UsageMeterRequest {
  event_type: string
  value_property: string
  aggregation: "sum" | "count"
  unit?: string
  group_by: Record<string, string>
}

export interface DefaultUsageRateCardRequest {
  product_id: string
  filter: Record<string, string[]>
  price: UsageRatePrice
  allowance?: UsageAllowance
}

export const listUsageMeters = (
  limit = 200,
  offset = 0,
  signal?: AbortSignal
) =>
  api<UsageMeterPage>("/merchant/catalog/meters", {
    query: { limit, offset },
    signal,
  })

export const getUsageMeter = (key: string, signal?: AbortSignal) =>
  api<UsageMeter>(`/merchant/catalog/meters/${encodeURIComponent(key)}`, {
    signal,
  })

export const listUsageMeterOverrides = (
  key: string,
  limit = 200,
  offset = 0,
  signal?: AbortSignal
) =>
  api<ItemsEnvelope<UsageMeterOverride>>(
    `/merchant/catalog/meters/${encodeURIComponent(key)}/overrides`,
    { query: { limit, offset }, signal }
  )

export const putUsageMeter = (key: string, body: UsageMeterRequest) =>
  api<UsageMeter>(`/merchant/catalog/meters/${encodeURIComponent(key)}`, {
    method: "PUT",
    body,
  })

export const putDefaultUsageRateCard = (
  key: string,
  body: DefaultUsageRateCardRequest
) =>
  api<UsageMeter>(
    `/merchant/catalog/meters/${encodeURIComponent(key)}/rate-card`,
    { method: "PUT", body }
  )

export const deleteDefaultUsageRateCard = (key: string) =>
  api<void>(`/merchant/catalog/meters/${encodeURIComponent(key)}/rate-card`, {
    method: "DELETE",
  })

export const listPrices = (productId?: string, signal?: AbortSignal) =>
  api<ItemsEnvelope<CatalogPrice>>("/merchant/catalog/prices", {
    query: productId ? { product_id: productId } : { limit: 1000 },
    signal,
  })

export interface PriceRequest {
  product_id: string
  unit_amount: number
  currency: string
  access_duration_hours?: number
  auto_renew?: boolean
  trial_unit_amount?: number
  trial_duration_hours?: number
  // Key (#774): declaring the SAME key as an existing live price with a
  // DIFFERENT amount is a version bump (the #777 wizard's whole mechanism) —
  // omit to auto-default.
  key?: string
  // Providers to (re)attach — e.g. carried over from the price being
  // replaced so the new version doesn't silently lose its Stripe/CCBill/NMI
  // links. Empty/omitted = DB-only price.
  providers?: string[]
}

export const createPrice = (body: PriceRequest) =>
  api<CatalogPrice>("/merchant/catalog/prices", { method: "POST", body })

// getPrice returns the price with its psp_links projection (`providers`).
// verify=true additionally performs a LIVE retrieve against every attached
// provider and fills in sync_status/drift — a read, never a write, and slow
// enough that it stays opt-in (or#812).
export const getPrice = (id: string, verify = false, signal?: AbortSignal) =>
  api<CatalogPrice>(`/merchant/catalog/prices/${id}`, {
    query: verify ? { verify: true } : undefined,
    signal,
  })

export const getPriceByKey = (key: string) =>
  api<CatalogPrice>(
    `/merchant/catalog/prices/by-key/${encodeURIComponent(key)}`
  )

export const activatePrice = (id: string) =>
  api<CatalogPrice>(`/merchant/catalog/prices/${id}/activate`, {
    method: "POST",
  })

export const deactivatePrice = (id: string) =>
  api<CatalogPrice>(`/merchant/catalog/prices/${id}/deactivate`, {
    method: "POST",
  })

// getPriceKeyHistory returns a price key's version chain (most-recent-first),
// resolved server-side from the #774 pointer-movement log — the price
// detail page's "version chain with dates" (#777).
export const getPriceKeyHistory = (key: string, signal?: AbortSignal) =>
  api<ItemsEnvelope<PriceKeyHistoryEntry>>(
    `/merchant/catalog/prices/by-key/${encodeURIComponent(key)}/history`,
    { signal }
  )

// --- Repricing / migration (#773 primitive, #777 console wizard) ---

// previewRepriceAllPriorVersions is the wizard's Step 2 affected-count
// preview: a READ-ONLY dry run, called BEFORE the price edit lands (so it
// never mutates, unlike repriceAllPriorVersions below).
export const previewRepriceAllPriorVersions = (priceKey: string) =>
  api<RepricePreviewResult>(
    "/merchant/catalog/reprice-all-prior-versions/preview",
    {
      query: { price_key: priceKey },
    }
  )

// repriceAllPriorVersions bulk-schedules every active subscription pinned to
// a prior version of priceKey to move to its current price at effectiveAt.
export const repriceAllPriorVersions = (
  priceKey: string,
  effectiveAt: string
) =>
  api<RepriceBatchResult>("/merchant/catalog/reprice-all-prior-versions", {
    method: "POST",
    body: { price_key: priceKey, effective_at: effectiveAt },
  })

// listRepriceBatchesByKey finds a price key's bulk reprice operations
// (most recent first) — the price page's pending-migration lookup, without
// already knowing a batch id.
export const listRepriceBatchesByKey = (
  priceKey: string,
  limit = 20,
  signal?: AbortSignal
) =>
  api<ItemsEnvelope<RepriceBatch>>("/merchant/reprices/batches", {
    query: { price_key: priceKey, limit },
    signal,
  })

export interface RepriceFilters {
  subscription_id?: string
  reprice_batch_id?: string
  status?: RepriceStatus
}

export const listReprices = (
  filters: RepriceFilters,
  limit = 100,
  offset = 0,
  signal?: AbortSignal
) =>
  api<ItemsEnvelope<SubscriptionReprice>>("/merchant/reprices", {
    query: { ...filters, limit, offset },
    signal,
  })

export const cancelReprice = (id: string) =>
  api<{ message: string }>(`/merchant/reprices/${id}/cancel`, {
    method: "POST",
  })

export const publishCatalog = (
  manifest: unknown,
  opts: {
    plan_only?: boolean
    insert?: boolean
    overwrite?: boolean
    prune?: boolean
  }
) =>
  api<{ plan: unknown; result?: unknown }>("/merchant/catalog/publish", {
    method: "POST",
    body: { catalog: manifest, ...opts },
  })

export const listCatalogDrift = (
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<ItemsEnvelope<CatalogDriftEvent>>("/merchant/catalog/drift", {
    query: { limit, offset },
    signal,
  })

export const refreshCatalogDrift = () =>
  api<CatalogDriftReport>("/merchant/catalog/drift/refresh", { method: "POST" })

// --- Ops ---

export const listFindings = (
  filters: { status?: string; severity?: string },
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<FindingsListResponse>("/merchant/findings", {
    query: { ...filters, limit, offset },
    signal,
  })

export const getFinding = (id: string) =>
  api<Finding>(`/merchant/findings/${id}`)

export const resolveFinding = (
  id: string,
  outcome: "approve" | "ignore",
  notes: string
) =>
  api<{ finding: Finding; execution?: Record<string, unknown> }>(
    `/merchant/findings/${id}/resolve`,
    { method: "POST", body: { outcome, notes } }
  )

export const listRepairAlerts = (
  limit: number,
  offset: number,
  signal?: AbortSignal
) =>
  api<ListEnvelope<RepairAlert>>("/merchant/repair-alerts", {
    query: { limit, offset },
    signal,
  })

export const listWorkerHealth = (signal?: AbortSignal) =>
  api<WorkerHealth[]>("/merchant/worker-health", { signal })

// --- Settings ---

export const getMerchantSettings = (signal?: AbortSignal) =>
  api<MerchantSettings>("/merchant/settings", { signal })

export const putMerchantSettings = (body: MerchantSettings) =>
  api<{ message: string }>("/merchant/settings", { method: "PUT", body })

export const listPaymentProviders = (signal?: AbortSignal) =>
  api<{
    data: PaymentProviderConfig[]
    provider_definitions: PaymentProviderDefinition[]
  }>("/merchant/payment-providers", { signal })

// #882: no `environment` — it is derived from the deployment's test_mode.
export interface UpsertProviderRequest {
  account_id: string
  public_config?: Record<string, string>
  credentials?: Record<string, string>
}

export const putPaymentProvider = (rail: string, body: UpsertProviderRequest) =>
  api<{ payment_provider: PaymentProviderConfig }>(
    `/merchant/payment-providers/${rail}`,
    {
      method: "PUT",
      body,
    }
  )

// dryRunCheckoutRouting (or#288) explains which PSP a checkout for this price
// would land on and why each other candidate was passed over. Read-only: it
// runs the production decision path without creating a session.
export const dryRunCheckoutRouting = (
  body: {
    price_id: string
    country?: string
    selector?: string
  },
  signal?: AbortSignal
) =>
  api<CheckoutRoutingDecision>("/merchant/payment-providers/routing/dry-run", {
    method: "POST",
    body,
    signal,
  })

export const deletePaymentProvider = (rail: string, environment?: string) =>
  api<{ payment_provider: PaymentProviderConfig }>(
    `/merchant/payment-providers/${rail}`,
    {
      method: "DELETE",
      query: environment ? { environment } : undefined,
    }
  )

// --- API keys (#757) ---

export const listApiKeys = (signal?: AbortSignal) =>
  api<{ data: MerchantAPIKey[] | null }>("/merchant/api-keys", { signal })

export const createApiKey = (name: string, role: string) =>
  api<MintedAPIKey>("/merchant/api-keys", {
    method: "POST",
    body: { name, role },
  })

export const revokeApiKey = (id: string) =>
  api<{ revoked: boolean; id: string }>(`/merchant/api-keys/${id}`, {
    method: "DELETE",
  })

// --- Team management (#760) ---

export const listTeam = (signal?: AbortSignal) =>
  api<{ data: TeamMember[] | null }>("/merchant/team", { signal })

export const listTeamInvites = (signal?: AbortSignal) =>
  api<{ data: TeamInvite[] | null; invites_enabled: boolean }>(
    "/merchant/team/invites",
    { signal }
  )

export const inviteTeamMember = (email: string, role: string) =>
  api<TeamInviteResult>("/merchant/team/invites", {
    method: "POST",
    body: { email, role },
  })

export const revokeTeamInvite = (id: string) =>
  api<{ revoked: boolean; id: string }>(`/merchant/team/invites/${id}`, {
    method: "DELETE",
  })

export const changeTeamRole = (userId: string, role: string) =>
  api<{ user_id: string; role: string }>(`/merchant/team/${userId}`, {
    method: "PATCH",
    body: { role },
  })

export const removeTeamMember = (userId: string) =>
  api<{ removed: boolean; user_id: string }>(`/merchant/team/${userId}`, {
    method: "DELETE",
  })

export const getCreditLimit = (customerId: string, currency: string) =>
  api<{ currency: string; credit_limit_amount: number }>(
    "/merchant/credit-limit",
    {
      query: { customer_id: customerId, currency },
    }
  )

export const setCreditLimit = (
  customerId: string,
  currency: string,
  amount: number
) =>
  api<{ message: string }>("/merchant/credit-limit", {
    method: "PUT",
    body: { customer_id: customerId, currency, credit_limit_amount: amount },
  })

export const getTrustLevel = (customerId: string, currency: string) =>
  api<{ currency: string; trust_level: string }>("/merchant/trust-level", {
    query: { customer_id: customerId, currency },
  })

// --- Alerting: rule templates (#736) ---

// listAlertTemplates fetches the in-code template registry (key, param schema,
// defaults) the create/edit dialog renders its fields from.
export const listAlertTemplates = (signal?: AbortSignal) =>
  api<{ data: AlertTemplateInfo[] }>("/merchant/alerts/templates", { signal })

// --- Alerting: rules (#736) ---

export interface AlertRuleRequest {
  template: AlertTemplate
  params: Record<string, unknown>
  severity: AlertSeverity
  channels: AlertChannelRef[]
  enabled: boolean
}

export const listAlertRules = (signal?: AbortSignal) =>
  api<{ data: AlertRule[] | null }>("/merchant/alerts/rules", { signal })

export const createAlertRule = (body: AlertRuleRequest) =>
  api<AlertRule>("/merchant/alerts/rules", { method: "POST", body })

export const updateAlertRule = (id: string, body: Partial<AlertRuleRequest>) =>
  api<AlertRule>(`/merchant/alerts/rules/${id}`, { method: "PATCH", body })

export const deleteAlertRule = (id: string) =>
  api<{ deleted: boolean; id: string }>(`/merchant/alerts/rules/${id}`, {
    method: "DELETE",
  })

// testAlertRule fires one test delivery through the rule's real channels —
// the ONLY test-fire surface (there is no per-webhook test endpoint).
export const testAlertRule = (id: string) =>
  api<{ results: AlertDeliveryResult[] }>(`/merchant/alerts/rules/${id}/test`, {
    method: "POST",
    body: {},
  })

// --- Alerting: webhooks (#736) ---

export interface WebhookRequest {
  name: string
  url: string
  format: WebhookFormat
  enabled?: boolean
}

export const listWebhooks = (signal?: AbortSignal) =>
  api<{ data: MerchantWebhook[] | null }>("/merchant/webhooks", { signal })

export const createWebhook = (body: WebhookRequest) =>
  api<MerchantWebhook>("/merchant/webhooks", { method: "POST", body })

export const deleteWebhook = (id: string) =>
  api<{ deleted: boolean; id: string }>(`/merchant/webhooks/${id}`, {
    method: "DELETE",
  })

// --- Alerting: notifications (in_app store / header bell, #736) ---

export const listNotifications = (unread?: boolean, signal?: AbortSignal) =>
  api<{ data: MerchantNotification[] | null }>("/merchant/notifications", {
    query: unread !== undefined ? { unread } : undefined,
    signal,
  })

export const markNotificationRead = (id: string) =>
  api<{ read: boolean; id: string }>(`/merchant/notifications/${id}/read`, {
    method: "POST",
    body: {},
  })

export const getUnreadCount = (signal?: AbortSignal) =>
  api<{ unread: number }>("/merchant/notifications/unread-count", { signal })
