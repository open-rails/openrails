import { useAdminContext } from "../context"

// BillingAdmin uses its host's session and merchant directory.
export function useAuth() {
  const { merchants, activeMerchant, selectMerchant } = useAdminContext()
  return { merchants, activeMerchant, selectMerchant }
}

export function useSelectedMerchant() {
  return useAdminContext().activeMerchant?.slug
}
