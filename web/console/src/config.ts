import type { BillingAdminMerchant } from "@openrails/billing-ui/admin"

export interface BootstrapConfig {
  auth_base_url: string
  api_base_url: string
  exchange?: {
    token_endpoint: string
    client_id: string
    resource: string
    scope: string
  } | null
}

export interface MerchantPage {
  data: BillingAdminMerchant[]
  next_cursor: string | null
  root: boolean
}
