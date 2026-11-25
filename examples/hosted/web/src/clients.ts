// The two browser clients, the only part of the React app that differs
// between the embedded, standalone and hosted examples.
import { createAuthClient } from "@openrails/auth-ui/client"
import { createBillingClient, tokenSource } from "@openrails/billing-ui"

// AuthKit's browser client, at /auth/v1: authFetch attaches the signed-in user's token.
export const auth = createAuthClient()

// The platform's API, which the app's server names.
const server: { base_url: string } = await fetch("/api/billing").then((res) => res.json())

// A customer token for the signed-in user, minted by the app's server and kept
// in memory. A new sign-in starts a new source, so no token outlives its user.
const customerTokens = () =>
  tokenSource({
    fetchToken: async () => {
      const res = await auth.authFetch("/api/billing/token", { method: "POST" })
      if (!res.ok) throw new Error(`customer token: ${res.status}`)
      return res.json()
    },
  })
let session: string | null = null
let tokens = customerTokens()

// The browser calls the platform directly with the customer token; signed
// out, the public routes need none.
export const billing = createBillingClient({
  baseUrl: server.base_url,
  getToken: () => {
    const current = auth.getAccessToken()
    if (!current) return null
    if (current !== session) {
      session = current
      tokens = customerTokens()
    }
    return tokens()
  },
})
