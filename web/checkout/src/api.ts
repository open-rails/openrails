// The page's API is this origin's /v1, under the checkout secret alone: no
// cookies, no referrer.
export class CheckoutError extends Error {
  constructor(
    readonly status: number,
    readonly code: string
  ) {
    super(code || `HTTP ${status}`)
  }
}

export type CheckoutFetch = (input: string, init?: RequestInit) => Promise<Response>

export function checkoutFetch(secret: string): CheckoutFetch {
  return (input, init = {}) => {
    const headers = new Headers(init.headers)
    headers.set("Authorization", `Bearer ${secret}`)
    return fetch(input, { ...init, headers, credentials: "omit", referrerPolicy: "no-referrer" })
  }
}

export async function getJSON<T>(send: CheckoutFetch, path: string, signal?: AbortSignal): Promise<T> {
  const res = await send(`/v1${path}`, { headers: { Accept: "application/json" }, signal })
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: { code?: string } } | null
    throw new CheckoutError(res.status, body?.error?.code ?? "")
  }
  return (await res.json()) as T
}

// The order fields the page reads; billing-ui's wire types replace these
// once orders and their checkout are in its generated types.
export interface OrderLine {
  id: string
  description: string
  quantity: number | null
  amount: string
}

export interface Order {
  id: string
  status: string
  currency: string
  total: string
  lines: OrderLine[]
  checkout: { success_url: string; cancel_url: string | null; saved_payment_methods: boolean } | null
}
