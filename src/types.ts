// Wire types for the #45 checkout session surface, validated at runtime with
// zod so a drifting API fails loudly at the boundary instead of rendering
// garbage. These schemas are the package's only contract with the host
// backend.
import { z } from "zod"

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
  "succeeded",
  "failed",
  "blocked",
  "expired",
  "canceled",
])
export type CheckoutSessionStatus = z.infer<typeof checkoutSessionStatusSchema>

export const paymentRailOptionSchema = z.object({
  id: z.string().min(1),
  rail: z.string(),
  mode: z.enum(["one_off", "subscription"]),
  driver: z.enum(["collect_js", "redirect", "solana_pay"]),
  // Browser-safe rail config the host serves (nmi: Collect.js
  // tokenization_key + tokenization_url).
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

export const checkoutLineItemSchema = z.object({
  label: z.string(),
  sublabel: z.string().optional(),
  amount_micros: z.number(),
})
export type CheckoutLineItem = z.infer<typeof checkoutLineItemSchema>

export const checkoutPlanSchema = z.object({
  display_name: z.string(),
  unit_amount_micros: z.number(),
  currency: z.string(),
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
  tax_micros: z.number().optional(),
  due_today_micros: z.number().optional(),
  rails: z.array(paymentRailOptionSchema),
  saved_methods: z.array(savedPaymentMethodSchema).optional(),
  transaction_url: z.string().startsWith("solana:").optional(),
  payment_id: z.string().optional(),
  subscription_id: z.string().optional(),
  failure_message: z.string().optional(),
  // Present on hosted-page reads so the page host can redirect on success.
  success_url: returnURLSchema.optional(),
  expires_at: z.string(),
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
