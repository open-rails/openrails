// CCBill billing identity rules — mirrors the engine's requireBillingFields
// (email + 6 identity fields; State optional; country ISO-3166 alpha-2).
import { z } from "zod"

export const ccbillBillingSchema = z.object({
  email: z.string().email("Enter a valid email"),
  first_name: z.string().trim().min(1, "First name is required"),
  last_name: z.string().trim().min(1, "Last name is required"),
  address1: z.string().trim().min(1, "Address is required"),
  city: z.string().trim().min(1, "City is required"),
  state: z.string().trim().optional(),
  zip: z.string().trim().min(1, "ZIP is required"),
  country: z
    .string()
    .trim()
    .regex(/^[A-Za-z]{2}$/, "Use the 2-letter country code"),
})

export type CCBillBilling = z.infer<typeof ccbillBillingSchema>

export const emptyCCBillBilling: CCBillBilling = {
  email: "",
  first_name: "",
  last_name: "",
  address1: "",
  city: "",
  state: "",
  zip: "",
  country: "",
}
