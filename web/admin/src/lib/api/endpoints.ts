// Per-endpoint client functions for /v1/admin/*. Shapes are the generated
// wire types where the route has them, else src/lib/api/types.ts.
import {
  api,
  type CursorEnvelope,
  type PageRequest,
} from "./client"
import type {
  AdminAccess,
  Allowance,
  CatalogApplicationReceipt,
  PSPRefresh,
  CreatePriceParams,
  Customer,
  ListPage,
  Meter,
  Payment,
  PaymentAttempt,
  PaymentMethod,
  Price,
  PriceKeyMovement,
  Product,
  PublicConfig,
  RateOverride,
  RatePrice,
  Renewal,
  RefundPaymentParams,
  UpdateCustomerParams,
  UpdatePriceParams,
  UpdateProductParams,
} from "./generated/wire"
import type {
  AdminSubscription,
  CustomerEntitlement,
  Finding,
  MerchantConfiguration,
  MerchantSettings,
  MerchantWebhook,
  PSP,
  PSPRoutingPreview,
  RawProductAccessGrant,
  PriceMigration,
  PriceMigrationCancel,
  PriceMigrationPreview,
  ChangeSubscriptionParams,
  SubscriptionChange,
  SubscriptionChangePreview,
  WebhookFormat,
} from "./types"

// --- Customers ---

export const listCustomers = (
  search: string,
  limit: number,
  cursor: string,
  signal?: AbortSignal
) =>
  api<ListPage<Customer>>("/admin/customers", {
    query: { search, limit, cursor },
    signal,
  })

// getCustomer is one customer: settings, and per currency its balance,
// arrears and collection card. Its lists are their own routes.
export const getCustomer = (customerId: string, signal?: AbortSignal) =>
  api<Customer>(`/admin/customers/${customerId}`, { signal })

