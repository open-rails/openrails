// Per-endpoint client functions for /v1/merchant/*. Shapes are the generated
// wire types where the route has them, else src/lib/api/types.ts.
import {
  api,
  apiResponse,
  type CursorEnvelope,
  type ListEnvelope,
  type PageRequest,
} from "./client"
import type {
  Allowance,
  CatalogDrift,
  CatalogDriftCheck,
  CreateOffChannelPaymentParams,
  CreditLimit,
  Customer,
  ListPage,
  Meter,
  Payment,
  PaymentAttempt,
  PaymentMethod,
  Price,
  PriceKeyMovement,
  Product,
  RateOverride,
  RatePrice,
  RebillCycle,
  RefundPaymentParams,
  TrustLevel,
  UpdatePriceParams,
  UpdateProductParams,
} from "./generated/wire"
import type {
  AdminSubscription,
  CheckoutRoutingDecision,
  CustomerBillingProfile,
  Finding,
  FindingsListResponse,
  MerchantAPIKey,
  MerchantNotification,
  MerchantSettings,
  MerchantWebhook,
  MintedAPIKey,
  PaymentProviderConfig,
  PaymentProviderDefinition,
  RawEntitlement,
  RawProductAccessGrant,
  RepairAlert,
  RepriceBatch,
  Reprice,
  RepriceBatchCancel,
  RepriceBatchPreview,
  RepriceBatchResult,
  RepriceStatus,
  TierChangePreview,
  TierChangeResult,
  TeamInvite,
  TeamInviteResult,
  TeamMember,
  WebhookFormat,
  WorkerHealth,
} from "./types"

// --- Customers ---

export const listCustomers = (
  q: string,
  limit: number,
  cursor: string,
  signal?: AbortSignal
) =>
  api<ListPage<Customer>>("/merchant/customers", {
    query: { q, limit, cursor },
    signal,
  })

export const getCustomerProfile = (customerId: string, signal?: AbortSignal) =>
  api<CustomerBillingProfile>(
    `/merchant/customers/${customerId}/billing-profile`,
    { signal }
  )

