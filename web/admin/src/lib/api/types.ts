// Types mirror the Go handlers' JSON shapes exactly (see internal/http/handlers).
// All money fields are MICROS (millionths of a currency unit).

export type SubscriptionStatus = "pending" | "active" | "past_due" | "cancelled" | "unknown"
export type Rail = "nmi" | "ccbill" | "solana" | "stripe" | string

// --- Customers (#740 list endpoint) ---

export interface CustomerSummary {
  id: string
  subject?: string
  email?: string
  created_at: string
  last_seen_at: string
}

// --- Raw DB models (returned verbatim by profile/subscription endpoints) ---

export interface RawSubscription {
  id: string // bare UUID
  merchant_id: string
  customer_id?: string
  product_id: string
  price_id: string
  status: SubscriptionStatus
  started_at: string
  ended_at: string | null
  current_period_starts_at: string | null
  current_period_ends_at: string | null
  rail: Rail
  rail_subscription_id: string
  user_email?: string
  payment_method_id: string | null
  retry_attempts: number | null
  next_retry_at: string | null
  grace_ends_at: string | null
  cancel_type: string | null
  cancel_feedback: string | null
  cancelled_at: string | null
  price?: RawPrice
  created_at: string
  updated_at: string
}

export interface RawPrice {
  id: string
  product_id?: string
  unit_amount?: number
  currency?: string
  [k: string]: unknown
}

export interface RawPayment {
  id: string
  customer_id?: string
  price_id: string
  subscription_id?: string
  refunded_payment_id?: string
  rail: Rail
  transaction_id: string
  amount: number
  list_amount: number
  currency: string
  status: string // "pending" | "completed" | "failed" | "refunded"
  card_brand?: string
  card_last4?: string
  purchased_at: string
  created_at: string
}

export interface RawEntitlement {
  id: string
  customer_id?: string
  entitlement: string
  start_at: string
  end_at?: string
  source_id?: string
  source_type: string
  revoked_at?: string
  revoke_reason?: string
  created_at: string
  updated_at: string
}

export interface RawProductAccessGrant {
  id: string
  customer_id?: string
  product_id: string
  source_type: string
  source_id: string
  payment_id?: string
  status: string
  starts_at: string
  ends_at?: string
  revoked_at?: string
  revoke_reason?: string
}

export interface PaymentMethodResponse {
  id: string // pm_...
  object: "payment_method"
  type: string
  rail: Rail
  card?: { brand?: string; last4?: string; exp_month?: number; exp_year?: number }
  livemode: boolean
  created: number
  health?: {
    expiry_status?: "valid" | "expiring_soon" | "expired"
    last_charged_at?: string
    last_charge_outcome?: string
    active: boolean
  }
  subscriptions?: { id: string; display_name: string; description: string; created_at: string }[]
}

export interface CreditBalance {
  currency: string
  display_name: string
  unit: string
  decimal_places: number
  balance: number
  held_balance: number
  outstanding_owed_amount: number
}

export interface CustomerBillingProfile {
  customer_id: string
  trust_level?: string
  subscriptions: RawSubscription[]
  entitlements: RawEntitlement[]
  payments: RawPayment[]
  payment_methods: PaymentMethodResponse[]
  credit_balance: CreditBalance[]
  product_access: RawProductAccessGrant[]
}

// --- Stripe-shaped payment object (payments endpoints) ---

export interface PaymentObject {
  id: string // pay_...
  object: "charge" | "refund"
  status?: "succeeded" | "pending" | "failed" | "refunded" | "partially_refunded"
  amount: number
  amount_refunded: number
  currency: string
  user: string // usr_...
  subscription?: string // sub_...
  rail: Rail
  transaction_id: string
  refunded: boolean
  captured?: boolean
  failure_code?: string
  failure_message?: string
  refunds?: { object: "list"; data: PaymentObject[] }
  created: number
}

// --- Subscription admin response (list/detail) ---

export interface AdminSubscription extends RawSubscription {
  payments?: RawPayment[]
}

// --- Catalog ---

export interface CatalogProduct {
  id: string
  key: string
  display_name: string
  description: string
  entitlements_spec?: Record<string, number | null>
  credits_spec?: Record<string, { unit?: string; amount: number; expiry_hours?: number; cadence?: string }>
  tier_group?: string
  tier_rank: number
  archived: boolean
  created_at: string
  updated_at: string
}

export interface CatalogProviderState {
  status: "linked" | "pending_manual_link" | "sync_disabled" | "error"
  ids?: Record<string, string>
  lookup_key?: string
  last_synced_at?: string
  sync_status?: string
  drift?: { field: string; openrails_value: string; remote_value: string }[]
  message?: string
}

export interface CatalogPrice {
  id: string
  product_id: string
  archived: boolean
  unit_amount: number
  currency: string
  access_duration_hours?: number
  auto_renew: boolean
  trial_unit_amount?: number
  trial_duration_hours?: number
  created_at: string
  updated_at: string
  providers?: Record<string, CatalogProviderState>
  pending_manual_actions?: { provider: string; action: string; hint: string }[]
}

export interface CatalogDriftEvent {
  id: string
  provider: string
  kind: string
  openrails_resource_type: string
  openrails_resource_id?: string
  external_resource_id?: string
  field?: string
  openrails_value?: string
  external_value?: string
  detected_at: string
  resolved_at?: string
}

