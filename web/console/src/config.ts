import type { BillingAdminMerchant } from "@openrails/billing-ui/admin"

export interface ConsoleIssuer {
  key: string
  url: string
  client_id: string
  name: string
  resource: string
  scope: string
}

export interface BootstrapConfig {
  auth_base_url: string
  api_base_url: string
  issuers: ConsoleIssuer[]
  app_sign_in: boolean
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
