import type { createEndpoints } from "./endpoints"

import type { InvoiceProfile } from "#orck/client/generated/wire"
export function createInvoiceProfileEndpoints(
  endpoints: ReturnType<typeof createEndpoints>
) {
  const { getCustomer } = endpoints
  const { updateCustomer } = endpoints

  // getInvoiceProfile is the customer's invoice_profile setting; null while it
  // has none.
  const getInvoiceProfile = async (customerId: string, signal?: AbortSignal) =>
    (await getCustomer(customerId, signal)).settings.invoice_profile ?? null

  const putInvoiceProfile = async (
    customerId: string,
    profile: InvoiceProfile
  ) => {
    const { settings } = await updateCustomer(customerId, {
      invoice_profile: profile,
    })
    return settings.invoice_profile
  }
  return { getInvoiceProfile, putInvoiceProfile }
}
