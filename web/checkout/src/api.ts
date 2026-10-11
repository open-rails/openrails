// The page's API is this origin's /v1, under the checkout secret alone: no
// cookies, no referrer.
export type CheckoutFetch = (input: string, init?: RequestInit) => Promise<Response>

export function checkoutFetch(secret: string): CheckoutFetch {
  return (input, init = {}) => {
    const headers = new Headers(init.headers)
    headers.set("Authorization", `Bearer ${secret}`)
    return fetch(input, { ...init, headers, credentials: "omit", referrerPolicy: "no-referrer" })
  }
}
