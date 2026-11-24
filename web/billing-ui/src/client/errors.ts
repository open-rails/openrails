// Mirrors OpenRails internal/api.ErrorResponse:
// {"error":{"type","code","message","request_id?","param?","metadata?"}}.
export interface BillingErrorBody {
  type: string
  code: string
  message: string
  request_id?: string
  param?: string
  metadata?: Record<string, unknown>
  /** The order a failed payment left open (402 card_error). */
  order?: unknown
}

// Local codes: network_error (no server reached), server_unavailable (a
// gateway or timeout answered), server_error (a 5xx without an envelope),
// invalid_response, unknown_error, and the wallet codes a wallet next action
// raises (wallet_rejected, wallet_no_signature, wallet_required).
export class BillingError extends Error {
  readonly status: number
  readonly type: string
  readonly code: string
  readonly param?: string
  readonly requestId?: string
  readonly metadata: Record<string, unknown>
  /** The order a failed payment left open, unvalidated. */
  readonly order?: unknown

  constructor(status: number, body: BillingErrorBody) {
    super(body.message)
    this.name = "BillingError"
    this.status = status
    this.type = body.type
    this.code = body.code
    this.param = body.param
    this.requestId = body.request_id
    this.metadata = body.metadata ?? {}
    this.order = body.order
  }
}

export const isBillingError = (value: unknown): value is BillingError =>
  value instanceof BillingError

/** A server failure (HTTP 5xx) from any client or host source that carries a numeric `status`. */
export const isServerError = (value: unknown): boolean => {
  const status = (value as { status?: unknown } | null)?.status
  return typeof status === "number" && status >= 500 && status < 600
}

export function localError(code: string, message: string): BillingError {
  return new BillingError(0, { type: "local", code, message })
}

export async function readBillingError(res: Response): Promise<BillingError> {
  let body: unknown
  try {
    body = await res.json()
  } catch {
    body = undefined
  }
  const err = (body as { error?: unknown } | undefined)?.error
  if (err && typeof err === "object") {
    const e = err as Partial<BillingErrorBody>
    if (typeof e.code === "string" && e.code) {
      return new BillingError(res.status, {
        type: typeof e.type === "string" ? e.type : "",
        code: e.code,
        message: typeof e.message === "string" ? e.message : e.code,
        request_id:
          typeof e.request_id === "string"
            ? e.request_id
            : (res.headers.get("X-Request-ID") ?? undefined),
        param: typeof e.param === "string" ? e.param : undefined,
        metadata:
          e.metadata && typeof e.metadata === "object" ? e.metadata : undefined,
        order: e.order,
      })
    }
  }
  return new BillingError(res.status, {
    type: res.status >= 500 ? "api_error" : "",
    code:
      typeof err === "string" && err
        ? err
        : GATEWAY_STATUS.has(res.status)
          ? "server_unavailable"
          : res.status >= 500
            ? "server_error"
            : "unknown_error",
    message: `HTTP ${res.status}`,
  })
}

// A gateway answered for OpenRails: down, restarting or timed out (52x: Cloudflare).
const GATEWAY_STATUS = new Set([502, 503, 504, 520, 521, 522, 523, 524])

// fetch rejects with a TypeError when no server answers (offline, DNS,
// connection refused, CORS); engines word it differently.
const FETCH_FAILURE = /fetch|network|load failed|connection/i

/** The local code of a request OpenRails never answered, if it is one. */
export function transportFailure(
  error: unknown
): "network_error" | "server_unavailable" | undefined {
  if (error instanceof TypeError) {
    const offline = globalThis.navigator?.onLine === false
    return offline || FETCH_FAILURE.test(error.message)
      ? "network_error"
      : undefined
  }
  if (!error || typeof error !== "object") return undefined
  const { name, code, status } = error as {
    name?: unknown
    code?: unknown
    status?: unknown
  }
  if (name === "TimeoutError") return "server_unavailable"
  if (code === "network_error" || code === "server_unavailable") return code
  if (code === undefined || code === "unknown_error" || code === "server_error")
    return typeof status === "number" && GATEWAY_STATUS.has(status)
      ? "server_unavailable"
      : undefined
  return undefined
}

// Normalizes anything a client call can throw into a BillingError.
export function toBillingError(error: unknown): BillingError {
  if (error instanceof BillingError) return error
  const transport = transportFailure(error)
  if (transport)
    return localError(
      transport,
      error instanceof Error ? error.message : transport
    )
  return localError(
    "unknown_error",
    error instanceof Error ? error.message : String(error)
  )
}
