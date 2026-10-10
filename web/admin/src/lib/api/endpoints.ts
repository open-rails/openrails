// Per-endpoint client functions for /v1/admin/*. Shapes are the generated
// wire types where the route has them, else src/lib/api/types.ts.
import {
  api,
  apiResponse,
  type CursorEnvelope,
  type PageRequest,
} from "./client"
import type {
  Allowance,
  CatalogApplicationReceipt,
  CatalogDriftRefresh,
  CreateOffChannelPaymentParams,
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
  RebillCycle,
  RefundPaymentParams,
  UpdateCustomerParams,
  UpdatePriceParams,
  UpdateProductParams,
} from "./generated/wire"
import type {
  AdminSubscription,
  CustomerEntitlement,
  Finding,
  MerchantAPIKey,
  MerchantConfiguration,
  MerchantSettings,
  MerchantWebhook,
  MintedAPIKey,
  PSP,
  PSPRoutingPreview,
  RailDefinition,
  RawProductAccessGrant,
  PriceMigration,
  PriceMigrationCancel,
  PriceMigrationPreview,
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
  api<ListPage<CustomerEntitlement>>(
    `/admin/customers/${customerId}/entitlements`,
    { query: { ...page }, signal }
  )

export const listCustomerProductAccess = (
  customerId: string,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<RawProductAccessGrant>>(
    `/admin/customers/${customerId}/product-access`,
    { query: { ...page }, signal }
  )

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
}

export const listCustomerUsageRateOverrides = (
  customerId: string,
  cursor?: string,
  signal?: AbortSignal
) =>
  api<ListPage<RateOverride>>(`/admin/customers/${customerId}/rate-overrides`, {
    query: { limit: PAGE_MAX, cursor },
    signal,
  })

export const putCustomerUsageRateOverride = (
  customerId: string,
  meterKey: string,
  body: CustomerUsageRateOverrideRequest
) =>
  api<RateOverride>(
    `/admin/customers/${customerId}/rate-overrides/${encodeURIComponent(meterKey)}`,
    { method: "PUT", body }
  )

export const deleteCustomerUsageRateOverride = (
  customerId: string,
  meterKey: string
) =>
  api<void>(
    `/admin/customers/${customerId}/rate-overrides/${encodeURIComponent(meterKey)}`,
    { method: "DELETE" }
  )

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

export const revokeProductAccess = (customerId: string, grantId: string) =>
  api<void>(`/admin/customers/${customerId}/product-access/${grantId}`, {
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
    `/admin/customers/${customerId}/payments/off-channel`,
    { method: "POST", body }
  )
  return { payment, recorded: status === 201 }
}

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

export const changeSubscriptionPaymentMethod = (
  id: string,
  paymentMethodId: string
) =>
  api<AdminSubscription>(`/admin/subscriptions/${id}/payment-method`, {
    method: "PUT",
    body: { payment_method_id: paymentMethodId },
  })

export const previewSubscriptionTierChange = (id: string, priceId: string) =>
  api<TierChangePreview>(`/admin/subscriptions/${id}/change-tier/preview`, {
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
  api<TierChangeResult>(`/admin/subscriptions/${id}/change-tier`, {
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
  api<ListPage<PaymentAttempt>>("/admin/payment-attempts", {
    query: { ...filters, ...page },
    signal,
  })

export const getPaymentAttempt = (id: string, signal?: AbortSignal) =>
  api<PaymentAttempt>(`/admin/payment-attempts/${id}`, { signal })

export const listRebillCycles = (
  filters: CycleFilters,
  page: PageRequest,
  signal?: AbortSignal
) =>
  api<ListPage<RebillCycle>>("/admin/rebill-cycles", {
    query: { ...filters, ...page },
    signal,
  })

export const getRebillCycle = (id: string, signal?: AbortSignal) =>
  api<RebillCycle>(`/admin/rebill-cycles/${id}`, { signal })

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
  api<ListPage<RateOverride>>(
    `/admin/catalog/meters/${encodeURIComponent(key)}/rate-overrides`,
    { query: { limit, cursor }, signal }
  )

export const putUsageMeter = (key: string, body: UsageMeterRequest) =>
  api<Meter>(`/admin/catalog/meters/${encodeURIComponent(key)}`, {
    method: "PUT",
    body,
  })

export const putDefaultUsageRateCard = (
  key: string,
  body: DefaultUsageRateCardRequest
) =>
  api<Meter>(`/admin/catalog/meters/${encodeURIComponent(key)}/rate-card`, {
    method: "PUT",
    body,
  })

export const deleteDefaultUsageRateCard = (key: string) =>
  api<void>(`/admin/catalog/meters/${encodeURIComponent(key)}/rate-card`, {
    method: "DELETE",
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

export const getPriceByKey = (productKey: string, key: string) =>
  api<Price>(
    `/admin/catalog/products/by-key/${encodeURIComponent(productKey)}/prices/by-key/${encodeURIComponent(key)}`
  )

// updatePrice archives or restores a price, or changes its PSP links
// (a PSP set to null is unlinked).
export const updatePrice = (id: string, body: UpdatePriceParams) =>
  api<Price>(`/admin/catalog/prices/${id}`, { method: "PATCH", body })

// getPriceKeyHistory returns a price key's history, most recent first: when
// the key moved to which price.
export const getPriceKeyHistory = (
  productKey: string,
  key: string,
  signal?: AbortSignal
) =>
  api<ListPage<PriceKeyMovement>>(
    `/admin/catalog/products/by-key/${encodeURIComponent(productKey)}/prices/by-key/${encodeURIComponent(key)}/history`,
    { query: { limit: PAGE_MAX }, signal }
  )

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

export const getCatalogRevision = () =>
  api<{ revision: number; writes_allowed: boolean }>("/admin/catalog/revision")

// JSON is valid YAML too. Keep the reviewed document byte-for-byte unchanged
// instead of parsing/re-encoding money in the browser. The server deduplicates
// batches by their canonical content.
export const applyCatalog = (document: string) =>
  api<CatalogApplicationReceipt>("/admin/catalog/applications", {
    method: "POST",
    rawBody: document,
    headers: { "Content-Type": "application/yaml" },
  })

export const refreshCatalogDrift = () =>
  api<CatalogDriftRefresh>("/admin/catalog/drift/refresh", { method: "POST" })

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

export const listWorkerHealth = (signal?: AbortSignal) =>
  api<ListPage<WorkerHealth>>("/admin/worker-health", { signal })

// --- Settings ---

export const getMerchantConfiguration = (signal?: AbortSignal) =>
  api<MerchantConfiguration>("/admin/configuration", { signal })

// Changes only the settings it names, against the revision the form read.
export const applyMerchantSettings = (
  revision: string,
  settings: MerchantSettings
) =>
  api<{ application_id: string; revision: string; replayed: boolean }>(
    "/admin/configuration/applications",
    {
      method: "POST",
      body: {
        application_id: crypto.randomUUID(),
        expected_revision: revision,
        settings,
      },
    }
  )

// A merchant has a handful of PSPs: one page holds them all.
export const listPSPs = (signal?: AbortSignal) =>
  api<ListPage<PSP>>("/admin/psps?limit=500", { signal })

export const listRails = (signal?: AbortSignal) =>
  api<ListPage<RailDefinition>>("/admin/rails", { signal })

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
  api<PSP>(`/admin/psps/${encodeURIComponent(id)}/archive`, {
    method: "POST",
    body: allowLast ? { allow_last: true } : {},
  })

// --- API keys (#757) ---

export const listApiKeys = (signal?: AbortSignal) =>
  api<ListPage<MerchantAPIKey>>("/merchant/api-keys", { signal })

export const createApiKey = (name: string, role: string) =>
  api<MintedAPIKey>("/merchant/api-keys", {
    method: "POST",
    body: { name, role },
  })

export const revokeApiKey = (id: string) =>
  api<void>(`/merchant/api-keys/${id}`, { method: "DELETE" })

// --- Team management (#760) ---

export const listTeam = (signal?: AbortSignal) =>
  api<ListPage<TeamMember>>("/merchant/team", { signal })

export const listTeamInvites = (signal?: AbortSignal) =>
  api<ListPage<TeamInvite>>("/merchant/team/invites", { signal })

// The public configuration: what the deployment serves (its
// capabilities.features.team_invites says whether an invite can mint a
// register-and-join link), the currency registry and the payment setup.
export const getConfig = (signal?: AbortSignal) =>
  api<PublicConfig>("/config", { signal })

export const inviteTeamMember = (email: string, role: string) =>
  api<TeamInviteResult>("/merchant/team/invites", {
    method: "POST",
    body: { email, role },
  })

export const revokeTeamInvite = (id: string) =>
  api<void>(`/merchant/team/invites/${id}`, { method: "DELETE" })

export const changeTeamRole = (userId: string, role: string) =>
  api<TeamMember>(`/merchant/team/${userId}`, {
    method: "PATCH",
    body: { role },
  })

export const removeTeamMember = (userId: string) =>
  api<void>(`/merchant/team/${userId}`, { method: "DELETE" })

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
  api<MerchantWebhook>(`/admin/alert-webhooks/${id}/url`, {
    method: "PUT",
    body: { url },
  })

export const deleteWebhook = (id: string) =>
  api<void>(`/admin/alert-webhooks/${id}`, { method: "DELETE" })
