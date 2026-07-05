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
  RawEntitlement,
  RepairAlert,
  WebhookFormat,
  WorkerHealth,
} from "./types"

// --- Customers ---

export const listCustomers = (q: string, limit: number, offset: number) =>
  api<ListEnvelope<CustomerSummary>>("/merchant/customers", { query: { q, limit, offset } })

export const getCustomerProfile = (customerId: string) =>
  api<CustomerBillingProfile>(`/merchant/customers/${customerId}`)

export const getCustomerPaymentMethods = (customerId: string) =>
  api<{ object: "list"; data: PaymentMethodResponse[] }>(
    `/merchant/customers/${customerId}/payment-methods`,
  )

export const grantEntitlement = (customerId: string, entitlement: string, hours?: number) =>
  api<RawEntitlement>(`/merchant/customers/${customerId}/entitlements`, {
    method: "POST",
    body: hours ? { entitlement, hours } : { entitlement },
  })

export const revokeEntitlement = (customerId: string, entitlementId: string) =>
  api<{ message: string }>(`/merchant/customers/${customerId}/entitlements/${entitlementId}`, {
    method: "DELETE",
  })

export const grantProductAccess = (customerId: string, productId: string, endsAt?: string) =>
  api<unknown>(`/merchant/customers/${customerId}/product-access`, {
    method: "POST",
    body: endsAt ? { product_id: productId, ends_at: endsAt } : { product_id: productId },
  })

export const revokeProductAccess = (customerId: string, grantId: string) =>
  api<{ message: string }>(`/merchant/customers/${customerId}/product-access/${grantId}`, {
    method: "DELETE",
  })

export interface OffChannelPaymentRequest {
  price_id: string
  transaction_id: string
  amount?: number
  currency?: string
  purchased_at?: string
  discount_code?: string
  discount_reason?: string
}

