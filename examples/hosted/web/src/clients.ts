// The two browser clients, the only part of the React app that differs
// between the embedded, standalone and hosted examples.
import { createAuthClient } from "@openrails/auth-ui/client"
import { createBillingClient } from "@openrails/billing-ui"

// AuthKit's browser client, at /api/v1: authFetch attaches the signed-in
// user's token. It also trades that session, as the courses-web OAuth
// client, for DPoP-bound access tokens to the platform.
export const auth = createAuthClient({ resourceTokens: { clientId: "courses-web" } })

// The merchant's API host on the platform, which the app's server names
// (/api/billing), and the platform's resource identifier, its tokens' aud.
const server: { base_url: string; resource: string } = await fetch("/api/billing").then((res) => res.json())

// The browser calls the platform directly. Signed in, each call carries an
// openrails:self token for the platform and a DPoP proof; signed out, the
// public catalog needs neither.
export const billing = createBillingClient({
  baseUrl: server.base_url,
  fetch: (input, init) =>
    auth.getAccessToken()
      ? auth.resourceFetch(input, { ...init, resource: server.resource, scope: "openrails:self" })
      : fetch(input, init),
})
