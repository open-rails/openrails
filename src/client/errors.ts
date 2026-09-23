// Mirrors OpenRails pkg/api.ErrorResponse:
// {"error":{"type","code","message","request_id?","param?","metadata?"}}.
export interface BillingErrorBody {
  type: string
  code: string
  message: string
  request_id?: string
  param?: string
  metadata?: Record<string, unknown>
}

// Local codes: network_error, invalid_response, unknown_error, and the wallet
// codes the Solana cancel flow raises.
export class BillingError extends Error {
  readonly status: number
  readonly type: string
  readonly code: string
  readonly param?: string
  readonly requestId?: string
  readonly metadata: Record<string, unknown>

  constructor(status: number, body: BillingErrorBody) {
    super(body.message)
    this.name = "BillingError"
    this.status = status
    this.type = body.type
    this.code = body.code
    this.param = body.param
    this.requestId = body.request_id
    this.metadata = body.metadata ?? {}
  }
}

export const isBillingError = (value: unknown): value is BillingError =>
  value instanceof BillingError

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
      })
    }
  }
  return new BillingError(res.status, {
    type: "",
    code: typeof err === "string" && err ? err : "unknown_error",
    message: `HTTP ${res.status}`,
  })
}

// Normalizes anything a client call can throw into a BillingError.
export function toBillingError(error: unknown): BillingError {
  if (error instanceof BillingError) return error
  if (error instanceof TypeError)
    return localError("network_error", error.message)
  return localError(
    "unknown_error",
    error instanceof Error ? error.message : String(error)
  )
}
