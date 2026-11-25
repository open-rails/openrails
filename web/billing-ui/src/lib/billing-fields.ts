// The merchant's billing fields, apart from the forms' country list so the
// headless entry can read them.

/** Whether a card form asks for a billing detail, and requires it. */
export type BillingFieldMode = "required" | "optional" | "hidden"

/**
 * The merchant's billing fields (`GET /me/config`'s
 * `payment.billing_fields`): `auto` asks for the postal code, and requires
 * it, only in the US.
 */
export interface BillingFields {
  country: BillingFieldMode
  postal_code: BillingFieldMode | "auto"
}

/** Stripe's: the country always, the postal code in the US alone. */
export const DEFAULT_BILLING_FIELDS: BillingFields = {
  country: "required",
  postal_code: "auto",
}

/** How the form asks for the postal code with this billing country. */
export function postalMode(
  fields: BillingFields,
  country: string
): BillingFieldMode {
  if (fields.postal_code !== "auto") return fields.postal_code
  return country.trim().toUpperCase() === "US" ? "required" : "hidden"
}
