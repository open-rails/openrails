// Wire types of the OpenRails customer surface (`/billing/v1/me/*`) and public
// catalog, validated at the boundary. Money is an int64 string in the
// currency's native unit; its scale comes from GET /config's currencies.
// Fixtures: the repository's canonical testdata/wire.
import { z } from "zod"

import { pspConfigSchema } from "../psp"

import type { CardEntry } from "../lib/card-entry"
import { isAmount } from "../lib/money"

import type * as wire from "./generated/wire"

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

/** The provider that owns and runs a provider price. */
export const priceProviderSchema = z.object({
  psp_id: z.string(),
  access_duration_default: z.boolean().optional(),
  drift: z
    .array(
      z.object({
        field: z.string(),
        openrails_value: z.string(),
        remote_value: z.string(),
      })
    )
    .nullish(),
  psp: z.string(),
  rail: z.string(),
  /** The provider's identifiers; empty in a buyer's view. */
  ids: z.record(z.string(), z.string()).nullish(),
  /** `unknown | in_sync | drifted | missing | sync_disabled`, after a verifying read. */
  sync_status: z.string().nullish(),
})

/**
 * A catalog price, embedded in a product (`GET /products`): `unit_amount` of
 * `currency` for `access_duration_hours` of access (null: for good),
 * charging every `billing_interval_hours` (null: one-time).
 */
