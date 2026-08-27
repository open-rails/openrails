// CCBill billing identity rules — mirrors the engine's requireBillingFields.
// The canonical full name replaces the legacy UI split; OpenRails adapts it
// to provider-specific first/last fields at the CCBill boundary.
import { z } from "zod"

import { countryCodeSchema, nameOnCardSchema } from "#orck/lib/billing"

export const ccbillBillingSchema = z.object({
  email: z.string().email("Enter a valid email"),
  name_on_card: nameOnCardSchema,
  address1: z.string().trim().min(1, "Address is required"),
  city: z.string().trim().min(1, "City is required"),
  state: z.string().trim().optional(),
  zip: z.string().trim().min(1, "ZIP is required"),
  country: countryCodeSchema,
})

export type CCBillBilling = z.infer<typeof ccbillBillingSchema>

export const emptyCCBillBilling: CCBillBilling = {
  email: "",
  name_on_card: "",
  address1: "",
  city: "",
  state: "",
  zip: "",
  country: "",
}
