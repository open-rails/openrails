import type { BillingAdminAuth, BillingAdminResource } from "../../types"
import type { OpenRailsErrorCode } from "../../../client/generated/error-codes"
import type { ListPage } from "../../../client/generated/wire"
import { transportFailure } from "../../../client/errors"
import { en } from "../../../locales/en"

// Stripe-shaped error envelope (internal/api); codes are billing.ErrorCodes.
export interface ApiErrorBody {
  error?: {
    type?: string
    code?: OpenRailsErrorCode
    message?: string
    param?: string
    metadata?: Record<string, unknown>
  }
}

export class ApiError extends Error {
  status: number
  type?: string
  code?: OpenRailsErrorCode
  param?: string
  metadata?: Record<string, unknown>

  constructor(status: number, body: ApiErrorBody | null, fallback: string) {
    super(body?.error?.message || fallback)
    this.status = status
    this.type = body?.error?.type
    this.code = body?.error?.code
    this.param = body?.error?.param
    this.metadata = body?.error?.metadata
  }

  get isPermissionDenied() {
    return this.status === 403
  }

  // The object changed since the revision the request named.
  get revisionMismatch() {
    return this.status === 409 && this.code === "revision_mismatch"
  }

  // The merchant's configuration is read from a file: no edit applies.
  get merchantConfigReadOnly() {
    return this.status === 409 && this.code === "merchant_config_read_only"
  }
}

async function parseError(res: Response): Promise<ApiError> {
  let body: ApiErrorBody | null = null
  try {
    body = (await res.json()) as ApiErrorBody
  } catch {
    // non-JSON error body
  }
  const failure = body?.error?.code
    ? undefined
    : transportFailure({ status: res.status })
  return new ApiError(
    res.status,
    body,
    failure ? en.errors[failure] : `request failed (${res.status})`
  )
}

// A request OpenRails never answered (status 0), named for the operator.
function unanswered(error: unknown): ApiError | undefined {
  const failure = transportFailure(error)
  return failure && new ApiError(0, null, en.errors[failure])
}

export interface RequestOptions {
  method?: string
  body?: unknown
  // Preserve catalog JSON/YAML bytes and exact monetary literals.
  rawBody?: string
  headers?: Record<string, string>
  query?: Record<string, string | number | boolean | undefined>
  signal?: AbortSignal
}

function buildURL(base: string, path: string, query?: RequestOptions["query"]) {
  let url = base + path
  if (query) {
    const params = new URLSearchParams()
    for (const [k, v] of Object.entries(query)) {
      if (v !== undefined && v !== "") params.set(k, String(v))
    }
    const qs = params.toString()
    if (qs) url += `?${qs}`
  }
  return url
}

export interface ApiResponse<T> {
  status: number
  body: T
}

// One cursor page request: cursor is the previous page's next_cursor.
export interface PageRequest {
  limit?: number
  cursor?: string
}

// collectCursorPages walks a cursor list to its end.
export async function collectCursorPages<T>(
  loadPage: (cursor?: string) => Promise<ListPage<T>>,
  signal?: AbortSignal
): Promise<T[]> {
  const rows: T[] = []
  let cursor: string | undefined
  for (;;) {
    signal?.throwIfAborted()
    const page = await loadPage(cursor)
    rows.push(...page.data)
    if (!page.next_cursor) return rows
    if (page.next_cursor === cursor)
      throw new Error("List paging stopped before all records were loaded")
    cursor = page.next_cursor
  }
}

// The cursor list envelope: next_cursor is null on the last page.
export interface CursorEnvelope<T> {
  data: T[]
  next_cursor: string | null
}

export interface AdminClientOptions {
  namespace?: string
  isCurrent?: () => boolean
  baseUrl: string
  auth: BillingAdminAuth
  merchant?: { id: string; slug: string }
  exchange?: Omit<BillingAdminResource, "group">
}

/** One immutable transport per component and selected merchant. */
export function createAdminClient(options: AdminClientOptions) {
  const baseUrl = options.baseUrl.replace(/\/$/, "")
  const selectedMerchant = () =>
    options.isCurrent && !options.isCurrent()
      ? undefined
      : options.merchant?.slug
  async function send<T>(
    path: string,
    opts: RequestOptions
  ): Promise<ApiResponse<T>> {
    const headers: Record<string, string> = { ...opts.headers }
    if (opts.body !== undefined) headers["Content-Type"] = "application/json"
    const init: RequestInit = {
      method: opts.method ?? "GET",
      headers,
      signal: opts.signal,
      body:
        opts.rawBody ??
        (opts.body !== undefined ? JSON.stringify(opts.body) : undefined),
    }
    const url = buildURL(baseUrl, path, opts.query)
    let res: Response
    if (options.exchange) {
      if (!options.merchant?.id || !options.auth.resourceFetch)
        throw new Error(
          "Merchant token exchange needs a merchant id and resourceFetch transport"
        )
      try {
        res = await options.auth.resourceFetch(url, {
          ...init,
          ...options.exchange,
          group: options.merchant.id,
        })
      } catch (error) {
        if (
          error &&
          typeof error === "object" &&
          "error" in error &&
          error.error === "invalid_target"
        )
          throw new ApiError(
            403,
            {
              error: {
                code: "merchant_unresolved",
                message: "You hold no role at this merchant.",
              },
            },
            "Merchant refused"
          )
        throw (!opts.signal?.aborted && unanswered(error)) || error
      }
    } else {
      try {
        res = await options.auth.authFetch(url, init)
      } catch (error) {
        throw (!opts.signal?.aborted && unanswered(error)) || error
      }
    }
    if (!res.ok) throw await parseError(res)
    return {
      status: res.status,
      body: (res.status === 204 ? undefined : await res.json()) as T,
    }
  }
  async function apiResponse<T>(
    path: string,
    opts: RequestOptions = {}
  ): Promise<ApiResponse<T>> {
    return send<T>(path, opts)
  }
  const api = async <T>(path: string, opts: RequestOptions = {}) =>
    (await apiResponse<T>(path, opts)).body
  const queryRoot = (requested?: string) => {
    const merchant =
      requested !== undefined &&
      requested !== options.merchant?.slug &&
      requested !== options.merchant?.id
        ? requested
        : options.merchant?.id || options.merchant?.slug || "unselected"
    return [
      ...(options.namespace ? ["billing-admin", options.namespace] : []),
      "merchant",
      merchant,
    ] as const
  }
  return {
    namespace: options.namespace,
    api,
    apiResponse,
    selectedMerchant,
    queryRoot,
  }
}
export type AdminClient = ReturnType<typeof createAdminClient>
