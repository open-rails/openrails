// Types mirror the Go handlers' JSON shapes exactly (see internal/http/handlers).
// Money is native units at the currency registry scale; exact wires send int64
// decimal strings (docs/money-wire.md).

import type { SubscriptionDunning } from "./generated/wire"

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
  // The change waiting for the next renewal (a migration's move or a
  // scheduled downgrade), or null.
  scheduled_change?: import("./generated/wire").ScheduledChange | null
  quantity: number | null // seats; null for a price without them
  collection_policy?: string
  status: SubscriptionStatus
  started_at: string
  ended_at: string | null
  current_period_starts_at: string | null
  current_period_ends_at: string | null
  rail: Rail
  rail_subscription_id: string | null
  // The subscription's own card; null follows the customer's default card.
  payment_method_id: string | null // pm_...
  // The declined renewal being collected; null unless past_due or
  // awaiting_method.
  dunning: SubscriptionDunning | null
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

// A customer's keys are the keys of the products they hold; quantity is the
// most seats a held per-seat product grants (null: none per seat).
export interface CustomerEntitlement {
  customer_id: string
  entitlement: string
  quantity: number | null
}

export interface RawProductAccessGrant {
  id: string
  customer_id?: string
  product_id: string
  product_key: string
  product_name: string
  source_type: string
  source_id: string
  payment_id?: string | null
  grant_reason?: string | null
  granted_by?: string | null
  note?: string | null
  status: string
  starts_at: string
  ends_at?: string | null
  revoked_at?: string | null
  revoke_reason?: string | null
}

export interface Page<T> {
  data: T[]
  next_cursor: string | null
}

// --- Subscription admin response (list/detail) ---

export type AdminSubscription = RawSubscription

export type {
  ChangeSubscriptionParams,
  SubscriptionChange,
  SubscriptionChangePreview,
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

// PSPRoutingPreview is POST /admin/psps/routing-preview (or#288): which
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

// --- Price migrations ---

export type {
  PriceMigration,
  PriceMigrationCancel,
  PriceMigrationPreview,
  ScheduledChange,
} from "./generated/wire"

// --- Ops: findings and repair alerts ---

export interface Recommendation {
  action: string
  params?: Record<string, unknown>
}

export interface Finding {
  id: string
  provider: string | null
  // Catalog and pull.* findings name the PSP whose read raised them; a catalog
  // finding also names the resource, the field and both values.
  psp_id?: string | null
  resource_type?: string | null
  resource_id?: string | null
  external_resource_id?: string | null
  field?: string | null
  openrails_value?: string | null
  external_value?: string | null
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

// The findings queue at a glance, from the metrics query's findings measures.
export interface FindingsGauges {
  total_open: number
  orphaned_members: number
  freeloaders: number
  duplicate_coverage: number
}

// --- Settings / providers ---

// GET /admin/configuration: the merchant's non-secret configuration and
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
  // DefaultPriceIncreaseNoticeDays (30). Decreases are exempt.
  reprice_notice_window_days?: number
}

// PSP is one merchant account on a rail (GET /admin/psps). Credential
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

// RailDefinition is a rail a PSP can be armed on (GET /config rails).
export interface RailDefinition {
  rail: Rail
  display_name: string
  credential_keys: string[]
  setting_keys: string[]
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

