// The page's API is this origin's /v1, under the pay URL secret alone: no
// cookies, no referrer.
export type PaymentPageFetch = (input: string, init?: RequestInit) => Promise<Response>

export function paymentPageFetch(secret: string): PaymentPageFetch {
  return (input, init = {}) => {
    const headers = new Headers(init.headers)
    headers.set("Authorization", `Bearer ${secret}`)
    return fetch(input, { ...init, headers, credentials: "omit", referrerPolicy: "no-referrer" })
  }
}
