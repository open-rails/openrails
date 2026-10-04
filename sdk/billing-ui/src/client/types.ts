// Wire types of the OpenRails customer surface (`/billing/v1/me/*`) and public
// catalog, validated at the boundary. Money is an int64 string in the
// currency's native unit; its scale comes from GET /currencies. Fixtures:
// src/test/fixtures/wire.
import { z } from "zod"

import type { CardEntry } from "../lib/card-entry"
import { isAmount } from "../lib/money"

const amount = z.string().refine(isAmount, "amount must be an int64 string")
const time = z.string()

export const pageSchema = <T extends z.ZodType>(item: T) =>
  z.object({
    data: z
      .array(item)
      .nullish()
      .transform((v) => v ?? []),
    total: z.number().nullish(),
    limit: z.number().nullish(),
    offset: z.number().nullish(),
    has_more: z.boolean().nullish(),
  })

export interface Page<T> {
  data: T[]
  total?: number | null
  limit?: number | null
  offset?: number | null
  has_more?: boolean | null
}

export const subscriptionStatusSchema = z.string()
/** `pending | active | past_due | awaiting_method | unverified | cancelled`; kept open for new values. */
export type SubscriptionStatus =
  | "pending"
  | "active"
  | "past_due"
  | "awaiting_method"
  | "unverified"
  | "cancelled"
  | (string & {})

export const subscriptionPriceSchema = z.object({
  id: z.string(),
  key: z.string().nullish(),
  product_id: z.string().nullish(),
  unit_amount: amount,
  currency: z.string(),
  auto_renew: z.boolean().nullish(),
  access_duration_hours: z.number().nullish(),
})
export type SubscriptionPrice = z.infer<typeof subscriptionPriceSchema>

export const subscriptionProductSchema = z.object({
  id: z.string(),
  key: z.string().nullish(),
  display_name: z.string().nullish(),
  description: z.string().nullish(),
  tier_group: z.string().nullish(),
  tier_rank: z.number().nullish(),
})
export type SubscriptionProduct = z.infer<typeof subscriptionProductSchema>

export const cardSummarySchema = z.object({
  brand: z.string().nullish(),
  last4: z.string().nullish(),
  exp_month: z.number().nullish(),
  exp_year: z.number().nullish(),
})
export type CardSummary = z.infer<typeof cardSummarySchema>

export const paymentOperationSchema = z.object({
  id: z.string(),
  status: z.string(),
})
export type PaymentOperation = z.infer<typeof paymentOperationSchema>

export const paymentRecoverySchema = z.object({
  last_failure_reason: z.string().nullish(),
  retryable: z.boolean(),
  blocked_reason: z.string().nullish(),
  operation: paymentOperationSchema.nullish(),
})
export type PaymentRecovery = z.infer<typeof paymentRecoverySchema>

export const subscriptionSchema = z.object({
  id: z.string(),
  status: subscriptionStatusSchema,
  rail: z.string().nullish(),
  product_id: z.string().nullish(),
  price_id: z.string().nullish(),
  psp_id: z.string().nullish(),
  payment_method_id: z.string().nullish(),
  started_at: time.nullish(),
  ended_at: time.nullish(),
  current_period_starts_at: time.nullish(),
  current_period_ends_at: time.nullish(),
  cancelled_at: time.nullish(),
  cancel_type: z.string().nullish(),
  resumable: z.boolean().nullish(),
  cancel_scheduled: z.boolean().nullish(),
  /** `reversible | destructive | external_portal` */
  cancel_mode: z.string().nullish(),
  cancel_portal_url: z.string().nullish(),
  grace_ends_at: time.nullish(),
  next_retry_at: time.nullish(),
  price: subscriptionPriceSchema.nullish(),
  product: subscriptionProductSchema.nullish(),
  scheduled_price: subscriptionPriceSchema.nullish(),
  scheduled_product: subscriptionProductSchema.nullish(),
  card: cardSummarySchema.nullish(),
  recovery: paymentRecoverySchema.nullish(),
  created_at: time.nullish(),
  updated_at: time.nullish(),
})
export type Subscription = z.infer<typeof subscriptionSchema> & {
  status: SubscriptionStatus
}

