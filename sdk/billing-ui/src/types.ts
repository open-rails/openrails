// Wire types for the checkout session surface, validated at runtime with zod
// so a drifting API fails loudly at the boundary instead of rendering garbage.
// OpenRails serves the session (GET /v1/checkout-sessions/{id}) and pins its
// shape in a canonical fixture (testdata/wire/checkout_session.json) that
// src/types.test.ts decodes.
import { z } from "zod"

import { isAmount, isUnitDecimals, MAX_UNIT_DECIMALS } from "./lib/money"

// optional reads a field OpenRails always sends, null when empty, as
// undefined.
const optional = <T extends z.ZodType>(schema: T) =>
  schema
    .nullish()
    .transform((value) => value ?? undefined)
    .optional()

// Money is an exact signed int64 decimal string of the plan currency's native
// unit ("99000000" is 99 USD at unit_decimals 6). A JSON number is refused:
// it cannot carry every int64 and a scale must never be assumed.
export const amountSchema = z
  .string()
  .refine(isAmount, "amount must be an int64 decimal string")

// The currency's registered scale from OpenRails' currency registry
// (openrails.LookupCurrency / GET /v1/config), stamped by the host.
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

export const paymentOptionSchema = z.object({
  id: z.string().min(1),
  /** The PSP the option pays on; saving a Stripe Elements card names it. */
  psp_id: optional(z.string()),
  rail: z.string(),
  mode: z.enum(["one_off", "subscription"]),
  // card: the PSP takes cards on OpenRails itself (card_entry: server); the
  // page posts the card and loads no gateway script.
  driver: z.enum([
    "collect_js",
    "card",
    "stripe_elements",
    "redirect",
    "solana_pay",
  ]),
  /** The PSP's checkout key; saving a new card names it. */
  psp_key: optional(z.string()),
  // Browser-safe rail config (nmi: Collect.js tokenization_key +
  // tokenization_url; stripe: publishable_key; driver card: none).
  public_config: optional(z.record(z.string(), z.string())),
})
export type PaymentOption = z.infer<typeof paymentOptionSchema>

// The step a payment awaits: Stripe's or CCBill's page (an https URL, opened
// in the top window), or a Solana Pay link shown as a QR code.
export const nextActionSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("redirect_to_url"), url: httpsURLSchema }),
  z.object({
    type: z.literal("solana_pay"),
    url: z.string().startsWith("solana:"),
  }),
])
export type NextAction = z.infer<typeof nextActionSchema>

// A card the session's customer has already stored with this merchant. Display
// data only — paying with one sends its id, which the host resolves.
export const savedPaymentMethodSchema = z.object({
  id: z.string(),
  option_id: z.string(),
  rail: z.string(),
  card: z
    .object({
      brand: z.string().nullish(),
      last4: z.string().nullish(),
      exp_month: z.number().nullish(),
      exp_year: z.number().nullish(),
    })
    .nullish(),
})
export type SavedPaymentMethod = z.infer<typeof savedPaymentMethodSchema>

// Amounts are in the plan's currency at the plan's unit_decimals.
export const checkoutLineItemSchema = z.object({
  label: z.string(),
  sublabel: optional(z.string()),
  amount: amountSchema,
})
export type CheckoutLineItem = z.infer<typeof checkoutLineItemSchema>

export const checkoutPlanSchema = z.object({
  auto_renew: z.boolean(),
  display_name: z.string(),
  unit_amount: amountSchema,
  currency: z.string().min(1),
  unit_decimals: unitDecimalsSchema,
  billing_interval_hours: optional(z.number()),
  access_duration_hours: optional(z.number()),
})
export type CheckoutPlan = z.infer<typeof checkoutPlanSchema>

export const checkoutSessionSchema = z.object({
  id: z.string(),
  status: checkoutSessionStatusSchema,
  merchant: z.object({ display_name: z.string() }),
  plan: checkoutPlanSchema,
  line_items: optional(z.array(checkoutLineItemSchema)),
  tax: optional(amountSchema),
  due_today: optional(amountSchema),
  options: z.array(paymentOptionSchema),
  saved_methods: optional(z.array(savedPaymentMethodSchema)),
  next_action: nextActionSchema.nullish(),
  payment_id: optional(z.string()),
  subscription_id: optional(z.string()),
  failure_message: optional(z.string()),
  failure: paymentFailureSchema.nullish(),
  operation: checkoutOperationSchema.nullish(),
  // Lets the page's host redirect on success.
  success_url: optional(returnURLSchema),
  // The origin of the app that framed the payment page: the only origin the
  // page exchanges frame messages with.
  embed_origin: optional(z.string().url()),
  expires_at: optional(z.string()),
})
export type CheckoutSession = z.infer<typeof checkoutSessionSchema>

export const payRequestSchema = z.object({
  option_id: z.string(),
  payment_token: z.string().optional(),
  payment_method_id: z.string().optional(),
  // A new card for a rail whose driver is card; never beside payment_token.
  card: z
    .object({
      number: z.string(),
      exp_month: z.number(),
      exp_year: z.number(),
      cvc: z.string(),
    })
    .optional(),
  // A new card's billing identity; the compact card form sends the name,
  // postal code and country.
  billing_details: z
    .object({
      name: z.string().optional(),
      email: z.string().optional(),
      phone: z.string().optional(),
      address: z
        .object({
          line1: z.string().optional(),
          line2: z.string().optional(),
          city: z.string().optional(),
          state: z.string().optional(),
          postal_code: z.string().optional(),
          country: z.string().optional(),
        })
        .optional(),
    })
    .optional(),
  token_symbol: z.string().optional(),
})
export type PayRequest = z.infer<typeof payRequestSchema>

export const payResultSchema = z.object({
  status: checkoutSessionStatusSchema,
  next_action: nextActionSchema.nullish(),
  /** The card payment to authenticate when status is requires_action. */
  operation: checkoutOperationSchema.nullish(),
  payment_id: optional(z.string()),
  subscription_id: optional(z.string()),
  failure_message: optional(z.string()),
  failure: paymentFailureSchema.nullish(),
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
