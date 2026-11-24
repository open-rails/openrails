// CCBill credit-card identity. Verified account email and customer IP remain
// server-owned; configurable street/city/state fields are not collected here.
import { z } from "zod"

import { countryCodeSchema, nameOnCardSchema } from "#orck/lib/billing"

export const ccbillBillingSchema = z.object({
  name_on_card: nameOnCardSchema,
  zip: z.string().trim().min(1, "ZIP is required"),
  country: countryCodeSchema,
})

export type CCBillBilling = z.infer<typeof ccbillBillingSchema>

export const emptyCCBillBilling: CCBillBilling = {
  name_on_card: "",
  zip: "",
  country: "",
}
