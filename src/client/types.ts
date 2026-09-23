// Wire types of the OpenRails customer surface (`/billing/v1/me/*`), validated
// at the boundary. Money is an int64 string in the currency's native unit;
// its scale comes from GET /currencies. Fixtures: src/test/fixtures/wire.
import { z } from "zod"

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
/** `pending | active | past_due | cancelled | unknown`; kept open for new values. */
export type SubscriptionStatus =
  "pending" | "active" | "past_due" | "cancelled" | "unknown" | (string & {})

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
    .object({ id: z.string().nullish(), product: z.string().nullish() })
    .nullish(),
  card: cardSummarySchema.nullish(),
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

export const solanaCancelTxSchema = z.object({
  transaction: z.string().min(1),
  subscription_pda: z.string().nullish(),
})

/** What a card setup hands the server: tokenized data only, never a PAN. */
export interface NewCard {
  /** OpenRails PSP key that issued the token (e.g. "nmi"); required. */
  provider: string
  payment_token: string
  name_on_card?: string
  country?: string
  zip?: string
  email?: string
}