export const paymentMethodSchema = z.object({
  id: z.string(),
  type: z.string().nullish(),
  rail: z.string().nullish(),
  psp_id: z.string().nullish(),
  card: cardSummarySchema.nullish(),
  billing_details: z
    .object({ name: z.string().nullish(), email: z.string().nullish() })
    .nullish(),
  health: z
    .object({
      /** `valid | expiring_soon | expired` */
      expiry_status: z.string().nullish(),
      last_charged_at: time.nullish(),
      last_charge_outcome: z.string().nullish(),
      active: z.boolean().nullish(),
    })
    .nullish(),
  subscriptions: z
    .array(
      z.object({
        id: z.string(),
        display_name: z.string().nullish(),
      })
    )
    .nullish(),
  /** The customer's default card: pre-selected, never charged implicitly. */
  default: z.boolean().nullish(),
  /** Currencies whose invoices collect from this method by default. */
  collection_default_currencies: z.array(z.string()).nullish(),
  created_at: time.nullish(),
})
export type PaymentMethod = z.infer<typeof paymentMethodSchema>

export const paymentSchema = z.object({
  id: z.string(),
  /** `charge | refund` */
  object: z.string().nullish(),
  /** `succeeded | pending | failed | refunded | partially_refunded` */
  status: z.string().nullish(),
  amount: amount,
  amount_refunded: amount.nullish(),
  currency: z.string(),
  subscription_id: z.string().nullish(),
  rail: z.string().nullish(),
  refunded: z.boolean().nullish(),
  created_at: time,
  price: z
    .object({
      id: z.string().nullish(),
      key: z.string().nullish(),
      product: z.string().nullish(),
      /** `one_time | recurring` */
      type: z.string().nullish(),
      /** Exact hours, e.g. `"720h"`. */
      recurring: z.object({ interval: z.string().nullish() }).nullish(),
    })
    .nullish(),
  /** What was bought; OpenRails newer than v0.160.0. */
  product: subscriptionProductSchema.nullish(),
  card: cardSummarySchema.nullish(),
  /** Normalized decline of a failed payment. */
  failure: z
    .object({
      reason: z.string(),
      message: z.string(),
      field: z.string().nullish(),
    })
    .nullish(),
})
export type Payment = z.infer<typeof paymentSchema>

export const invoiceSchema = z.object({
  id: z.string(),
  currency: z.string(),
  invoice_number: z.string().nullish(),
  period_from: time,
  period_to: time,
  total_amount: amount,
  amount_paid: amount,
  amount_due: amount,
  status: z.string(),
  issued_at: time.nullish(),
  due_at: time.nullish(),
  paid_at: time.nullish(),
  recovery: paymentRecoverySchema.nullish(),
  created_at: time,
})
export type Invoice = z.infer<typeof invoiceSchema>

export const invoicePageSchema = z.object({
  invoices: z
    .array(invoiceSchema)
    .nullish()
    .transform((v) => v ?? []),
  total: z.number().nullish(),
  limit: z.number().nullish(),
  offset: z.number().nullish(),
})

export const billingStatusSchema = z.object({
  has_active_subscription: z.boolean(),
  subscription: subscriptionSchema.nullish(),
  next_renewal_at: time.nullish(),
  entitlements: z
    .array(
      z.object({
        entitlement: z.string(),
        start_at: time.nullish(),
        end_at: time.nullish(),
        revoked_at: time.nullish(),
      })
    )
    .nullish(),
})
export type BillingStatus = z.infer<typeof billingStatusSchema>

/** Currency code (upper case) to its native-unit scale. */
export type CurrencyScales = Readonly<Record<string, number>>

export const currencySchema = z.object({
  code: z.string(),
  /** Native-unit decimals of every amount on the wire. */
  decimals: z.number(),
  /** Decimals the provider settles in. */
  minor_decimals: z.number().nullish(),
})
export type Currency = z.infer<typeof currencySchema>

