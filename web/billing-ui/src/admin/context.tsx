import { createContext, useContext } from "react"
import type { BillingAdminMerchant, BillingAdminProps } from "./types"
import { createAdminClient, type AdminClientOptions } from "./lib/api/client"
import { createAdminAPI } from "./lib/api"
import { createAdminToast, createToastHandlers } from "./lib/toast"
import { createQueries } from "./lib/queries"
import { createMutations } from "./lib/mutations"
import { createCreditQueries } from "./lib/credit-queries"
import { createOrderQueries } from "./lib/order-queries"
import { createImportQueries } from "./lib/import-queries"

/** Every request and cache key is bound to this instance's selected merchant. */
export function createAdminServices(options: AdminClientOptions) {
  const client = createAdminClient(options)
  const api = createAdminAPI(client)
  const queries = createQueries(client, api)
  const toast = createAdminToast(options.namespace)
  return {
    toast,
    ...createToastHandlers(toast),
    ...client,
    ...api,
    ...queries,
    ...createMutations(client, api, queries),
    ...createCreditQueries(client, api),
    ...createOrderQueries(client, api, queries),
    ...createImportQueries(client, api),
  }
}

export interface AdminContextValue {
  props: BillingAdminProps
  services: ReturnType<typeof createAdminServices>
  merchants: BillingAdminMerchant[]
  activeMerchant?: BillingAdminMerchant
  selectMerchant(slug: string): void
  portal: HTMLElement | null
}

export const AdminContext = createContext<AdminContextValue | null>(null)

export function useAdminContext() {
  const value = useContext(AdminContext)
  if (!value) throw new Error("BillingAdmin context is missing")
  return value
}

export const useAdminServices = () => useAdminContext().services