export const priceSchema = z.object({
  id: z.string(),
  key: z.string(),
  /** The price key's revision: every change to any of its versions; absent on older servers. */
  revision: z.number().int().nonnegative().optional(),
  /** This price's terms within its key, from zero; absent on older servers. */
  version: z.number().int().nonnegative().optional(),
  product_id: z.string(),
  archived: z.boolean(),
  unit_amount: amount,
  /** Optional bounds for a deposit whose amount is selected before purchase. */
  customer_amount: z
    .object({ min_amount: amount, max_amount: amount })
    .nullish(),
  currency: z.string(),
  access_duration_hours: z.number().nullable(),
  billing_interval_hours: z.number().nullable(),
  billing_interval_months: z.number().int().positive().nullish(),
  quantity: z
    .object({
      min: z.number().int().positive(),
      max: z.number().int().positive(),
    })
    .nullish(),
  solana_plan: z.string().nullish(),
  trial_unit_amount: amount.nullable(),
  trial_duration_hours: z.number().nullable(),
  /** The methods one of ours sells through (`card`, `solana`); null: every method. */
  sell_via: z.array(z.string()).nullish(),
  /** Set on a provider price: the PSP that owns and runs it. Null: ours. */
  provider: priceProviderSchema.nullish(),
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

export const paymentRecoverySchema = z.object({
  last_failure_reason: z.string().nullish(),
  retryable: z.boolean(),
  blocked_reason: z.string().nullish(),
  /** An engine-owned subscription's unpaid renewal: the order to pay. */
  order_id: z.string().nullish(),
})
export type PaymentRecovery = z.infer<typeof paymentRecoverySchema>

/**
 * What the customer completes before an action or payment can finish:
 * `authenticate` runs the PSP's own script with `payload` (Stripe.js
 * `handleNextAction` with `client_secret`), then the payable is confirmed;
 * `redirect_to_url` leaves for `url`; with `solana_sign_transactions` the
 * wallet signs and sends `transactions` in order and the action is repeated
 * with the last signature. `capture` initializes vendor-owned card fields
 * from the scoped payload, then confirms the pending payment method with
 * the vendor session token. Capturing never charges the card.
 */
export const nextActionSchema = z.object({
  type: z.string(),
  url: z.string().nullish(),
  psp_id: z.string().nullish(),
  transactions: z.array(z.string()).nullish(),
  payload: z.record(z.string(), z.string()).nullish(),
})
export type NextAction = z.infer<typeof nextActionSchema>

/**
 * A declined renewal being collected. `retries_left` and `final_retry_at`
 * follow the merchant's dunning policy; both are null when the provider runs
 * the retries. While `waiting_for_new_card`, nothing is charged until the card
 * is replaced, and the subscription ends at `final_retry_at`.
 */
export const subscriptionDunningSchema = z.object({
  attempts: z.number(),
  retries_left: z.number().nullish(),
  next_retry_at: time.nullish(),
  final_retry_at: time.nullish(),
  waiting_for_new_card: z.boolean(),
  last_failure_reason: z.string().nullish(),
})
export type SubscriptionDunning = z.infer<typeof subscriptionDunningSchema>

export const subscriptionSchema = z.object({
  id: z.string(),
  status: subscriptionStatusSchema,
  rail: z.string().nullish(),
  product_id: z.string().nullish(),
  price_id: z.string().nullish(),
  /** Seats of a per-seat price; null otherwise. */
  quantity: z.number().nullish(),
  psp_id: z.string().nullish(),
  /** `engine | nmi_schedule | provider` */
  collection_policy: z.string().nullish(),
  /** The subscription's own card; null follows the default card. */
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
  /** Null unless the subscription is `past_due` or `awaiting_method`. */
  dunning: subscriptionDunningSchema.nullish(),
  price: priceSchema.nullish(),
  product: subscriptionProductSchema.nullish(),
  /** The change waiting for the next renewal, with its price and product. */
  scheduled_change: z
    .object({
      price_id: z.string(),
      quantity: z.number().nullish(),
      effective_at: time,
      /** `change | migration` */
      source: z.string(),
      price: priceSchema.nullish(),
      product: subscriptionProductSchema.nullish(),
    })
    .nullish(),
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
  /** `requires_action` while vendor card entry or bank authentication waits. */
  status: z.string().nullish(),
  /** Vendor card capture or the bank's authentication of a card being saved. */
  next_action: z
    .object({
      type: z.string(),
      psp_id: z.string().nullish(),
      payload: z.record(z.string(), z.string()).nullish(),
    })
    .nullish(),
  rail: z.string().nullish(),
  /** The PSP holding the card; null for a card a custodian holds. */
  psp_id: z.string().nullish(),
  card: cardSummarySchema.nullish(),
  billing_details: billingDetailsSchema.nullish(),
  /** Kept for one-click buys; a card stored only for a subscription is not. */
  reusable: z.boolean().nullish(),
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
  /** Currencies whose usage orders this card pays. */
  default_currencies: z.array(z.string()).nullish(),
  created_at: time.nullish(),
})
export type PaymentMethod = z.infer<typeof paymentMethodSchema>

/** Why the latest payment attempt failed, in the customer's words. */
export const paymentFailureSchema = z.object({
  reason: z.string(),
  message: z.string(),
  field: z.string().nullish(),
})
export type PaymentFailure = z.infer<typeof paymentFailureSchema>

/**
 * One line of an order. A purchased line names its price and product; a
 * usage order's line names its accrual source (`metered:<meter>`) instead.
 */
export const orderLineSchema = z.object({
  id: z.string(),
  price_id: z.string().nullish(),
  product_id: z.string().nullish(),
  source: z.string().nullish(),
  description: z.string(),
  quantity: z.number().nullish(),
  unit_amount: amount,
  amount: amount,
  billing_interval_hours: z.number().nullish(),
  access_duration_hours: z.number().nullish(),
  subscription_id: z.string().nullish(),
  product_access_id: z.string().nullish(),
})
export type OrderLine = z.infer<typeof orderLineSchema>

/**
 * A purchase, a subscription's renewal (`origin: subscription`) or usage
 * billed in arrears (`origin: usage`): frozen lines and total, read and paid
 * by its own customer. `status` is the order's (`open | processing |
 * complete | canceled | expired | void | uncollectible`); `payment.status`
 * its payment's (`requires_payment_method | requires_action | processing |
 * succeeded`).
 */
export const orderSchema = z.object({
  id: z.string(),
  customer_id: z.string(),
  origin: z.string(),
  /** Only `engine` orders accept generic payment commands. */
  collection_owner: z.string().nullish(),
  collection_psp_id: z.string().nullish(),
  provider_bills: z
    .array(
      z.object({
        psp_id: z.string(),
        kind: z.string(),
        reference: z.string(),
        settlement_source: z.string().nullish(),
        non_cash_amount: amount,
        settled_at: time.nullish(),
      })
    )
    .nullish()
    .transform((bills) => bills ?? []),
  status: z.string(),
  number: z.number().int().nullish(),
  currency: z.string(),
  total: amount,
  lines: z.array(orderLineSchema),
  payment: z.object({
    status: z.string(),
    next_action: nextActionSchema.nullish(),
    last_payment_error: paymentFailureSchema.nullish(),
    payment_method_id: z.string().nullish(),
    payment_id: z.string().nullish(),
  }),
  payment_options: z
    .array(
      z.object({
        psp_id: z.string(),
        rail: z.string(),
        accepts: z.array(z.string()),
        saved_payment_method_ids: z.array(z.string()).optional(),
        /** The order's failed payments through the PSP. */
        failed_attempts: z
          .number()
          .nullish()
          .transform((v) => v ?? 0),
      })
    )
    .nullish()
    .transform((v) => v ?? []),
  /** Where the customer pays the order; only on the answer that made it. */
  pay_url: z.string().nullish(),
  /** When the pay URL stops working; null without one. */
  pay_url_expires_at: time.nullish(),
  /** The merchant's page frames the pay URL. */
  embedded: z
    .boolean()
    .nullish()
    .transform((v) => v ?? false),
  /** A merchant order's return; none on a customer's embedded one. */
  success_url: z.string().nullish(),
  cancel_url: z.string().nullish(),
  saved_payment_methods: z
    .boolean()
    .nullish()
    .transform((v) => v ?? false),
  renewal: z
    .object({
      subscription_id: z.string(),
      period_starts_at: time,
      period_ends_at: time,
      due_at: time,
    })
    .nullish(),
  /**
   * A usage order's period, when it is due and how much of it is paid (a
   * funded balance and remittances pay part of it). `delinquent`: overdue
   * past grace, so new usage in its currency is refused until it is paid.
   */
  usage: z
    .object({
      period_starts_at: time,
      period_ends_at: time,
      due_at: time,
      amount_paid: amount,
      amount_due: amount,
      collection_method: z.string(),
      next_collection_attempt_at: time.nullish(),
      po_number: z.string().nullish(),
      memo: z.string().nullish(),
      delinquent: z.boolean(),
    })
    .nullish(),
  expires_at: time.nullish(),
  completed_at: time.nullish(),
  canceled_at: time.nullish(),
  expired_at: time.nullish(),
  voided_at: time.nullish(),
  uncollectible_at: time.nullish(),
  created_at: time,
})
export type Order = z.infer<typeof orderSchema>

/** Why a line cannot be bought: `already_owned`, `entitlement_required`, `unavailable`, ... */
export const orderLineRefusalSchema = z.object({
  code: z.string(),
  message: z.string(),
  owned_by: z.string().nullish(),
  hint: z.string().nullish(),
  /** `entitlement_required`: the required keys the customer lacks. */
  missing: z.array(z.string()).optional(),
})
export type OrderLineRefusal = z.infer<typeof orderLineRefusalSchema>

/** One line priced as an order would freeze it, or why it cannot be bought. */
export const orderPreviewLineSchema = z.object({
  price_id: z.string(),
  product_id: z.string(),
  description: z.string(),
  quantity: z.number().nullish(),
  unit_amount: amount,
  amount: amount,
  ownership: z.string(),
  billing_interval_hours: z.number().nullish(),
  access_duration_hours: z.number().nullish(),
  refusal: orderLineRefusalSchema.nullish(),
})
export type OrderPreviewLine = z.infer<typeof orderPreviewLineSchema>

/**
 * The customer's cart, priced live: one per customer of a merchant, shared by
 * the merchant's sites. `last_changed_at` and `expires_at` are null while it
 * is empty.
 */
export const cartSchema = z.object({
  currency: z.string(),
  total: amount,
  lines: z.array(orderPreviewLineSchema),
  payment_options: orderSchema.shape.payment_options,
  last_changed_at: time.nullish(),
  expires_at: time.nullish(),
})
export type Cart = z.infer<typeof cartSchema>

/**
 * How to pay an order: a saved card, a card just entered (the token of the
 * PSP's own fields, its `psp_id`), a Solana wallet (`token_symbol`, the
 * token it pays in) or the PSP's hosted page (`return_url`).
 */
export type OrderPaymentInput =
  | { payment_method_id: string }
  | {
      token: string
      psp_id?: string
      billing_details?: NewCard["billing_details"]
    }
  | { token_symbol: string; psp_id?: string }
  | { return_url: string; psp_id?: string }

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
  /** Keys a customer must already hold to order the product. */
  required_entitlements: z.array(z.string()).optional(),
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
  /** The bill this money movement settles or reverses. */
  order_id: z.string().nullish(),
  /** `rail | manual | admin` */
  channel: z.string().nullish(),
  rail: z.string().nullish(),
  created_at: time,
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

/**
 * An entitlement key the customer holds through a product. `quantity` is the
 * most seats a held per-seat product grants it; null when none is per seat.
 */
export const customerEntitlementSchema: z.ZodType<wire.CustomerEntitlement> =
  z.object({
    customer_id: z.string(),
    entitlement: z.string(),
    quantity: z.number().int().nullable(),
  })
export type CustomerEntitlement = wire.CustomerEntitlement

/**
 * A customer's money in one currency, as of `as_of`. `accrued_amount` is what
 * its running charges have charged and the ledger does not hold yet, already
 * taken off `available_amount`; until `depletes_at` (null with no running
 * charge) show `available_amount - accrual_rate_per_hour x hours since as_of`.
 */
export const balanceSchema = z.object({
  currency: z.string(),
  billing_mode: z.string(),
  balance_amount: amount,
  held_amount: amount,
  accrued_amount: amount,
  available_amount: amount,
  owed_amount: amount,
  accrual_rate_per_hour: amount,
  as_of: z.string(),
  depletes_at: z.string().nullable(),
})
export type Balance = z.infer<typeof balanceSchema>

/**
 * The low-balance line a balance is judged at: the customer is told when what
 * it may spend is under the larger of `amount` and its burn rate times
 * `runway_hours`. `source` is `customer`, `merchant` or `default`.
 */
export const lowBalanceLineSchema = z.object({
  currency: z.string(),
  amount: amount,
  runway_hours: z.number().int(),
  source: z.enum(["customer", "merchant", "default"]),
})
export type LowBalanceLine = z.infer<typeof lowBalanceLineSchema>

/**
 * The signed-in customer's summary (`GET /me`): money per currency, the
 * default card of each currency, and the low-balance line of each balance.
 */
export const accountSchema = z.object({
  id: z.string(),
  balances: z.array(balanceSchema),
  default_payment_methods: z.array(
    z.object({ currency: z.string(), payment_method_id: z.string() })
  ),
  low_balance_lines: z.array(lowBalanceLineSchema),
})
export type Account = z.infer<typeof accountSchema>

/**
 * One group of a usage report: its events and their total `amount`.
 * `dimensions` are the summed event dimensions when grouped by event type.
 * Grouped by meter, `key` is the meter, `cell` its matrix cell and
 * `quantity` what it priced.
 */
export const usageRowSchema = z.object({
  key: z.string(),
  cell: z.string().nullish(),
  event_count: z.number(),
  amount,
  quantity: z.number().nullish(),
  dimensions: z.record(z.string(), z.number()).nullish(),
})
export type UsageRow = z.infer<typeof usageRowSchema>

/**
 * One page of the customer's usage in `currency` over [`from`, `to`): its
 * groups by total amount, largest first, then by key. `next_cursor` reads the
 * next page; null on the last.
 */
export const usageSchema = z.object({
  customer_id: z.string(),
  currency: z.string(),
  from: time,
  to: time,
  /** `event_type | resource | invoker`; kept open for new values. */
  group_by: z.string(),
  rows: z.array(usageRowSchema),
  next_cursor: z
    .string()
    .nullish()
    .transform((v) => v ?? null),
})
export type Usage = z.infer<typeof usageSchema>

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

export const subscriptionChangePreviewSchema = z.object({
  price_id: z.string(),
  /** Seats of a per-seat price; null otherwise. */
  quantity: z.number().nullish(),
  rail: z.string().nullish(),
  currency: z.string(),
  /** Charged immediately; `"0"` for a change at period end. */
  amount_due_now: amount,
  /** The new price, charged at the next renewal. */
  next_charge_amount: amount,
  next_charge_date: time.nullish(),
  /** `now | period_end` */
  effective: z.string(),
  /** The rail finalizes the exact amount (Stripe provider changes). */
  is_estimate: z.boolean().nullish(),
  message: z.string().nullish(),
})
export type SubscriptionChangePreview = z.infer<
  typeof subscriptionChangePreviewSchema
>

/** A price the subscription can change to now, with the change's preview. */
export const subscriptionChangeOptionSchema = z.object({
  product: subscriptionProductSchema,
  price: priceSchema,
  preview: subscriptionChangePreviewSchema,
})
export type SubscriptionChangeOption = z.infer<
  typeof subscriptionChangeOptionSchema
>

export const subscriptionChangeSchema = z.object({
  /** `succeeded | processing | requires_action | blocked` */
  status: z.string(),
  /** `now | period_end` */
  effective: z.string().nullish(),
  price_id: z.string().nullish(),
  quantity: z.number().nullish(),
  /** The subscription now carrying the plan (an upgrade may open a successor). */
  subscription_id: z.string().nullish(),
  /** On `requires_action`: complete it, then `confirmSubscriptionChange`. */
  next_action: nextActionSchema.nullish(),
  /** When a change at period end takes effect. */
  delayed_start: time.nullish(),
  currency: z.string().nullish(),
  amount_due_now: amount.nullish(),
  next_charge_amount: amount.nullish(),
  next_charge_date: time.nullish(),
  message: z.string().nullish(),
})
export type SubscriptionChange = z.infer<typeof subscriptionChangeSchema>

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
 * The challenge to solve when a request answers 403 `captcha_required`: load
 * `script_url`, solve with `site_key` and `action`, and resend the request
 * with the token in the `token_header` header.
 */
export const captchaConfigSchema = z.object({
  provider: z.string(),
  site_key: z.string(),
  script_url: z.string(),
  action: z.string(),
  token_header: z.string(),
})
export type CaptchaConfig = z.infer<typeof captchaConfigSchema>

/** The merchant as its customers see it; logo and support are https. */
export const publicMerchantSchema = z.object({
  display_name: z.string(),
  logo_url: z.string().nullish(),
  support_url: z.string().nullish(),
  terms_url: z.string().nullish(),
  privacy_url: z.string().nullish(),
  /** The name card charges carry on statements. */
  statement_descriptor: z.string().nullish(),
  /** The origins that may frame its embedded payment pages. */
  embed_origins: z
    .array(z.string())
    .nullish()
    .transform((v) => v ?? []),
})
export type PublicMerchant = z.infer<typeof publicMerchantSchema>

/**
 * What any browser needs to know about the deployment (`GET /config`, no
 * credential): the mount's capabilities, the currency registry and the
 * captcha (null when the deployment challenges nobody).
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
  captcha: captchaConfigSchema.nullish(),
  /**
   * The payment page's sign-in to the customer network (OpenID Connect,
   * code and PKCE); null without the network or a payment page.
   */
  network: z.object({ issuer: z.string(), client_id: z.string() }).nullish(),
})
export type PublicConfig = z.infer<typeof publicConfigSchema>

