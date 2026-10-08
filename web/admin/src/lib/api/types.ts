// Types mirror the Go handlers' JSON shapes exactly (see internal/http/handlers).
// Money is native units at the currency registry scale; exact wires send int64
// decimal strings (docs/money-wire.md).

import type {
  Balance,
  Customer,
  Payment,
  PaymentMethod,
} from "./generated/wire"

export type SubscriptionStatus =
  | "pending"
  | "active"
  | "past_due"
  | "awaiting_method"
  | "unverified"
  | "canceled"
export type Rail = "nmi" | "ccbill" | "solana" | "stripe" | string

// --- Shared Client DTOs (subscription and profile endpoints) ---

// RawSubscription mirrors openrails.Subscription: ids of prefixed kinds are
// typed (sub_, prod_, price_, pm_, pay_); customer_id and psp_id are plain UUIDs.
export interface RawSubscription {
  id: string // sub_...
  customer_id: string
  psp_id?: string
  product_id: string // prod_...
  price_id: string // price_...
  scheduled_price_id?: string | null // price_...
  status: SubscriptionStatus
  started_at: string
  ended_at: string | null
  current_period_starts_at: string | null
  current_period_ends_at: string | null
  rail: Rail
  rail_subscription_id: string | null
  payment_method_id: string | null // pm_...
  retry_attempts: number | null
  next_retry_at: string | null
  grace_ends_at: string | null
  cancel_type: string | null
  cancel_feedback: string | null
  canceled_at: string | null
  price?: RawPrice
  created_at: string
  updated_at: string
}

// RawPrice is the catalog Price embedded on subscription and payment
// responses. Money is unit_amount, an exact decimal string.
export interface RawPrice {
  id: string
  product_id?: string
  unit_amount?: string
  currency?: string
  archived?: boolean
  // Key (#774): the durable, movable-pointer handle for this price's
  // version chain — see Price.key.
  key?: string
  access_duration_hours?: number
  billing_interval_hours?: number
  [k: string]: unknown
}

