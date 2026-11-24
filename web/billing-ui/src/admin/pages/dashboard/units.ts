// The merchant's own units, as the dashboard formats their amounts: at
// each unit's scale with its name. Staff who cannot read the merchant's
// configuration see a unit's code instead.
import * as React from "react"
import { useQuery } from "@tanstack/react-query"

import { useAdminServices } from "#orck/admin/context"
import { useMerchantConfig } from "#orck/admin/lib/capabilities"
import { merchantUnits, type MerchantUnits } from "#orck/admin/lib/format"

export const MerchantUnitsContext = React.createContext<MerchantUnits>({})

// useDeclaredUnits reads the units the merchant declares.
export function useDeclaredUnits(): MerchantUnits {
  const { adminQueries } = useAdminServices()
  const readable = useMerchantConfig()
  const { data } = useQuery({
    ...adminQueries.merchantConfiguration(),
    enabled: readable,
    retry: false,
  })
  return React.useMemo(() => merchantUnits(data?.currencies), [data])
}

// useUnits are the units of the widget being drawn.
export const useUnits = () => React.useContext(MerchantUnitsContext)
