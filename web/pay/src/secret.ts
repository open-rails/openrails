// A pay URL carries its secret in the fragment, which a browser never
// sends to a server, and after it the checkout's fallbacks. The page reads
// them once, keeps them for this tab (reloads, and returns from 3-D Secure or
// a provider's page) and drops them from the address bar and history.
import { readPaymentFragment, type PaymentFallbacks } from "@openrails/billing-ui"

const storageKey = (order: string) => `openrails.payment.${order}`

export function orderIdFromPath(path = window.location.pathname): string | null {
  const match = /^\/pay\/(ord_[0-9a-f-]{36})\/?$/.exec(path)
  return match ? match[1] : null
}

export function takeFragment(order: string): { secret: string | null; fallbacks?: PaymentFallbacks } {
  const fromHash = readPaymentFragment(window.location.hash)
  if (fromHash.secret) {
    try {
      window.sessionStorage.setItem(storageKey(order), window.location.hash)
    } catch {
      // Storage refused: this load still has the secret.
    }
    window.history.replaceState(null, "", window.location.pathname + window.location.search)
    return fromHash
  }
  try {
    return readPaymentFragment(window.sessionStorage.getItem(storageKey(order)) ?? "")
  } catch {
    return { secret: null }
  }
}