export const createOffChannelPayment = (customerId: string, body: OffChannelPaymentRequest) =>
  api<{ payment_id: string; status?: string; entitlements?: string[] }>(
    `/merchant/customers/${customerId}/payments/off-channel`,
    { method: "POST", body },
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

export const listSubscriptions = (filters: SubscriptionFilters, limit: number, offset: number) =>
  api<ListEnvelope<AdminSubscription>>("/merchant/subscriptions", {
    query: { ...filters, limit, offset },
  })

export const getSubscription = (id: string) => api<AdminSubscription>(`/merchant/subscriptions/${id}`)

export const cancelSubscription = (id: string, reason: string, revokeAccess: boolean) =>
  api<{ message: string }>(`/merchant/subscriptions/${id}/cancel`, {
    method: "POST",
    body: { reason, revoke_access: revokeAccess },
  })

export const resumeSubscription = (id: string) =>
  api<{ status: string }>(`/merchant/subscriptions/${id}/resume`, { method: "POST", body: {} })

export const changeSubscriptionPaymentMethod = (id: string, paymentMethodId: string) =>
  api<{ success: boolean; message: string }>(`/merchant/subscriptions/${id}/payment-method`, {
    method: "PUT",
    body: { payment_method_id: paymentMethodId },
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

export const listPayments = (filters: PaymentFilters, limit: number, offset: number) =>
  api<ListEnvelope<PaymentObject>>("/merchant/payments", {
    query: { ...filters, limit, offset },
  })

export const getPayment = (id: string) => api<PaymentObject>(`/merchant/payments/${id}`)

export const refundPayment = (id: string, amount: number, reason: string, revokeAccess: boolean) =>
  api<PaymentObject>(`/merchant/payments/${id}/refunds`, {
    method: "POST",
    headers: { "X-Idempotency-Key": crypto.randomUUID() },
    body: { amount, reason: reason || undefined, revoke_access: revokeAccess },
  })

// Rails whose refunds route through a provider API today (admin_payments.go).
export const REFUNDABLE_RAILS = ["nmi", "ccbill", "stripe"]

// --- Catalog ---

export const listProducts = (limit: number, offset: number, activeOnly?: boolean) =>
  api<ItemsEnvelope<CatalogProduct>>("/merchant/catalog/products", {
    query: { limit, offset, active_only: activeOnly },
  })

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
  body: Partial<ProductRequest> & { set_entitlements?: boolean },
) => api<CatalogProduct>(`/merchant/catalog/products/${id}`, { method: "PATCH", body })

export const activateProduct = (id: string) =>
  api<CatalogProduct>(`/merchant/catalog/products/${id}/activate`, { method: "POST" })

export const deactivateProduct = (id: string) =>
  api<CatalogProduct>(`/merchant/catalog/products/${id}/deactivate`, { method: "POST" })

export const listPrices = (productId?: string) =>
  api<ItemsEnvelope<CatalogPrice>>("/merchant/catalog/prices", {
    query: productId ? { product_id: productId } : { limit: 1000 },
  })

export interface PriceRequest {
  product_id: string
  unit_amount: number
  currency: string
  access_duration_hours?: number
  auto_renew?: boolean
  trial_unit_amount?: number
  trial_duration_hours?: number
}

export const createPrice = (body: PriceRequest) =>
  api<CatalogPrice>("/merchant/catalog/prices", { method: "POST", body })

export const activatePrice = (id: string) =>
  api<CatalogPrice>(`/merchant/catalog/prices/${id}/activate`, { method: "POST" })

export const deactivatePrice = (id: string) =>
  api<CatalogPrice>(`/merchant/catalog/prices/${id}/deactivate`, { method: "POST" })

export const publishCatalog = (manifest: unknown, opts: { plan_only?: boolean; insert?: boolean; overwrite?: boolean; prune?: boolean }) =>
  api<{ plan: unknown; result?: unknown }>("/merchant/catalog/publish", {
    method: "POST",
    body: { catalog: manifest, ...opts },
  })

export const listCatalogDrift = (limit: number, offset: number) =>
  api<ItemsEnvelope<CatalogDriftEvent>>("/merchant/catalog/drift", { query: { limit, offset } })

export const refreshCatalogDrift = () =>
  api<CatalogDriftReport>("/merchant/catalog/drift/refresh", { method: "POST" })

// --- Ops ---

export const listFindings = (
  filters: { status?: string; severity?: string },
  limit: number,
  offset: number,
) => api<FindingsListResponse>("/merchant/findings", { query: { ...filters, limit, offset } })

export const getFinding = (id: string) => api<Finding>(`/merchant/findings/${id}`)

export const resolveFinding = (id: string, outcome: "approve" | "ignore", notes: string) =>
  api<{ finding: Finding; execution?: Record<string, unknown> }>(
    `/merchant/findings/${id}/resolve`,
    { method: "POST", body: { outcome, notes } },
  )

export const listRepairAlerts = (limit: number, offset: number) =>
  api<ListEnvelope<RepairAlert>>("/merchant/repair-alerts", { query: { limit, offset } })

export const listWorkerHealth = () => api<WorkerHealth[]>("/merchant/worker-health")

// --- Settings ---

export const getMerchantSettings = () => api<MerchantSettings>("/merchant/settings")

export const putMerchantSettings = (body: MerchantSettings) =>
  api<{ message: string }>("/merchant/settings", { method: "PUT", body })

export const listPaymentProviders = () =>
  api<{ data: PaymentProviderConfig[] }>("/merchant/payment-providers")

export interface UpsertProviderRequest {
  environment?: string
  account_id: string
  public_config?: Record<string, string>
  credentials?: Record<string, string>
}

export const putPaymentProvider = (rail: string, body: UpsertProviderRequest) =>
  api<{ payment_provider: PaymentProviderConfig }>(`/merchant/payment-providers/${rail}`, {
    method: "PUT",
    body,
  })

export const deletePaymentProvider = (rail: string, environment?: string) =>
  api<{ payment_provider: PaymentProviderConfig }>(`/merchant/payment-providers/${rail}`, {
    method: "DELETE",
    query: environment ? { environment } : undefined,
  })

// --- API keys (#757) ---

export const listApiKeys = () => api<{ data: MerchantAPIKey[] | null }>("/merchant/api-keys")

export const createApiKey = (name: string, role: string) =>
  api<MintedAPIKey>("/merchant/api-keys", { method: "POST", body: { name, role } })

export const revokeApiKey = (id: string) =>
  api<{ revoked: boolean; id: string }>(`/merchant/api-keys/${id}`, { method: "DELETE" })

export const getCreditLimit = (customerId: string, currency: string) =>
  api<{ currency: string; credit_limit_amount: number }>("/merchant/credit-limit", {
    query: { customer_id: customerId, currency },
  })

export const setCreditLimit = (customerId: string, currency: string, amount: number) =>
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
export const listAlertTemplates = () =>
  api<{ data: AlertTemplateInfo[] }>("/merchant/alerts/templates")

// --- Alerting: rules (#736) ---

export interface AlertRuleRequest {
  template: AlertTemplate
  params: Record<string, unknown>
  severity: AlertSeverity
  channels: AlertChannelRef[]
  enabled: boolean
}

export const listAlertRules = () => api<{ data: AlertRule[] | null }>("/merchant/alerts/rules")

export const createAlertRule = (body: AlertRuleRequest) =>
  api<AlertRule>("/merchant/alerts/rules", { method: "POST", body })

export const updateAlertRule = (id: string, body: Partial<AlertRuleRequest>) =>
  api<AlertRule>(`/merchant/alerts/rules/${id}`, { method: "PATCH", body })

export const deleteAlertRule = (id: string) =>
  api<{ deleted: boolean; id: string }>(`/merchant/alerts/rules/${id}`, { method: "DELETE" })

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

export const listWebhooks = () => api<{ data: MerchantWebhook[] | null }>("/merchant/webhooks")

export const createWebhook = (body: WebhookRequest) =>
  api<MerchantWebhook>("/merchant/webhooks", { method: "POST", body })

export const deleteWebhook = (id: string) =>
  api<{ deleted: boolean; id: string }>(`/merchant/webhooks/${id}`, { method: "DELETE" })

// --- Alerting: notifications (in_app store / header bell, #736) ---

export const listNotifications = (unread?: boolean) =>
  api<{ data: MerchantNotification[] | null }>("/merchant/notifications", {
    query: unread !== undefined ? { unread } : undefined,
  })

export const markNotificationRead = (id: string) =>
  api<{ read: boolean; id: string }>(`/merchant/notifications/${id}/read`, {
    method: "POST",
    body: {},
  })

export const getUnreadCount = () => api<{ unread: number }>("/merchant/notifications/unread-count")