export const listCustomerEntitlements = (
  customerId: string,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<CustomerEntitlement>>("/admin/entitlements", {
    query: { customer_id: customerId, ...page },
    signal,
  })

export const listCustomerProductAccess = (
  customerId: string,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<RawProductAccessGrant>>("/admin/product-access", {
    query: { customer_id: customerId, ...page },
    signal,
  })

export const listCustomerPaymentMethods = (
  customerId: string,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<PaymentMethod>>(
    `/admin/customers/${customerId}/payment-methods`,
    { query: { ...page }, signal }
  )

export interface CustomerUsageRateOverrideRequest {
  price: RatePrice
  allowance?: Allowance
  // The override's revision this edit read; 0 when it read none.
  expected_revision?: number
}

export const listCustomerUsageRateOverrides = (
  customerId: string,
  cursor?: string,
  signal?: AbortSignal
) =>
  api<ListPage<RateOverride>>("/admin/catalog/rate-overrides", {
    query: { customer_id: customerId, limit: PAGE_MAX, cursor },
    signal,
  })

const rateOverridePath = (customerId: string, meterKey: string) =>
  `/admin/catalog/rate-overrides/${customerId}/${encodeURIComponent(meterKey)}`

export const putCustomerUsageRateOverride = (
  customerId: string,
  meterKey: string,
  body: CustomerUsageRateOverrideRequest
) =>
  api<RateOverride>(rateOverridePath(customerId, meterKey), {
    method: "PUT",
    body,
  })

export const deleteCustomerUsageRateOverride = (
  customerId: string,
  meterKey: string
) => api<void>(rateOverridePath(customerId, meterKey), { method: "DELETE" })

export interface ProductGrant {
  productId: string
  hours?: number
  endsAt?: string
  reason?: "comp" | "staff"
  note?: string
}

// grantProductAccess grants one product free; the customer holds its keys
// while the grant is live.
export const grantProductAccess = (customerId: string, grant: ProductGrant) =>
  api<{ items: RawProductAccessGrant[] }>(`/admin/product-access`, {
    method: "POST",
    body: {
      items: [
        {
          customer_id: customerId,
          product_id: grant.productId,
          ...(grant.hours ? { hours: grant.hours } : {}),
          ...(grant.endsAt ? { ends_at: grant.endsAt } : {}),
          ...(grant.reason ? { reason: grant.reason } : {}),
          ...(grant.note ? { note: grant.note } : {}),
        },
      ],
    },
  })

// revokeProductAccess takes a product back; the window keeps its row and the
// reason.
export const revokeProductAccess = (grantId: string, reason: string) =>
  api<void>(`/admin/product-access/${grantId}/revoke`, {
    method: "POST",
    body: { reason },
  })

// --- Subscriptions ---

export interface SubscriptionFilters {
  status?: string
  // dunning "true" keeps the subscriptions past_due or awaiting_method.
  dunning?: string
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
  api<CursorEnvelope<AdminSubscription>>("/admin/subscriptions", {
    query: { ...filters, limit, ...(cursor ? { cursor } : {}) },
    signal,
  })

export const getSubscription = (id: string, signal?: AbortSignal) =>
  api<AdminSubscription>(`/admin/subscriptions/${id}`, { signal })

export const cancelSubscription = (
  id: string,
  reason: string,
  revokeAccess: boolean
) =>
  api<AdminSubscription>(`/admin/subscriptions/${id}/cancel`, {
    method: "POST",
    body: { reason, revoke_access: revokeAccess },
  })

export const resumeSubscription = (id: string) =>
  api<AdminSubscription>(`/admin/subscriptions/${id}/resume`, {
    method: "POST",
  })

// null makes the subscription follow the customer's default card.
export const changeSubscriptionPaymentMethod = (
  id: string,
  paymentMethodId: string | null
) =>
  api<AdminSubscription>(`/admin/subscriptions/${id}/payment-method`, {
    method: "PUT",
    body: { payment_method_id: paymentMethodId },
  })

export const previewSubscriptionChange = (
  id: string,
  change: ChangeSubscriptionParams
) =>
  api<SubscriptionChangePreview>(`/admin/subscriptions/${id}/change/preview`, {
    method: "POST",
    body: change,
  })

// A change is a durable operation keyed by this header: the same key replays
// its result, so a retry must reuse the key of the reviewed change.
export const changeSubscription = (
  id: string,
  change: ChangeSubscriptionParams,
  idempotencyKey: string
) =>
  api<SubscriptionChange>(`/admin/subscriptions/${id}/change`, {
    method: "POST",
    headers: { "Idempotency-Key": idempotencyKey },
    body: change,
  })

// --- Payments ---

// Payments are listed newest first.
export interface PaymentFilters {
  customer_id?: string
  subscription_id?: string
  invoice_id?: string
  order_id?: string
  status?: Payment["status"]
  rail?: string
  kind?: Payment["kind"]
  transaction_id?: string
}

export const listPayments = (
  filters: PaymentFilters,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<Payment>>("/admin/payments", {
    query: { ...filters, ...page },
    signal,
  })

export const getPayment = (id: string, signal?: AbortSignal) =>
  api<Payment>(`/admin/payments/${id}`, { signal })

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
  return api<Payment>(`/admin/payments/${id}/refunds`, {
    method: "POST",
    headers: { "Idempotency-Key": crypto.randomUUID() },
    body,
  })
}

// --- Payment attempts and renewals (#1116) ---
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
  "payment_id",
  "invoice_id",
  "order_id",
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
  api<ListPage<PaymentAttempt>>("/admin/payment-attempts", {
    query: { ...filters, ...page },
    signal,
  })

export const getPaymentAttempt = (id: string, signal?: AbortSignal) =>
  api<PaymentAttempt>(`/admin/payment-attempts/${id}`, { signal })

export const listRenewals = (
  filters: CycleFilters,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<Renewal>>("/admin/renewals", {
    query: { ...filters, ...page },
    signal,
  })

export const getRenewal = (id: string, signal?: AbortSignal) =>
  api<Renewal>(`/admin/renewals/${id}`, { signal })

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
  api<ListPage<Product>>("/admin/catalog/products", {
    query: { limit, cursor, archived },
    signal,
  })

export const getProduct = (id: string, signal?: AbortSignal) =>
  api<Product>(`/admin/catalog/products/${id}`, { signal })

export interface ProductRequest {
  key: string
  display_name: string
  description: string
  tier_group?: string
  tier_rank?: number
  entitlements?: string[]
}

export const createProduct = (body: ProductRequest) =>
  api<Product>("/admin/catalog/products", { method: "POST", body })

// updateProduct is a merge patch: omitted fields stay, null clears
// description and tier_group; entitlements: [] clears the granted keys.
export const updateProduct = (id: string, body: UpdateProductParams) =>
  api<Product>(`/admin/catalog/products/${id}`, {
    method: "PATCH",
    body,
  })

export interface UsageMeterRequest {
  event_type: string
  value_property: string
  aggregation: "sum" | "count"
  unit?: string
  group_by: Record<string, string>
  // Omitted keeps the meter's rate card; null removes it.
  rate_card?: DefaultUsageRateCardRequest | null
  // The meter's revision this edit read; 0 for a new meter.
  expected_revision?: number
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
  api<ListPage<Meter>>("/admin/catalog/meters", {
    query: { limit, cursor },
    signal,
  })

export const getUsageMeter = (key: string, signal?: AbortSignal) =>
  api<Meter>(`/admin/catalog/meters/${encodeURIComponent(key)}`, {
    signal,
  })

export const listUsageMeterOverrides = (
  key: string,
  limit = PAGE_MAX,
  cursor?: string,
  signal?: AbortSignal
) =>
  api<ListPage<RateOverride>>("/admin/catalog/rate-overrides", {
    query: { meter_key: key, limit, cursor },
    signal,
  })

export const putUsageMeter = (key: string, body: UsageMeterRequest) =>
  api<Meter>(`/admin/catalog/meters/${encodeURIComponent(key)}`, {
    method: "PUT",
    body,
  })

// putUsageMeterRateCard sets a meter's rate card, or removes it with null.
// The rate card is a field of the meter, so the meter is restated with it.
export const putUsageMeterRateCard = (
  meter: Meter,
  rateCard: DefaultUsageRateCardRequest | null
) =>
  putUsageMeter(meter.key, {
    event_type: meter.event_type,
    value_property: meter.value_property,
    aggregation: meter.aggregation as UsageMeterRequest["aggregation"],
    unit: meter.unit,
    group_by: meter.group_by ?? {},
    rate_card: rateCard,
    expected_revision: meter.revision,
  })

export const listPrices = (
  limit: number,
  cursor?: string,
  productId?: string,
  signal?: AbortSignal
) =>
  api<ListPage<Price>>("/admin/catalog/prices", {
    query: { limit, cursor, product_id: productId },
    signal,
  })

// A price that declares the key of a live price with other terms becomes
// its new version and archives the old one.
export const createPrice = (body: CreatePriceParams) =>
  api<Price>("/admin/catalog/prices", { method: "POST", body })

// getPrice returns the price with its state on each linked PSP. verify=true
// also reads every PSP's copy and reports its drift: a read, never a write,
// and slow enough that it stays opt-in.
export const getPrice = (id: string, verify = false, signal?: AbortSignal) =>
  api<Price>(`/admin/catalog/prices/${id}`, {
    query: verify ? { verify: true } : undefined,
    signal,
  })

// getProductByKey reads the product a key names, with its current prices:
// a products list filtered by key.
export const getProductByKey = async (productKey: string) => {
  const page = await api<ListPage<Product>>("/admin/catalog/products", {
    query: { keys: productKey },
  })
  const product = page.data[0]
  if (!product) throw new Error(`No product ${productKey}`)
  return product
}

// updatePrice archives or restores a price, or changes its PSP links
// (a PSP set to null is unlinked).
export const updatePrice = (id: string, body: UpdatePriceParams) =>
  api<Price>(`/admin/catalog/prices/${id}`, { method: "PATCH", body })

// getPriceHistory returns the history of a price's key, most recent first:
// when the key moved to which price.
export const getPriceHistory = (priceId: string, signal?: AbortSignal) =>
  api<ListPage<PriceKeyMovement>>(`/admin/catalog/prices/${priceId}/history`, {
    query: { limit: PAGE_MAX },
    signal,
  })

// --- Price migrations ---

// previewPriceMigration is the wizard's affected-count dry run, called
// before the price edit lands: by key with no target it counts every
// version's subscribers. It never writes.
export const previewPriceMigration = (productKey: string, priceKey: string) =>
  api<PriceMigrationPreview>("/admin/price-migrations/preview", {
    method: "POST",
    body: { product_key: productKey, price_key: priceKey },
  })

// createPriceMigration moves the subscribers of every version of priceKey
// but toPriceId to it, each at its first renewal on or after effectiveAt.
export const createPriceMigration = (
  productKey: string,
  priceKey: string,
  toPriceId: string,
  effectiveAt: string
) =>
  api<PriceMigration>("/admin/price-migrations", {
    method: "POST",
    body: {
      product_key: productKey,
      price_key: priceKey,
      to_price_id: toPriceId,
      effective_at: effectiveAt,
    },
  })

// listPriceMigrations lists a price key's migrations, newest first.
export const listPriceMigrations = (
  productKey: string,
  priceKey: string,
  limit = 20,
  signal?: AbortSignal
) =>
  api<CursorEnvelope<PriceMigration>>("/admin/price-migrations", {
    query: { product_key: productKey, price_key: priceKey, limit },
    signal,
  })

export const cancelPriceMigration = (id: string) =>
  api<PriceMigrationCancel>(`/admin/price-migrations/${id}/cancel`, {
    method: "POST",
  })

export type { CatalogApplicationReceipt } from "./generated/wire"

// JSON is valid YAML too. Keep the reviewed document byte-for-byte unchanged
// instead of parsing/re-encoding money in the browser. The server deduplicates
// batches by their canonical content. force overwrites what edits set.
export const applyCatalog = (document: string, force = false) =>
  api<CatalogApplicationReceipt>("/admin/catalog/applications", {
    method: "POST",
    rawBody: document,
    headers: { "Content-Type": "application/yaml" },
    ...(force ? { query: { force: "true" } } : {}),
  })

// refreshPSPs re-reads every PSP now, its catalog drift included; the pass
// runs in the background.
export const refreshPSPs = () =>
  api<PSPRefresh>("/admin/psps/refresh", { method: "POST" })

// --- Ops ---

// listFindings reads the findings queue; type is one finding type or a prefix
// ending in ".*" (catalog.*: catalog drift).
export const listFindings = (
  filters: { status?: string; severity?: string; type?: string },
  limit: number,
  signal?: AbortSignal
) =>
  api<ListPage<Finding>>("/admin/findings", {
    query: { ...filters, limit },
    signal,
  })

export const getFinding = (id: string) => api<Finding>(`/admin/findings/${id}`)

export const resolveFinding = (
  id: string,
  outcome: "approve" | "ignore",
  notes: string
) =>
  api<{ finding: Finding; execution?: Record<string, unknown> }>(
    `/admin/findings/${id}/resolve`,
    { method: "POST", body: { outcome, notes } }
  )

// --- Settings ---

export const getMerchantConfiguration = (signal?: AbortSignal) =>
  api<MerchantConfiguration>("/admin/configuration", { signal })

// Changes only the settings it names, against the revision the form read.
export const applyMerchantSettings = (
  revision: string,
  settings: MerchantSettings
) =>
  api<{ revision: string; replayed: boolean }>("/admin/configuration", {
    method: "PATCH",
    headers: { "Idempotency-Key": crypto.randomUUID() },
    body: { expected_revision: revision, settings },
  })

// A merchant has a handful of PSPs: one page holds them all.
export const listPSPs = (signal?: AbortSignal) =>
  api<ListPage<PSP>>("/admin/psps?limit=500", { signal })

// The rail registry is static per build: it arrives with the configuration.
export const listRails = async (signal?: AbortSignal) =>
  (await getConfig(signal)).rails

// Credentials are write-only and checked with the provider before anything
// is stored. operation_id makes a retried submission return the first result.
export interface CreatePSPRequest {
  operation_id: string
  key: string
  rail: string
  account_id: string
  settings?: Record<string, string>
  credentials?: Record<string, string>
}

export const createPSP = (body: CreatePSPRequest) =>
  api<PSP>("/admin/psps", { method: "POST", body })

// expected_revision is the PSP revision the form read; a PSP changed since
// is refused.
export interface UpdatePSPRequest {
  operation_id: string
  expected_revision: number
  settings?: Record<string, string>
  credentials?: Record<string, string>
}

export const updatePSP = (id: string, body: UpdatePSPRequest) =>
  api<PSP>(`/admin/psps/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body,
  })

// previewPSPRouting (or#288) explains which PSP a checkout for this price
// would use and why each other PSP was passed over. Read-only: it runs the
// production decision path without creating a session.
export const previewPSPRouting = (
  body: { price_id: string; country?: string; psp?: string },
  signal?: AbortSignal
) =>
  api<PSPRoutingPreview>("/admin/psps/routing-preview", {
    method: "POST",
    body,
    signal,
  })

// archivePSP (#655) archives exactly this PSP without contacting the
// provider. The rail's last active PSP is refused (409 psp_last_active)
// unless allowLast.
export const archivePSP = (id: string, allowLast = false) =>
  api<PSP>(`/admin/psps/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: allowLast ? { archived: true, allow_last: true } : { archived: true },
  })

// The public configuration: what the deployment serves, the currency
// registry and the payment setup.
export const getConfig = (signal?: AbortSignal) =>
  api<PublicConfig>("/config", { signal })

// getAdminAccess is what the signed-in staff member may use of each staff
// route group that is mounted: the console's areas.
export const getAdminAccess = (signal?: AbortSignal) =>
  api<AdminAccess>("/admin/access", { signal })

// updateCustomer changes only the settings params names and answers the
// customer.
export const updateCustomer = (customerId: string, params: UpdateCustomerParams) =>
  api<Customer>(`/admin/customers/${customerId}`, {
    method: "PATCH",
    body: params,
  })

// --- Alerting: webhooks (#736) ---

export interface WebhookRequest {
  name: string
  url: string
  format: WebhookFormat
  enabled?: boolean
}

export const listWebhooks = (signal?: AbortSignal) =>
  api<ListPage<MerchantWebhook>>("/admin/alert-webhooks", { signal })

export const createWebhook = (body: WebhookRequest) =>
  api<MerchantWebhook>("/admin/alert-webhooks", { method: "POST", body })

export const rotateWebhookURL = (id: string, url: string) =>
  api<MerchantWebhook>(`/admin/alert-webhooks/${id}`, {
    method: "PATCH",
    body: { url },
  })

export const deleteWebhook = (id: string) =>
  api<void>(`/admin/alert-webhooks/${id}`, { method: "DELETE" })