export const currencyRegistrySchema = z.object({
  currencies: z.array(currencySchema),
})

/** A catalog price (`GET /prices`, embedded in `GET /products`). */
export const priceSchema = z.object({
  id: z.string(),
  key: z.string().nullish(),
  unit_amount: amount,
  currency: z.string(),
  /** `one_time | recurring` */
  type: z.string().nullish(),
  /** Exact hours, e.g. `"720h"`; absent on a one-time price. */
  recurring: z.object({ interval: z.string() }).nullish(),
  /** Product id. */
  product: z.string().nullish(),
  active: z.boolean().nullish(),
  /** PSP keys the price is linked to. */
  providers: z.array(z.string()).nullish(),
  metadata: z.record(z.string(), z.string()).nullish(),
  created_at: time.nullish(),
})
export type Price = z.infer<typeof priceSchema>

export const productSchema = z.object({
  id: z.string(),
  key: z.string().nullish(),
  name: z.string(),
  description: z.string().nullish(),
  /** Keyed by the entitlements the product grants. */
  entitlements_spec: z.record(z.string(), z.number().nullable()).nullish(),
  /** Products sharing a group are tiers a subscription can change between. */
  tier_group: z.string().nullish(),
  tier_rank: z.number().nullish(),
  active: z.boolean().nullish(),
  metadata: z.record(z.string(), z.string()).nullish(),
  created_at: time.nullish(),
  updated_at: time.nullish(),
  prices: z
    .array(priceSchema)
    .nullish()
    .transform((v) => v ?? []),
})
export type Product = z.infer<typeof productSchema>

export const tierChangePreviewSchema = z.object({
  /** `upgrade | downgrade` */
  action: z.string(),
  price_id: z.string(),
  rail: z.string().nullish(),
  currency: z.string(),
  /** Charged immediately; `"0"` for a downgrade. */
  amount_due_now: amount,
  /** The new price, charged at the next renewal. */
  next_charge_amount: amount,
  next_charge_date: time.nullish(),
  /** `now | period_end` */
  effective: z.string(),
  /** The rail finalizes the exact amount (Stripe upgrades). */
  is_estimate: z.boolean().nullish(),
  message: z.string().nullish(),
})
export type TierChangePreview = z.infer<typeof tierChangePreviewSchema>

export const tierChangeSchema = z.object({
  /** `succeeded | processing | requires_action | blocked` */
  status: z.string(),
  /** `upgrade | downgrade` */
  action: z.string().nullish(),
  /** `now | period_end` */
  effective: z.string().nullish(),
  price_id: z.string().nullish(),
  /** The subscription now carrying the plan (an upgrade may open a successor). */
  subscription_id: z.string().nullish(),
  url: z.string().nullish(),
  next_action: z
    .object({
      /** `payment_authentication` uses `operation_id`. */
      type: z.string(),
      redirect_to_url: z.object({ url: z.string().nullish() }).nullish(),
    })
    .nullish(),
  /** When a scheduled downgrade takes effect. */
  delayed_start: time.nullish(),
  currency: z.string().nullish(),
  amount_due_now: amount.nullish(),
  next_charge_amount: amount.nullish(),
  next_charge_date: time.nullish(),
  message: z.string().nullish(),
  /** The durable operation: unresolved while `processing`, the payment to authenticate on `requires_action`. */
  operation_id: z.string().nullish(),
})
export type TierChange = z.infer<typeof tierChangeSchema>

export const solanaCancelTxSchema = z.object({
  transaction: z.string().min(1),
  subscription_pda: z.string().nullish(),
})

const tokenUnits = z.string().regex(/^\d+$/, "units must be a uint64 string")

