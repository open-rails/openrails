// A checkout URL carries its secret in the fragment, which a browser never
// sends to a server. The page reads it once, keeps it for this tab (reloads,
// and returns from 3-D Secure or a provider's page) and drops it from the
// address bar and history.
const storageKey = (order: string) => `openrails.checkout.${order}`

export function orderIdFromPath(path = window.location.pathname): string | null {
  const match = /^\/c\/(ord_[0-9a-f-]{36})\/?$/.exec(path)
  return match ? match[1] : null
}

export function takeSecret(order: string): string | null {
  const fromHash = window.location.hash.replace(/^#/, "")
  if (fromHash.startsWith("cks_")) {
    try {
      window.sessionStorage.setItem(storageKey(order), fromHash)
    } catch {
      // Storage refused: this load still has the secret.
    }
    window.history.replaceState(null, "", window.location.pathname + window.location.search)
    return fromHash
  }
  try {
    return window.sessionStorage.getItem(storageKey(order))
  } catch {
    return null
  }
}
