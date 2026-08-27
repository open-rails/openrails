// CCBill credit-card identity. The customer's IP remains server-owned; street,
// city, and state are configurable FlexForms fields and are not collected here.
import { z } from "zod"

import { countryCodeSchema, nameOnCardSchema } from "#orck/lib/billing"

export const ccbillBillingSchema = z.object({
  email: z.string().email("Enter a valid email"),
  name_on_card: nameOnCardSchema,
  zip: z.string().trim().min(1, "ZIP is required"),
  country: countryCodeSchema,
})

export type CCBillBilling = z.infer<typeof ccbillBillingSchema>

export const emptyCCBillBilling: CCBillBilling = {
  email: "",
  name_on_card: "",
  zip: "",
  country: "",
}
