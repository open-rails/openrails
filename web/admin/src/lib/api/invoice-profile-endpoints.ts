import { getCustomer, updateCustomer } from "./endpoints"
import type { InvoiceProfile } from "./generated/wire"

// getInvoiceProfile is the customer's invoice_profile setting; null while it
// has none.
export const getInvoiceProfile = async (
  customerId: string,
  signal?: AbortSignal
) => (await getCustomer(customerId, signal)).settings.invoice_profile ?? null
export const putInvoiceProfile = async (
  customerId: string,
  profile: InvoiceProfile
) => {
  const { settings } = await updateCustomer(customerId, {
    invoice_profile: profile,
  })
  return settings.invoice_profile
}