export interface CatalogDriftReport {
  scanned_products: number
  scanned_prices: number
  scanned_nmi_plans: number
  scanned_solana_plans: number
  open_events: CatalogDriftEvent[]
  new_events: number
  resolved_events: number
}

// --- Ops: findings / repair alerts / worker health ---

export interface Recommendation {
  action: string
  params?: Record<string, unknown>
}

export interface Finding {
  id: string
  tenant_id: string
  provider?: string
  finding_type: string
  subject_key: string
  severity: "critical" | "high" | "medium" | "low"
  status: string
  requires_review?: boolean
  recommended_action?: string
  evidence?: Record<string, unknown>
  last_seen_at: string
  resolved_at?: string
  resolution?: string
  operator_notes?: string
  created_at: string
  updated_at: string
  recommendation?: Recommendation
}

export interface FindingsGauges {
  orphaned_members: number
  freeloaders: number
  duplicate_coverage: number
  open_by_severity: Record<string, number>
  total_open: number
}

export interface FindingsListResponse {
  items: Finding[]
  total: number
  limit: number
  offset: number
  gauges: FindingsGauges
}

export interface RepairAlert {
  id: string
  customer_id?: string
  event_type: string
  data?: Record<string, unknown>
  seen: boolean
  created_at: string
}

export interface WorkerHealth {
  worker_kind: string
  registered_at: string
  expected_period_seconds?: number
  last_success_at?: string
  last_error_at?: string
  last_error?: string
  consecutive_failures: number
  last_alerted_at?: string
  updated_at: string
}

// --- Settings / providers ---

export interface MerchantSettings {
  profile?: {
    display_name?: string
    logo_url?: string
    from_email?: string
    support_url?: string
  }
  collection_threshold?: number
  monthly_floor?: number
  billing_period_boundary?: string
  // Destination for operator alert emails (#736). Unset ⇒ the email channel is
  // inactive (fail-soft to in_app + webhooks). RECONCILE: if Agent A nests this
  // under profile, move the read/write in pages/settings/alerts.tsx.
  alert_email?: string
}

export interface PaymentProviderConfig {
  id: string
  rail: Rail
  environment: string
  account_id: string
  archived: boolean
  drained: boolean
  open_obligations: number
  public_config?: Record<string, string>
  credentials: Record<string, { configured: boolean; last_validated_at?: string }>
  first_seen_at: string
  last_validated_at?: string
  replaced_at?: string
  created_at: string
  updated_at: string
}

// --- API keys (#757) ---

export interface MerchantAPIKey {
  id: string
  name: string
  role: string
  // Non-secret leading token part ("openrails_st_<key_id>") for matching a
  // stored credential. The secret itself is shown once, at mint time only.
  prefix: string
  created_at: string
  last_used_at?: string
  expires_at?: string
  revoked_at?: string
}

export interface MintedAPIKey extends MerchantAPIKey {
  secret: string
}

// --- Auth (AuthKit authhttp) ---

export interface AuthCapabilities {
  providers: {
    id: string
    name: string
    kind: string
    supports_login: boolean
  }[]
  password: { login?: boolean; [k: string]: unknown }
  [k: string]: unknown
}

export interface AuthTokens {
  access_token: string
  token_type: string
  expires_in: number
  refresh_token?: string
  requires_2fa?: boolean
  requires_verification?: boolean
}

export interface Me {
  id: string
  email?: string | null
  username?: string
  roles?: string[]
  entitlements?: string[]
}

// --- Alerting (#736) ---

export type AlertSeverity = "warning" | "critical"
export type AlertTemplate =
  | "chargeback_rate"
  | "dunning_spike"
  | "payers_at_depletion_risk"
  | "payment_methods_expiring"
export type WebhookFormat = "generic" | "discord" | "slack"

// AlertChannels is the wire shape for a rule's channel set. in_app is always
// on; email is fail-soft (inactive when merchant alert_email is unset);
// webhook_ids reference merchant_webhooks rows.
// RECONCILE (Agent A as-built): if the engine serializes channels as a typed
// array instead of this object, adjust channelsFromWire/channelsToWire in
// pages/settings/alerts.tsx — the rest of the UI is shape-agnostic.
export interface AlertChannels {
  in_app: boolean
  email: boolean
  webhook_ids: string[]
}

export interface AlertRule {
  id: string
  template: AlertTemplate
  params: Record<string, unknown>
  severity: AlertSeverity
  channels: AlertChannels
  enabled: boolean
  // Edge-trigger state: set while the rule is currently firing.
  fired_at?: string | null
  cleared_at?: string | null
  created_at: string
  updated_at?: string
}

export interface MerchantWebhook {
  id: string
  name: string
  url: string
  format: WebhookFormat
  enabled: boolean
  created_at: string
  updated_at?: string
}

// MerchantNotification is the in_app store — MERCHANT-operator-facing (distinct
// from the customer notification_queue). Surfaced as the header bell.
export interface MerchantNotification {
  id: string
  severity: AlertSeverity
  title: string
  body: string
  link?: string | null
  created_at: string
  read_at?: string | null
}
