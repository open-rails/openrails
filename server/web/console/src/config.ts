import type { BillingAdminMerchant } from "@openrails/billing-ui/admin"

export interface BootstrapConfig {
  auth_base_url: string
  api_base_url: string
  issuer: {
    url: string
    client_id: string
    name: string
    resource: string
    scope: string
  } | null
  merchant?: BillingAdminMerchant | null
  exchange?: {
    token_endpoint: string
    client_id: string
    resource: string
    scope: string
  } | null
}