export interface RawEntitlement {
  id: string
  customer_id?: string
  entitlement: string
  starts_at: string
  ends_at: string | null
  source_id: string
  source_type: string
  revoked_at: string | null
  revoke_reason: string | null
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

// CustomerBillingProfile composes the shared Client DTOs each dedicated
// route serves (subscriptions, payments, entitlements, product access).
export interface CustomerBillingProfile {
  customer: Customer
  balances: Balance[]
  subscriptions: AdminSubscription[]
  entitlements: RawEntitlement[]
  payments: Payment[] | null
  payment_methods: PaymentMethod[] | null
  product_access: RawProductAccessGrant[]
}

// --- Subscription admin response (list/detail) ---

export interface AdminSubscription extends RawSubscription {
  // Recovery history: the same Payment shape the payments endpoints serve.
  payments?: Payment[]
}

export type {
  TierChange as TierChangeResult,
  TierChangePreview,
} from "./generated/wire"

// --- Catalog ---

export type UsageAggregation =
  "sum" | "count" | "max" | "min" | "unique_count" | "latest"
export type UsagePriceModel = "per_unit" | "tiered" | "package"


// PSPRoutingSkip mirrors the or#288 skip vocabulary
// (internal/db/models/checkout_session.go). null = the PSP is eligible.
export type PSPRoutingSkip =
  | "unknown_selector"
  | "ambiguous_selector"
  | "not_armed"
  | "credentials_missing"
  | "link_missing"
  | "mode_unsupported"
  | "posture_disarmed"
  | "service_unavailable"
  | "resolve_failed"

export interface PSPRoutingCandidate {
  psp: string
  rail: Rail
  skip: PSPRoutingSkip | null
}

// PSPRoutingPreview is POST /merchant/psps/routing-preview (or#288): which
// PSP a checkout for this price would use, and why every other PSP was passed
// over. Read-only — it creates nothing.
export interface PSPRoutingPreview {
  policy: string
  rule: number | null
  psp: string | null
  rail: Rail | null
  mode: string | null
  candidates: PSPRoutingCandidate[]
}

// --- Price repricing / migration ---

export type RepriceStatus = "scheduled" | "applied" | "canceled" | "blocked"
export type RepriceKind = "reprice" | "plan_change"

// Reprice is one subscription's scheduled price change.
export interface Reprice {
  id: string
  subscription_id: string
  from_price_id: string
  to_price_id: string
  effective_at: string
  status: RepriceStatus
  kind: RepriceKind
  blocked_reason: string | null
  reprice_batch_id: string | null
  acknowledged_short_notice: boolean
  created_at: string
  applied_at: string | null
  canceled_at: string | null
}

// RepriceBatch is one bulk reprice or plan migration. matched and skipped
// are fixed at creation; scheduled/applied/canceled/blocked count its
// reprices now.
export interface RepriceBatch {
  id: string
  kind: RepriceKind
  price_key: string | null
  source_price_id: string | null
  to_price_id: string
  effective_at: string
  fallback_policy: string | null
  matched: number
  skipped: number
  scheduled: number
  applied: number
  canceled: number
  blocked: number
  created_at: string
}

export interface RepriceOutcome {
  subscription_id: string
  reprice_id: string | null
  reason: string | null
  acknowledged_short_notice: boolean
}

export interface RepriceBatchResult {
  batch_id: string
  to_price_id: string
  matched: number
  scheduled: RepriceOutcome[]
  skipped: RepriceOutcome[]
}

// RepriceBatchPreview is the wizard's affected-count dry run, called before
// the price edit lands.
export interface RepriceBatchPreview {
  product_key: string
  price_key: string
  to_price_id: string
  matched: number
}

export interface RepriceBatchCancel {
  canceled: number
  rail_release_required: string[]
  warning: string | null
}

// --- Ops: findings / repair alerts / worker health ---

export interface Recommendation {
  action: string
  params?: Record<string, unknown>
}

export interface Finding {
  id: string
  provider: string | null
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

// GET /merchant/findings/summary: the queue at a glance.
export interface FindingsGauges {
  orphaned_members: number
  freeloaders: number
  duplicate_coverage: number
  open_by_severity: Record<string, number>
  total_open: number
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

// GET /merchant/configuration: the merchant's non-secret configuration and
// the revision an application must name.
export interface MerchantConfiguration {
  revision: string
  display_name: string
  api_host: string
  settings: MerchantSettings
}

export interface MerchantSettings {
  profile?: {
    display_name?: string
    logo_url?: string
    from_email?: string
    support_url?: string
  }
  collection_threshold?: string
  monthly_floor?: string
  billing_period_boundary?: string
  // Destination for operator alert emails (#736), top-level per the as-built
  // engine (service_admission.go). Unset ⇒ the email channel is inactive
  // (fail-soft to in_app + webhooks).
  alert_email?: string
  // Minimum advance-notice window (days) a subscription price INCREASE's
  // effective_at must give existing subscribers (#781). Unset ⇒ the server's
  // DefaultRepriceNoticeWindowDays (30). Decreases are exempt.
  reprice_notice_window_days?: number
}

// PSP is one merchant account on a rail (GET /merchant/psps). Credential
// values are never returned.
export interface PSP {
  id: string // psp_...
  key: string
  rail: Rail
  environment: string
  account_id: string
  archived: boolean
  archived_at: string | null
  // An archived PSP with none is drained.
  open_obligations: number
  settings: Record<string, unknown>
  // rotation_version (or#812) is the cross-node cutover watermark: how many
  // times the credential was rotated through the API.
  credentials: Record<
    string,
    {
      configured: boolean
      validated_at: string | null
      rotation_version: number
    }
  >
  revision: number
  created_at: string
  updated_at: string
}

// RailDefinition is a rail a PSP can be armed on (GET /merchant/rails).
export interface RailDefinition {
  rail: Rail
  display_name: string
  credential_keys: string[]
  setting_keys: string[]
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
  last_used_at: string | null
  expires_at: string | null
  revoked_at: string | null
}

export interface MintedAPIKey extends MerchantAPIKey {
  secret: string
}

// --- Team management (#760) ---

export interface TeamMember {
  user_id: string
  email: string | null
  username: string | null
  role: string
}

export interface TeamInvite {
  id: string
  role: string
  created_at: string
  expires_at: string | null
  redeemed_at: string | null
  revoked_at: string | null
}

// Outcome of inviting an email: either a live account had verified the address
// (added to the team immediately) or a single-use register+join link was minted
// (url shown once for the owner to share).
export interface TeamInviteResult {
  member: TeamMember | null
  invite: TeamInvite | null
  url: string | null
}

// --- Auth (AuthKit authhttp) ---

export interface AuthCapabilities {
  external_login_providers: {
    id: string
    name: string
    supports_login: boolean
  }[]
  password: { login?: boolean; [k: string]: unknown }
  [k: string]: unknown
}

// AuthKit TwoFactorFactor: one shape for sign-in, step-up and management.
// destination is the masked address a code goes to; null for totp.
export interface TwoFactorFactor {
  id: string
  method: string
  is_default: boolean
  destination: string | null
}

// AuthKit TokenSet.
export interface AuthTokens {
  access_token: string
  token_type: string
  expires_in: number
  refresh_token?: string | null
}

// AuthKit AuthResult: the 200 body of every sign-in (/password/login,
// /2fa/verify, /2fa/challenge, /token, /oidc/exchange). A finished sign-in
// carries its tokens; otherwise status names the step it waits on.
export interface AuthResult {
  status:
    | "complete"
    | "second_factor_required"
    | "enrollment_required"
    | "verification_required"
    | "account_recovery_required"
  token_set: AuthTokens | null
  second_factor: SecondFactorStep | null
  recovery: { token: string; expires_at: string; purge_at: string } | null
}

export interface SecondFactorStep {
  user_id: string
  challenge: string
  factor: TwoFactorFactor
  factors: TwoFactorFactor[]
}

export interface Me {
  id: string
  email?: string | null
  username?: string
  roles?: string[]
  entitlements?: string[]
}

// A merchant the signed-in user holds a role in (GET /v1/merchants).
export interface MerchantMembership {
  id: string
  slug: string
  display_name?: string
  role: string
}

export interface MerchantMembershipList {
  object: "list"
  data: MerchantMembership[]
}

// --- Alerting (#736) ---

export type AlertSeverity = "warning" | "critical"
export type WebhookFormat = "generic" | "discord" | "slack"

export interface MerchantWebhook {
  id: string
  name: string
  destination_host: string
  format: WebhookFormat
  enabled: boolean
  created_at: string
  updated_at?: string
}

// MerchantNotification is the in_app store — MERCHANT-operator-facing (distinct
// from customer recipients in notifications). Surfaced as the header bell.
export interface MerchantNotification {
  id: string
  severity: AlertSeverity
  title: string
  body: string
  link?: string | null
  created_at: string
  read_at?: string | null
}
