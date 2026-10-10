// The two browser clients, the only part of the React app that differs
// between the embedded, standalone and hosted examples.
import { createAuthClient } from "@openrails/auth-ui/client"
import { createBillingClient } from "@openrails/billing-ui"

// AuthKit's browser client, at /api/v1: authFetch attaches the signed-in user's token.
export const auth = createAuthClient()

// OpenRails is mounted in this server at /billing, behind the same AuthKit.
export const billing = createBillingClient({ baseUrl: "/billing/v1", fetch: auth.authFetch })
