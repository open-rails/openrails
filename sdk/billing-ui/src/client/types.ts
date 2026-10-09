// Wire types of the OpenRails customer surface (`/billing/v1/me/*`) and public
// catalog, validated at the boundary. Money is an int64 string in the
// currency's native unit; its scale comes from GET /config's currencies.
// Fixtures: src/test/fixtures/wire.
import { z } from "zod"

import { pspConfigSchema } from "../psp"

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
    next_cursor: z.string().nullish(),
  })

export interface Page<T> {
  data: T[]
  total?: number | null
  limit?: number | null
  offset?: number | null
  has_more?: boolean | null
  /** The next page's cursor; null on the last page of a cursor list. */
  next_cursor?: string | null
}

/** One page of a cursor list; `next_cursor` is null on the last page. */
export const listPageSchema = <T extends z.ZodType>(item: T) =>
  z.object({
    data: z
      .array(item)
      .nullish()
      .transform((v) => v ?? []),
    next_cursor: z
      .string()
      .nullish()
      .transform((v) => v ?? null),
  })

export interface ListPage<T> {
  data: T[]
  next_cursor: string | null
}

export const subscriptionStatusSchema = z.string()
/** `pending | active | past_due | awaiting_method | unverified | canceled`; kept open for new values. */
export type SubscriptionStatus =
  | "pending"
  | "active"
  | "past_due"
  | "awaiting_method"
  | "unverified"
  | "canceled"
  | (string & {})

export const pspLinkStateSchema = z.object({
  /** `linked | pending_manual_link | sync_disabled | error` */
  status: z.string(),
  ids: z.record(z.string(), z.string()).nullish(),
  sync_status: z.string().nullish(),
})

/**
 * A catalog price, embedded in a product (`GET /catalog/products`): `unit_amount` of
 * `currency` for `access_duration_hours` of access (null: for good),
 * charging every `billing_interval_hours` (null: one-time).
 */
export const priceSchema = z.object({
  id: z.string(),
  key: z.string(),
  /** Revision within this product/key pair; absent on older servers. */
  revision: z.number().int().nonnegative().optional(),
  product_id: z.string(),
  archived: z.boolean(),
  unit_amount: amount,
  /** Optional bounds for a deposit whose amount is selected before checkout. */
  customer_amount: z
    .object({ min_amount: amount, max_amount: amount })
    .nullish(),
  currency: z.string(),
  access_duration_hours: z.number().nullable(),
  billing_interval_hours: z.number().nullable(),
  trial_unit_amount: amount.nullable(),
  trial_duration_hours: z.number().nullable(),
  /** Keyed by PSP key. */
  psps: z.record(z.string(), pspLinkStateSchema).nullish(),
  created_at: time,
  updated_at: time,
})
export type Price = z.infer<typeof priceSchema>

export const subscriptionProductSchema = z.object({
  id: z.string(),
  key: z.string().nullish(),
  revision: z.number().int().nonnegative().optional(),
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

/**
 * A step the customer takes before an action completes. With
 * `solana_sign_transactions` the wallet signs and sends `transactions` in
 * order; the action is then repeated with the last signature.
 */
export const nextActionSchema = z.object({
  /** `redirect_to_url | payment_authentication | solana_sign_transactions` */
  type: z.string(),
  redirect_to_url: z.object({ url: z.string().nullish() }).nullish(),
  transactions: z.array(z.string()).nullish(),
})
export type NextAction = z.infer<typeof nextActionSchema>

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
  canceled_at: time.nullish(),
  cancel_type: z.string().nullish(),
  resumable: z.boolean().nullish(),
  cancel_scheduled: z.boolean().nullish(),
  /** `reversible | destructive | external_portal` */
  cancel_mode: z.string().nullish(),
  cancel_portal_url: z.string().nullish(),
  access: z.object({ starts_at: time, ends_at: time.nullish() }).nullish(),
  grace_ends_at: time.nullish(),
  next_retry_at: time.nullish(),
  price: priceSchema.nullish(),
  product: subscriptionProductSchema.nullish(),
  scheduled_price: priceSchema.nullish(),
  scheduled_product: subscriptionProductSchema.nullish(),
  card: cardSummarySchema.nullish(),
  recovery: paymentRecoverySchema.nullish(),
  /** Set on an action's answer when the rail needs the customer's step. */
  next_action: nextActionSchema.nullish(),
  created_at: time.nullish(),
  updated_at: time.nullish(),
})
export type Subscription = z.infer<typeof subscriptionSchema> & {
  status: SubscriptionStatus
}

export const billingDetailsSchema = z.object({
  name: z.string().nullish(),
  email: z.string().nullish(),
  phone: z.string().nullish(),
  address: z
    .object({
      line1: z.string().nullish(),
      line2: z.string().nullish(),
      city: z.string().nullish(),
      state: z.string().nullish(),
      postal_code: z.string().nullish(),
      country: z.string().nullish(),
    })
    .nullish(),
})
export type BillingDetails = z.infer<typeof billingDetailsSchema>

