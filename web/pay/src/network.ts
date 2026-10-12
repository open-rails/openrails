// "Save with openrails.dev": the shopper signs in to the customer network at
// the server's issuer with the page's own client (OpenID Connect code flow,
// PKCE S256, DPoP, prompt=login, the link challenge's nonce), and the
// pay URL links the order's customer to that account
// (docs/customer-network.md). Top level, the page goes to the issuer and
// back to /pay/sign-in; framed, the sign-in runs in a popup whose return hands
// the code to the frame that opened it.
import { dpopFetch, loadDPoPKey } from "@openrails/auth-ui/client"
import type { BillingClient } from "@openrails/billing-ui"
import type { CustomerLink, PublicConfig } from "@openrails/billing-ui/client"

export type NetworkSignIn = NonNullable<PublicConfig["network"]>

// The sign-in return's message to the frame that opened its popup.
export const SIGN_IN_MESSAGE = "openrails.payment.sign-in"
export const SIGN_IN_PATH = "/pay/sign-in"
const POPUP_STATE = ".popup"

/** Whether a sign-in's state is a popup's. */
export const popupState = (state: string) => state.endsWith(POPUP_STATE)

type Pending = {
  order: string
  challenge: string
  verifier: string
  issuer: string
  clientId: string
  tokenEndpoint: string
}

type Metadata = { issuer: string; authorization_endpoint: string; token_endpoint: string }

const pendingKey = (state: string) => `openrails.network.${state}`
const linkedKey = (order: string) => `openrails.network.linked.${order}`
const redirectUri = () => `${window.location.origin}${SIGN_IN_PATH}`

const b64url = (bytes: Uint8Array) =>
  btoa(String.fromCharCode(...bytes))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "")

const random = (n: number) => b64url(crypto.getRandomValues(new Uint8Array(n)))

async function metadata(issuer: string): Promise<Metadata> {
  const base = issuer.replace(/\/+$/, "")
  const res = await fetch(`${base}/.well-known/openid-configuration`, { credentials: "omit" })
  if (!res.ok) throw new Error(`The sign-in is unavailable (${res.status}).`)
  const m = (await res.json()) as Metadata
  if (m.issuer.replace(/\/+$/, "") !== base) throw new Error("The sign-in is unavailable.")
  return m
}

/** Whether the order's customer was linked in this tab. */
export function linkedHere(order: string): boolean {
  try {
    return window.sessionStorage.getItem(linkedKey(order)) === "1"
  } catch {
    return false
  }
}

/**
 * The authorization URL that signs the shopper in for order, with its
 * pending state kept for the return. The link challenge comes from the
 * order's pay URL.
 */
export async function beginSignIn(
  network: NetworkSignIn,
  client: BillingClient,
  order: string,
  popup: boolean
): Promise<string> {
  const [challenge, meta] = await Promise.all([client.createNetworkLinkChallenge(), metadata(network.issuer)])
  const verifier = random(32)
  const codeChallenge = b64url(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier))))
  // A popup's state says so: its return hands the code to the frame.
  const state = `${order}.${random(16)}${popup ? POPUP_STATE : ""}`
  const pending: Pending = {
    order,
    challenge: challenge.id,
    verifier,
    issuer: network.issuer,
    clientId: network.client_id,
    tokenEndpoint: meta.token_endpoint,
  }
  window.sessionStorage.setItem(pendingKey(state), JSON.stringify(pending))
  const url = new URL(meta.authorization_endpoint)
  url.search = new URLSearchParams({
    response_type: "code",
    client_id: network.client_id,
    redirect_uri: redirectUri(),
    scope: "openid",
    state,
    nonce: challenge.nonce,
    code_challenge: codeChallenge,
    code_challenge_method: "S256",
    prompt: "login",
  }).toString()
  return url.toString()
}

/** The order a sign-in's state belongs to, while it is pending here. */
export function pendingOrder(state: string): string | null {
  try {
    const raw = window.sessionStorage.getItem(pendingKey(state))
    return raw ? (JSON.parse(raw) as Pending).order : null
  } catch {
    return null
  }
}

/**
 * Redeems the sign-in's code for its ID token and links the order's
 * customer to the account with it.
 */
export async function finishSignIn(state: string, code: string, client: BillingClient): Promise<CustomerLink> {
  const raw = window.sessionStorage.getItem(pendingKey(state))
  if (!raw) throw new Error("This sign-in was not started here, or already finished.")
  window.sessionStorage.removeItem(pendingKey(state))
  const pending = JSON.parse(raw) as Pending
  const key = await loadDPoPKey(`openrails-payment-page:${pending.issuer}`)
  const res = await dpopFetch(fetch, key, new Map(), pending.tokenEndpoint, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      code,
      redirect_uri: redirectUri(),
      client_id: pending.clientId,
      code_verifier: pending.verifier,
    }).toString(),
    credentials: "omit",
  })
  const tokens = (await res.json().catch(() => ({}))) as { id_token?: string; error_description?: string }
  if (!res.ok || !tokens.id_token) throw new Error(tokens.error_description || "The sign-in did not finish.")
  const link = await client.createNetworkLink({ challengeId: pending.challenge, idToken: tokens.id_token })
  try {
    window.sessionStorage.setItem(linkedKey(pending.order), "1")
  } catch {
    // The link stands; only this tab's reminder of it is lost.
  }
  return link
}

/**
 * The sign-in in a popup, for a framed page: its return hands the code back
 * here. popup is opened by the click itself (about:blank), so no blocker
 * stops it.
 */
export function signInInPopup(popup: Window, url: string): Promise<{ state: string; code: string }> {
  popup.location.href = url
  return new Promise((resolve, reject) => {
    const timer = window.setInterval(() => {
      if (popup.closed) {
        cleanup()
        reject(new Error("The sign-in window was closed."))
      }
    }, 500)
    const heard = (event: MessageEvent) => {
      if (event.origin !== window.location.origin || event.source !== popup) return
      const data = event.data as { type?: string; state?: string; code?: string; error?: string }
      if (data?.type !== SIGN_IN_MESSAGE) return
      cleanup()
      if (data.code && data.state) resolve({ state: data.state, code: data.code })
      else reject(new Error(data.error === "access_denied" ? "The sign-in was canceled." : "The sign-in did not finish."))
    }
    const cleanup = () => {
      window.clearInterval(timer)
      window.removeEventListener("message", heard)
    }
    window.addEventListener("message", heard)
  })
}