/**
 * What a customer's browser needs to know about the merchant it pays
 * (`GET /me/config`): its payment setup and how it presents itself.
 */
export const customerConfigSchema = z.object({
  payment: paymentConfigSchema,
  merchant: publicMerchantSchema,
  /** The customer's country as the merchant's proxy reports it, or "". */
  client_country: z
    .string()
    .nullish()
    .transform((v) => v ?? ""),
})
export type CustomerConfig = z.infer<typeof customerConfigSchema>

export const networkLinkChallengeSchema = z.object({
  id: z.string(),
  nonce: z.string(),
  expires_at: time,
})
export type NetworkLinkChallenge = z.infer<typeof networkLinkChallengeSchema>

export const customerLinkSchema = z.object({
  id: z.string(),
  merchant_id: z.string(),
  customer_id: z.string(),
  account_id: z.string(),
  method: z.string(),
  status: z.string(),
  linked_at: time,
  revoked_at: time.nullish(),
  revoked_by: z.string().nullish(),
})
export type CustomerLink = z.infer<typeof customerLinkSchema>

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
 * A card to save: the single-use token of the PSP's own fields (a Collect.js
 * token, a Stripe `pm_`), or, for a PSP whose card_entry is server
 * (`cardSetupDriver` "card"), the card itself, posted to OpenRails and never
 * kept by this package. A HyperSwitch capture starts without either; reuse an
 * Idempotency-Key and confirm the pending method with its vendor session token.
 */
export type NewCard = NewCardFields &
  (
    | { token: string; card?: never }
    | { card: CardEntry; token?: never }
    | { token?: never; card?: never }
  )

/** A token issued by the vendor session of an owned pending payment method. */
export interface CardCaptureReference {
  custodian_id: string
  session_id: string
  token: string
}
