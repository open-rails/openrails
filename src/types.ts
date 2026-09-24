// Wire types for the hosted checkout session surface, validated at runtime
// with zod so a drifting API fails loudly at the boundary instead of rendering
// garbage. These schemas are the package's only contract with the host
// backend. OpenRails publishes the same document as Go types
// (openrails.HostedCheckoutSession) and a canonical fixture
// (testdata/wire/hosted_checkout_session.json) that src/types.test.ts decodes.
import { z } from "zod"

import { isAmount, isUnitDecimals, MAX_UNIT_DECIMALS } from "./lib/money"

// Money is an exact signed int64 decimal string of the plan currency's native
// unit ("99000000" is 99 USD at unit_decimals 6). A JSON number is refused:
// it cannot carry every int64 and a scale must never be assumed.
export const amountSchema = z
  .string()
  .refine(isAmount, "amount must be an int64 decimal string")

// The currency's registered scale from OpenRails' currency registry
// (openrails.LookupCurrency / GET /v1/currencies), stamped by the host.
export const unitDecimalsSchema = z
  .number()
  .refine(
    isUnitDecimals,
    `unit_decimals must be an integer from 0 to ${MAX_UNIT_DECIMALS}`
  )

const httpsURLSchema = z
  .string()
  .url()
  .refine((value) => {
    try {
      const parsed = new URL(value)
      return (
        parsed.protocol === "https:" && !parsed.username && !parsed.password
      )
    } catch {
      return false
    }
  }, "redirect URL must be HTTPS")

const returnURLSchema = z
  .string()
  .url()
  .refine((value) => {
    try {
      const parsed = new URL(value)
      return (
        (parsed.protocol === "https:" || parsed.protocol === "http:") &&
        !parsed.username &&
        !parsed.password
      )
    } catch {
      return false
    }
  }, "return URL must be HTTP(S)")

export const checkoutSessionStatusSchema = z.enum([
  "created",
  "requires_action",
  "processing",
  "succeeded",
  "failed",
  "blocked",
  "expired",
  "canceled",
])
export type CheckoutSessionStatus = z.infer<typeof checkoutSessionStatusSchema>

// A definite decline, normalized by OpenRails. `field` places the message
// next to the card field it concerns ("" = the card as a whole).
export const paymentFailureSchema = z.object({
  reason: z.string(),
  message: z.string(),
  field: z.string().nullish(),
})
export type PaymentFailure = z.infer<typeof paymentFailureSchema>

// The accepted payment operation; `requires_action` sessions authenticate it.
export const checkoutOperationSchema = z.object({
  id: z.string(),
  status: z.string(),
})

export const paymentRailOptionSchema = z.object({
  id: z.string().min(1),
  rail: z.string(),
  mode: z.enum(["one_off", "subscription"]),
  driver: z.enum(["collect_js", "stripe_elements", "redirect", "solana_pay"]),
  /** The PSP's checkout key; saving a new card names it. */
  psp_key: z.string().optional(),
  // Browser-safe rail config the host serves (nmi: Collect.js
  // tokenization_key + tokenization_url; stripe: publishable_key).
  public_config: z.record(z.string(), z.string()).optional(),
})
export type PaymentRailOption = z.infer<typeof paymentRailOptionSchema>

// A card the session's customer has already stored with this merchant. Display
// data only — paying with one sends its id, which the host resolves.
export const savedPaymentMethodSchema = z.object({
  id: z.string(),
  option_id: z.string(),
  rail: z.string(),
  brand: z.string().optional(),
  last_four: z.string().optional(),
  exp_month: z.number().optional(),
  exp_year: z.number().optional(),
})
export type SavedPaymentMethod = z.infer<typeof savedPaymentMethodSchema>

// Amounts are in the plan's currency at the plan's unit_decimals.
export const checkoutLineItemSchema = z.object({
  label: z.string(),
  sublabel: z.string().optional(),
  amount: amountSchema,
})
export type CheckoutLineItem = z.infer<typeof checkoutLineItemSchema>

export const checkoutPlanSchema = z.object({
  display_name: z.string(),
  unit_amount: amountSchema,
  currency: z.string().min(1),
  unit_decimals: unitDecimalsSchema,
  period_hours: z.number().nullish(),
  automatically_renews: z.boolean(),
})
export type CheckoutPlan = z.infer<typeof checkoutPlanSchema>

export const checkoutSessionSchema = z.object({
  id: z.string(),
  status: checkoutSessionStatusSchema,
  merchant: z.object({ display_name: z.string() }),
  plan: checkoutPlanSchema,
  line_items: z.array(checkoutLineItemSchema).optional(),
  tax: amountSchema.optional(),
  due_today: amountSchema.optional(),
  rails: z.array(paymentRailOptionSchema),
  saved_methods: z.array(savedPaymentMethodSchema).optional(),
  transaction_url: z.string().startsWith("solana:").optional(),
  payment_id: z.string().optional(),
  subscription_id: z.string().optional(),
  failure_message: z.string().optional(),
  failure: paymentFailureSchema.nullish(),
  operation: checkoutOperationSchema.nullish(),
  // Present on hosted-page reads so the page host can redirect on success.
  success_url: returnURLSchema.optional(),
  expires_at: z.string().nullish(),
})
export type CheckoutSession = z.infer<typeof checkoutSessionSchema>

export const payRequestSchema = z.object({
  option_id: z.string(),
  payment_token: z.string().optional(),
  payment_method_id: z.string().optional(),
  // Canonical billing identity. The compact card paths send only their
  // required fields; optional address fields remain available to an explicit
  // provider-specific source that models them.
  email: z.string().optional(),
  name_on_card: z.string().optional(),
  address1: z.string().optional(),
  city: z.string().optional(),
  state: z.string().optional(),
  zip: z.string().optional(),
  country: z.string().optional(),
  // Collect.js display metadata for a new card (never the PAN).
  last_four: z.string().optional(),
  card_type: z.string().optional(),
  expiry_date: z.string().optional(),
  token_symbol: z.string().optional(),
})
export type PayRequest = z.infer<typeof payRequestSchema>

export const payResultSchema = z.object({
  status: checkoutSessionStatusSchema,
  redirect_url: httpsURLSchema.optional(),
  transaction_url: z.string().startsWith("solana:").optional(),
  payment_id: z.string().optional(),
  subscription_id: z.string().optional(),
  failure_message: z.string().optional(),
  failure: paymentFailureSchema.nullish(),
  /** The payment operation to authenticate when status is requires_action. */
  operation_id: z.string().optional(),
})
export type PayResult = z.infer<typeof payResultSchema>

// The lifecycle phases the Checkout component moves through. Superset of
// session statuses: load/ready concerns are the component's, not the wire's.
export type CheckoutPhase =
  | "loading"
  | "ready"
  | "processing"
  | "succeeded"
  | "failed"
  | "blocked"
  | "expired"
  | "unavailable"
  | "error"