export const listCustomerPaymentMethods = (
  customerId: string,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<PaymentMethod>>(
    `/merchant/customers/${customerId}/payment-methods`,
    { query: { ...page }, signal }
  )

export interface CustomerUsageRateOverrideRequest {
  price: RatePrice
  allowance?: Allowance
}

export const listCustomerUsageRateOverrides = (
  customerId: string,
  cursor?: string,
  signal?: AbortSignal
) =>
  api<ListPage<RateOverride>>(
    `/merchant/customers/${customerId}/rate-overrides`,
    { query: { limit: PAGE_MAX, cursor }, signal }
  )

export const putCustomerUsageRateOverride = (
  customerId: string,
  meterKey: string,
  body: CustomerUsageRateOverrideRequest
) =>
  api<RateOverride>(
    `/merchant/customers/${customerId}/rate-overrides/${encodeURIComponent(meterKey)}`,
    { method: "PUT", body }
  )

export const deleteCustomerUsageRateOverride = (
  customerId: string,
  meterKey: string
) =>
  api<void>(
    `/merchant/customers/${customerId}/rate-overrides/${encodeURIComponent(meterKey)}`,
    { method: "DELETE" }
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
  api<void>(`/merchant/customers/${customerId}/entitlements/${entitlementId}`, {
    method: "DELETE",
  })

export const grantProductAccess = (
  customerId: string,
  productId: string,
  endsAt?: string
) =>
  api<RawProductAccessGrant>(
    `/merchant/customers/${customerId}/product-access`,
    {
      method: "POST",
      body: endsAt
        ? { product_id: productId, ends_at: endsAt }
        : { product_id: productId },
    }
  )

export const revokeProductAccess = (customerId: string, grantId: string) =>
  api<void>(`/merchant/customers/${customerId}/product-access/${grantId}`, {
    method: "DELETE",
  })

// createOffChannelPayment records a payment taken outside any rail. The
// transaction id is its identity: recorded is false when it was already
// recorded with the same terms (200 instead of 201).
export const createOffChannelPayment = async (
  customerId: string,
  body: CreateOffChannelPaymentParams
) => {
  const { status, body: payment } = await apiResponse<Payment>(
    `/merchant/customers/${customerId}/payments/off-channel`,
    { method: "POST", body }
  )
  return { payment, recorded: status === 201 }
}

// --- Subscriptions ---

export interface SubscriptionFilters {
  status?: string
  rail?: string
  customer_id?: string
  price_id?: string
}

export const listSubscriptions = (
  filters: SubscriptionFilters,
  limit: number,
  cursor?: string,
  signal?: AbortSignal
) =>
  api<CursorEnvelope<AdminSubscription>>("/merchant/subscriptions", {
    query: { ...filters, limit, ...(cursor ? { cursor } : {}) },
    signal,
  })

export const getSubscription = (id: string, signal?: AbortSignal) =>
  api<AdminSubscription>(`/merchant/subscriptions/${id}`, { signal })

export const cancelSubscription = (
  id: string,
  reason: string,
  revokeAccess: boolean
) =>
  api<AdminSubscription>(`/merchant/subscriptions/${id}/cancel`, {
    method: "POST",
    body: { reason, revoke_access: revokeAccess },
  })

export const resumeSubscription = (id: string) =>
  api<AdminSubscription>(`/merchant/subscriptions/${id}/resume`, {
    method: "POST",
  })

export const changeSubscriptionPaymentMethod = (
  id: string,
  paymentMethodId: string
) =>
  api<AdminSubscription>(`/merchant/subscriptions/${id}/payment-method`, {
    method: "PUT",
    body: { payment_method_id: paymentMethodId },
  })

export const previewSubscriptionTierChange = (id: string, priceId: string) =>
  api<TierChangePreview>(`/merchant/subscriptions/${id}/change-tier/preview`, {
    method: "POST",
    body: { price_id: priceId },
  })

// A tier change is a durable operation keyed by this header: the same key
// replays its result, so a retry must reuse the key of the reviewed change.
export const changeSubscriptionTier = (
  id: string,
  priceId: string,
  idempotencyKey: string
) =>
  api<TierChangeResult>(`/merchant/subscriptions/${id}/change-tier`, {
    method: "POST",
    headers: { "Idempotency-Key": idempotencyKey },
    body: { price_id: priceId },
  })

// --- Payments ---

// Payments are listed newest first.
export interface PaymentFilters {
  customer_id?: string
  subscription_id?: string
  price_id?: string
  rail?: string
  kind?: Payment["kind"]
  transaction_id?: string
}

export const listPayments = (
  filters: PaymentFilters,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<Payment>>("/merchant/payments", {
    query: { ...filters, ...page },
    signal,
  })

export const getPayment = (id: string, signal?: AbortSignal) =>
  api<Payment>(`/merchant/payments/${id}`, { signal })

// refundPayment answers the refund: succeeded, or pending (202) while the
// rail settles it.
export const refundPayment = (
  id: string,
  amount: string,
  reason: string,
  revokeAccess: boolean
) => {
  const body: RefundPaymentParams = {
    amount,
    reason: reason || undefined,
    revoke_access: revokeAccess,
  }
  return api<Payment>(`/merchant/payments/${id}/refunds`, {
    method: "POST",
    headers: { "Idempotency-Key": crypto.randomUUID() },
    body,
  })
}

// --- Payment attempts and rebill cycles (#1116) ---
// Filters are the API's query parameters verbatim; a text filter is one value
// or a comma-separated list, so a console URL carries them unchanged.

export type AttemptFilters = Partial<
  Record<(typeof ATTEMPT_FILTERS)[number], string>
>
export const ATTEMPT_FILTERS = [
  "kind",
  "owner",
  "category",
  "reason",
  "response_code",
  "card_entry",
  "source",
  "observed_via",
  "avs_result",
  "cvv_result",
  "psp_id",
  "customer_id",
  "checkout_id",
  "subscription_id",
  "cycle_id",
  "since",
  "until",
] as const

export type CycleFilters = Partial<
  Record<(typeof CYCLE_FILTERS)[number], string>
>
export const CYCLE_FILTERS = [
  "owner",
  "first_outcome",
  "miss_reason",
  "outcome",
  "psp_id",
  "subscription_id",
  "due_since",
  "due_until",
] as const

// filtersFrom reads the named filters from a console URL.
export function filtersFrom<K extends string>(
  params: URLSearchParams,
  keys: readonly K[]
): Partial<Record<K, string>> {
  const out: Partial<Record<K, string>> = {}
  for (const key of keys) {
    const v = params.get(key)?.trim()
    if (v) out[key] = v
  }
  return out
}

export const listPaymentAttempts = (
  filters: AttemptFilters,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<PaymentAttempt>>("/merchant/payment-attempts", {
    query: { ...filters, ...page },
    signal,
  })

export const getPaymentAttempt = (id: string, signal?: AbortSignal) =>
  api<PaymentAttempt>(`/merchant/payment-attempts/${id}`, { signal })

export const listRebillCycles = (
  filters: CycleFilters,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<RebillCycle>>("/merchant/rebill-cycles", {
    query: { ...filters, ...page },
    signal,
  })

export const getRebillCycle = (id: string, signal?: AbortSignal) =>
  api<RebillCycle>(`/merchant/rebill-cycles/${id}`, { signal })

// Rails whose refunds route through a provider API today (admin_payments.go);
// off-rail payments are refunded where they were taken.
export const REFUNDABLE_RAILS = ["nmi", "stripe"]

// --- Catalog ---

// PAGE_MAX is the largest page a list route serves.
export const PAGE_MAX = 500

// archived: false lists live products, true archived ones, undefined both.
// Each product carries its current prices.
export const listProducts = (
  limit: number,
  cursor?: string,
  archived?: boolean,
  signal?: AbortSignal
) =>
  api<ListPage<Product>>("/merchant/catalog/products", {
    query: { limit, cursor, archived },
    signal,
  })

export const getProduct = (id: string, signal?: AbortSignal) =>
  api<Product>(`/merchant/catalog/products/${id}`, { signal })

export interface ProductRequest {
  key: string
  display_name: string
  description: string
  tier_group?: string
  tier_rank?: number
  entitlements_spec?: Record<string, number | null>
}

export const createProduct = (body: ProductRequest) =>
  api<Product>("/merchant/catalog/products", { method: "POST", body })

// updateProduct is a merge patch: omitted fields stay, null clears
// description, entitlements_spec and tier_group.
export const updateProduct = (id: string, body: UpdateProductParams) =>
  api<Product>(`/merchant/catalog/products/${id}`, {
    method: "PATCH",
    body,
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
  price: RatePrice
  allowance?: Allowance
}

export const listUsageMeters = (
  limit = PAGE_MAX,
  cursor?: string,
  signal?: AbortSignal
) =>
  api<ListPage<Meter>>("/merchant/catalog/meters", {
    query: { limit, cursor },
    signal,
  })

export const getUsageMeter = (key: string, signal?: AbortSignal) =>
  api<Meter>(`/merchant/catalog/meters/${encodeURIComponent(key)}`, {
    signal,
  })

export const listUsageMeterOverrides = (
  key: string,
  limit = PAGE_MAX,
  cursor?: string,
  signal?: AbortSignal
) =>
  api<ListPage<RateOverride>>(
    `/merchant/catalog/meters/${encodeURIComponent(key)}/rate-overrides`,
    { query: { limit, cursor }, signal }
  )

export const putUsageMeter = (key: string, body: UsageMeterRequest) =>
  api<Meter>(`/merchant/catalog/meters/${encodeURIComponent(key)}`, {
    method: "PUT",
    body,
  })

export const putDefaultUsageRateCard = (
  key: string,
  body: DefaultUsageRateCardRequest
) =>
  api<Meter>(`/merchant/catalog/meters/${encodeURIComponent(key)}/rate-card`, {
    method: "PUT",
    body,
  })

export const deleteDefaultUsageRateCard = (key: string) =>
  api<void>(`/merchant/catalog/meters/${encodeURIComponent(key)}/rate-card`, {
    method: "DELETE",
  })

export const listPrices = (
  limit: number,
  cursor?: string,
  productId?: string,
  signal?: AbortSignal
) =>
  api<ListPage<Price>>("/merchant/catalog/prices", {
    query: { limit, cursor, product_id: productId },
    signal,
  })

export interface PriceRequest {
  product_id: string
  unit_amount: string
  currency: string
  access_duration_hours?: number
  auto_renew?: boolean
  trial_unit_amount?: string
  trial_duration_hours?: number
  // Declaring the key of a live price with other terms makes this its new
  // version and archives the old one; omit for the default key.
  key?: string
  // The PSPs that sell the price, e.g. carried over from the version it
  // replaces. Empty: sold through no PSP.
  psps?: string[]
}

export const createPrice = (body: PriceRequest) =>
  api<Price>("/merchant/catalog/prices", { method: "POST", body })

// getPrice returns the price with its state on each linked PSP. verify=true
// also reads every PSP's copy and reports its drift: a read, never a write,
// and slow enough that it stays opt-in.
export const getPrice = (id: string, verify = false, signal?: AbortSignal) =>
  api<Price>(`/merchant/catalog/prices/${id}`, {
    query: verify ? { verify: true } : undefined,
    signal,
  })

export const getPriceByKey = (key: string) =>
  api<Price>(`/merchant/catalog/prices/by-key/${encodeURIComponent(key)}`)

// updatePrice moves a price to another key, archives or restores it, or
// changes its PSP links (a PSP set to null is unlinked).
export const updatePrice = (id: string, body: UpdatePriceParams) =>
  api<Price>(`/merchant/catalog/prices/${id}`, { method: "PATCH", body })

// getPriceKeyHistory returns a price key's history, most recent first: when
// the key moved to which price.
export const getPriceKeyHistory = (key: string, signal?: AbortSignal) =>
  api<ListPage<PriceKeyMovement>>(
    `/merchant/catalog/prices/by-key/${encodeURIComponent(key)}/history`,
    { query: { limit: PAGE_MAX }, signal }
  )

// --- Repricing / migration (#773 primitive, #777 console wizard) ---

// previewRepriceBatch is the wizard's Step 2 affected-count dry run, called
// BEFORE the price edit lands; it never writes.
export const previewRepriceBatch = (priceKey: string) =>
  api<RepriceBatchPreview>("/merchant/reprice-batches/preview", {
    method: "POST",
    body: { price_key: priceKey },
  })

// createRepriceBatch schedules every active subscription on a prior version
// of priceKey to move to its current price at effectiveAt.
export const createRepriceBatch = (priceKey: string, effectiveAt: string) =>
  api<RepriceBatchResult>("/merchant/reprice-batches", {
    method: "POST",
    body: { price_key: priceKey, effective_at: effectiveAt },
  })

// listRepriceBatches lists a price key's batches, newest first.
export const listRepriceBatches = (
  priceKey: string,
  limit = 20,
  signal?: AbortSignal
) =>
  api<CursorEnvelope<RepriceBatch>>("/merchant/reprice-batches", {
    query: { price_key: priceKey, limit },
    signal,
  })

export const cancelRepriceBatch = (id: string) =>
  api<RepriceBatchCancel>(`/merchant/reprice-batches/${id}/cancel`, {
    method: "POST",
  })

export interface RepriceFilters {
  subscription_id?: string
  reprice_batch_id?: string
  status?: RepriceStatus
}

export const listReprices = (
  filters: RepriceFilters,
  limit = 100,
  cursor?: string,
  signal?: AbortSignal
) =>
  api<CursorEnvelope<Reprice>>("/merchant/reprices", {
    query: { ...filters, limit, ...(cursor ? { cursor } : {}) },
    signal,
  })

export const cancelReprice = (id: string) =>
  api<Reprice>(`/merchant/reprices/${id}/cancel`, {
    method: "POST",
  })

export interface CatalogApplicationReceipt {
  application_id: string
  catalog_id: string
  base_revision: number
  applied_revision: number
  replayed: boolean
  products_changed: number
  prices_changed: number
}

export const getCatalogRevision = () =>
  api<{ revision: number; writes_allowed: boolean }>(
    "/merchant/catalog/revision"
  )

// JSON is valid YAML too. Keep the reviewed document byte-for-byte unchanged
// instead of parsing/re-encoding money or application identity in the browser.
export const applyCatalog = (document: string) =>
  api<CatalogApplicationReceipt>("/merchant/catalog/applications", {
    method: "POST",
    rawBody: document,
    headers: { "Content-Type": "application/yaml" },
  })

export const listCatalogDrift = (
  limit: number,
  cursor?: string,
  signal?: AbortSignal
) =>
  api<ListPage<CatalogDrift>>("/merchant/catalog/drift", {
    query: { limit, cursor },
    signal,
  })

export const refreshCatalogDrift = () =>
  api<CatalogDriftCheck>("/merchant/catalog/drift/refresh", { method: "POST" })

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
  operation_id: string
  expected_revision: number
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

// archivePaymentProviderAccount (#655) archives exactly this account by its
// immutable id, without contacting the provider. The rail's last active
// account is refused (409 provider_account_last_active) unless allowLast.
export const archivePaymentProviderAccount = (
  rail: string,
  id: string,
  allowLast = false
) =>
  api<{ payment_provider: PaymentProviderConfig }>(
    `/merchant/payment-providers/${rail}/accounts/${id}/archive`,
    {
      method: "POST",
      body: allowLast ? { allow_last: true } : {},
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
  api<CreditLimit>(
    `/merchant/customers/${encodeURIComponent(customerId)}/credit-limit`,
    { query: { currency } }
  )

export const setCreditLimit = (
  customerId: string,
  currency: string,
  amount: string
) =>
  api<CreditLimit>(
    `/merchant/customers/${encodeURIComponent(customerId)}/credit-limit`,
    { method: "PUT", body: { currency, amount } }
  )

export const getTrustLevel = (customerId: string, currency: string) =>
  api<TrustLevel>(
    `/merchant/customers/${encodeURIComponent(customerId)}/trust-level`,
    { query: { currency } }
  )

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

export const rotateWebhookURL = (id: string, url: string) =>
  api<MerchantWebhook>(`/merchant/webhooks/${id}/url`, {
    method: "PUT",
    body: { url },
  })

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
