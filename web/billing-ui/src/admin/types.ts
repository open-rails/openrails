import type { CSSProperties, ReactNode } from "react"
import type { QueryClient } from "@tanstack/react-query"
import type { BillingAppearance } from "../appearance"
import type { Customer } from "../client/generated/wire"

/** The host's authenticated transport. Tokens remain with the host. */
export interface BillingAdminAuth {
  authFetch(input: string, init?: RequestInit): Promise<Response>
  resourceFetch?(
    input: string,
    init: RequestInit & BillingAdminResource
  ): Promise<Response>
}

export interface BillingAdminResource {
  resource: string
  scope?: string
  group: string
}

/** The host router's current location and navigation operation. */
export interface BillingAdminNavigation {
  pathname: string
  search: string
  navigate(to: string, options?: { replace?: boolean }): void
}

export interface BillingAdminMerchant {
  id: string
  slug: string
  display_name?: string
  role?: string
}

export interface BillingAdminSettingsTab {
  value: string
  title: string
  element: ReactNode
}

export interface BillingAdminProps {
  /** The engine's API prefix, including /v1. */
  baseUrl: string
  auth: BillingAdminAuth
  basePath?: string
  navigation?: BillingAdminNavigation
  merchant?: string | BillingAdminMerchant
  merchants?: BillingAdminMerchant[]
  onMerchantChange?: (merchant: BillingAdminMerchant) => void
  /** The standalone server's resource; each selected merchant supplies group. */
  exchange?: Omit<BillingAdminResource, "group">
  settingsTabs?: BillingAdminSettingsTab[]
  customerPanels?: (customer: Customer) => ReactNode
  userHref?: (id: string) => string
  newMerchantHref?: string
  emptyState?: ReactNode
  appearance?: BillingAppearance
  className?: string
  style?: CSSProperties
  queryClient?: QueryClient
  language?: string
  messages?: Record<string, string>
}
