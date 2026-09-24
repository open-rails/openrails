// CheckoutSource is the flow's only data dependency: how to read the session
// and how to pay it. The HTTP source targets the #45 session API with the
// session id as the sole credential; tests and previews inject fixtures
// through the same interface.
import {
  checkoutSessionSchema,
  payResultSchema,
  type CheckoutSession,
  type PayRequest,
  type PayResult,
} from "./types"

export interface CheckoutSource {
  getSession(): Promise<CheckoutSession>
  pay(request: PayRequest): Promise<PayResult>
}

export class CheckoutSourceError extends Error {
  readonly status?: number
  constructor(message: string, status?: number) {
    super(message)
    this.name = "CheckoutSourceError"
    this.status = status
  }
}

async function parseJSON(response: Response): Promise<unknown> {
  try {
    return await response.json()
  } catch {
    throw new CheckoutSourceError("invalid response", response.status)
  }
}

function parseSession(response: Response, value: unknown): CheckoutSession {
  const parsed = checkoutSessionSchema.safeParse(value)
  if (!parsed.success) {
    throw new CheckoutSourceError("session unavailable", response.status)
  }
  return parsed.data
}

function parsePayResult(response: Response, value: unknown): PayResult {
  const parsed = payResultSchema.safeParse(value)
  if (!parsed.success) {
    throw new CheckoutSourceError("Payment failed. Try again.", response.status)
  }
  return parsed.data
}

export function createHttpSource(options: {
  baseUrl: string
  sessionId: string
  fetch?: typeof fetch
}): CheckoutSource {
  const doFetch = options.fetch ?? fetch
  const base = options.baseUrl.replace(/\/$/, "")
  const sessionURL = `${base}/api/v1/checkout/sessions/${encodeURIComponent(options.sessionId)}`
  return {
    async getSession() {
      const response = await doFetch(sessionURL, {
        headers: { Accept: "application/json" },
      })
      if (!response.ok) {
        throw new CheckoutSourceError("session unavailable", response.status)
      }
      return parseSession(response, await parseJSON(response))
    },
    async pay(request: PayRequest) {
      const response = await doFetch(`${sessionURL}/pay`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json",
        },
        body: JSON.stringify(request),
      })
      if (!response.ok) {
        let message = "Payment failed. Try again."
        try {
          const body = (await response.json()) as { error?: unknown }
          if (typeof body.error === "string" && body.error) {
            message = body.error
          }
        } catch {
          // keep the generic message
        }
        throw new CheckoutSourceError(message, response.status)
      }
      return parsePayResult(response, await parseJSON(response))
    },
  }
}