export const paymentMethodSchema = z.object({
  id: z.string(),
  rail: z.string().nullish(),
  /** The PSP holding the card; null for a card a custodian holds. */
  psp_id: z.string().nullish(),
  card: cardSummarySchema.nullish(),
  billing_details: billingDetailsSchema.nullish(),
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
  /** Currencies whose invoices this card collects. */
  collection_currencies: z.array(z.string()).nullish(),
  created_at: time.nullish(),
})
export type PaymentMethod = z.infer<typeof paymentMethodSchema>

/** A price's state on one PSP; public routes carry the status only. */
export const productSchema = z.object({
  id: z.string(),
  key: z.string(),
  /** Current mutation counter; absent on older servers. */
  revision: z.number().int().nonnegative().optional(),
  display_name: z.string(),
  description: z.string(),
  /** Opaque entitlement keys granted by the product. */
  entitlements: z.array(z.string()),
  /** Prepaid balance fulfilled once for each qualifying successful payment. */
  credit_grant: z
    .object({
      currency: z.string(),
      amount: amount.nullish(),
      from_payment: z.boolean().optional(),
      expires_after_days: z.number().int().positive().nullish(),
    })
    .nullish(),
  /** Products sharing a group are tiers a subscription can change between. */
  tier_group: z.string().nullable(),
  tier_rank: z.number(),
  archived: z.boolean(),
  prices: z
    .array(priceSchema)
    .nullish()
    .transform((v) => v ?? []),
  created_at: time,
  updated_at: time,
})
export type Product = z.infer<typeof productSchema>

export const paymentSchema = z.object({
  id: z.string(),
  /** `charge | refund | chargeback | dispute_reversal` */
  kind: z.string(),
  /** `succeeded | pending | failed | refunded | partially_refunded` */
  status: z.string(),
  /** Negative for a refund or chargeback. */
  amount: amount,
  amount_refunded: amount.nullish(),
  currency: z.string(),
  subscription_id: z.string().nullish(),
  price_id: z.string().nullish(),
  /** `rail | manual | admin` */
  channel: z.string().nullish(),
  rail: z.string().nullish(),
  created_at: time,
  /** What was bought. */
  price: priceSchema.nullish(),
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
  period_starts_at: time,
  period_ends_at: time,
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
  /** `payment_authentication` uses `operation_id`. */
  next_action: nextActionSchema.nullish(),
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

/**
 * The merchant's browser payment setup: its armed PSPs with their public
 * values and, with a Solana PSP, the network and accepted tokens a wallet
 * adapter is configured with.
 */
export const paymentConfigSchema = z.object({
  psps: z
    .array(pspConfigSchema)
    .nullish()
    .transform((v) => v ?? []),
  solana: z
    .object({
      /** `mainnet | devnet | testnet` */
      network: z.string(),
      /** `solana:<network>` */
      chain: z.string(),
      preferred_token: z.string().nullish(),
      tokens: z
        .array(
          z.object({
            symbol: z.string(),
            name: z.string().nullish(),
            mint: z.string(),
            decimals: z.number().int(),
            preferred: z.boolean().nullish(),
            recurring_eligible: z.boolean().nullish(),
          })
        )
        .nullish()
        .transform((v) => v ?? []),
    })
    .nullish(),
})
export type PaymentConfig = z.infer<typeof paymentConfigSchema>

/**
 * What a browser needs to know about the deployment and its merchant
 * (`GET /config`): the mount's capabilities, the currency registry and the
 * merchant's payment setup (null when the request resolves no merchant).
 */
export const publicConfigSchema = z.object({
  capabilities: z.object({
    route_groups: z.record(z.string(), z.boolean()),
    features: z.record(z.string(), z.boolean()),
  }),
  currencies: z
    .array(currencySchema)
    .nullish()
    .transform((v) => v ?? []),
  payment: paymentConfigSchema.nullish(),
})
export type PublicConfig = z.infer<typeof publicConfigSchema>

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

/**
 * What a card setup hands the server: tokenized data only, never a PAN.
 * OpenRails reads the saved card's brand, last four and expiry from the PSP.
 */
interface NewCardFields {
  /** The PSP the card is saved with (`psp_id` of a rail option); required. */
  psp_id: string
  billing_details?: {
    name?: string
    email?: string
    address?: { postal_code?: string; country?: string }
  }
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

/**
 * A minted checkout session. `id` reads and pays it with no other credential
 * (`client.checkoutSource(id)`); hand it to this customer's browser only.
 * `url` is the shared payment page (`<CheckoutFrame url>`), null when the app
 * renders `<Checkout>` itself.
 */
export const checkoutSessionLinkSchema = z.object({
  id: z.string().startsWith("ocs_"),
  url: z.string().url().nullish(),
  expires_at: time,
})
export type CheckoutSessionLink = z.infer<typeof checkoutSessionLinkSchema>