export const solanaTokenSchema = z.object({
  symbol: z.string(),
  name: z.string().nullish(),
  mint: z.string(),
  decimals: z.number(),
  /** USD price as an exact decimal string; absent without a feed quote. */
  price: z.string().nullish(),
  /** The stablecoin to present first. */
  preferred: z.boolean().nullish(),
  /** Can back a recurring subscription, not only a one-off payment. */
  recurring_eligible: z.boolean().nullish(),
  /** What the requested price costs in this token. */
  quote: z
    .object({
      /** Display decimal of `units`. */
      amount: z.string(),
      /** Exact on-chain base units. */
      units: tokenUnits,
      token_price_usd: z.string().nullish(),
      fx_rate: z.string().nullish(),
      fx_currency: z.string().nullish(),
      quoted_at: time.nullish(),
      expires_at: time.nullish(),
    })
    .nullish(),
  /** The requested wallet's holding. */
  balance: z
    .object({
      amount: z.string(),
      units: tokenUnits,
      /** Covers `quote`. */
      sufficient: z.boolean().nullish(),
    })
    .nullish(),
})
export type SolanaToken = z.infer<typeof solanaTokenSchema>

export const solanaTokensSchema = z.object({
  tokens: z
    .array(solanaTokenSchema)
    .nullish()
    .transform((v) => v ?? []),
})

export const solanaConfigSchema = z.object({
  /** `mainnet | devnet | testnet` */
  network: z.string(),
  /** `solana:<network>` */
  chain: z.string().nullish(),
  /** Never set by OpenRails: wallets bring their own RPC. */
  rpcUrl: z.string().nullish(),
  /** Explorer `?cluster=` value; absent on mainnet. */
  explorerCluster: z.string().nullish(),
  preferredToken: z.string().nullish(),
  tokens: z
    .array(solanaTokenSchema)
    .nullish()
    .transform((v) => v ?? []),
  features: z
    .object({
      solanaPay: z.boolean().nullish(),
      recurringSubscriptions: z.boolean().nullish(),
      solanaPayRecurringSubscriptions: z.boolean().nullish(),
    })
    .nullish(),
})
export type SolanaConfig = z.infer<typeof solanaConfigSchema>

export const solanaTierChangeTxSchema = z.object({
  /** Base64; partially signed for an upgrade, unsigned for a downgrade. */
  transaction: z.string().min(1),
  /** `upgrade | downgrade` */
  kind: z.string().nullish(),
  new_subscription_pda: z.string().nullish(),
})
export type SolanaTierChangeTx = z.infer<typeof solanaTierChangeTxSchema>

export const solanaTierChangeSchema = z.object({
  /** The subscription now carrying the plan. */
  subscription_id: z.string().nullish(),
  new_subscription_id: z.string().nullish(),
  /** `upgrade | downgrade` */
  kind: z.string().nullish(),
  status: z.string().nullish(),
  /** An earlier confirm already recorded this change. */
  already_confirmed: z.boolean().nullish(),
})
export type SolanaTierChange = z.infer<typeof solanaTierChangeSchema>

/** An in-page card setup; `payment_method_id` is set once the card is saved. */
export const cardSetupSchema = z.object({
  id: z.string(),
  status: z.string(),
  client_secret: z.string().nullish(),
  payment_method_id: z.string().nullish(),
})
export type CardSetup = z.infer<typeof cardSetupSchema>

export const paymentAuthenticationSchema = z.object({
  client_secret: z.string().nullish(),
})
export type PaymentAuthentication = z.infer<typeof paymentAuthenticationSchema>

/** What a card setup hands the server: tokenized data only, never a PAN. */
interface NewCardFields {
  /** OpenRails PSP key the card is saved with (e.g. "nmi"); required. */
  provider: string
  name_on_card?: string
  country?: string
  zip?: string
  email?: string
  /** Collect.js display metadata: last four digits, brand, MM/YY. */
  last_four?: string
  card_type?: string
  expiry_date?: string
}

/**
 * A card to save: a token from the PSP's own fields (`payment_token`), or,
 * for a PSP whose card_entry is server (`cardSetupDriver` "card"), the card
 * itself, posted to OpenRails and never kept by this package.
 */
export type NewCard = NewCardFields &
  (
    | { payment_token: string; card?: never }
    | { card: CardEntry; payment_token?: never }
  )
