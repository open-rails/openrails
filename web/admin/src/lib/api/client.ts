// Thin fetch wrapper for the OpenRails merchant API. Base URLs come from the
// mount's config.json (served by the Go binary): standalone defaults are
// auth=/auth/v1, api=/v1; embedded hosts point at their own bases. The
// session is auth-ui's: it holds the bearer, refreshes it, and steps up.
import {
  AuthKitError,
  isAuthKitError,
  type AuthClient,
} from "@openrails/auth-ui/client"
import type { Guard } from "@openrails/auth-ui/react"

import type { OpenRailsErrorCode } from "./generated/error-codes"
import type { ListPage } from "./generated/wire"

export interface BootstrapConfig {
  auth_base_url: string
  api_base_url: string
  // #741 fail-closed LLM gate: false hides the natural-language widget box
  // entirely (the generate endpoint is not mounted).
  nl_widgets_enabled: boolean
  // #756 metrics Q&A gate (llm.ask_enabled AND an LLM key): false renders the
  // Ask panel as a pointed empty-state (the ask endpoint is not mounted).
  ask_enabled: boolean
  // #779 catalog copilot Q&A gate (llm.catalog_copilot_enabled AND an LLM
  // key): false renders the catalog copilot panel as a pointed empty-state.
  catalog_copilot_enabled: boolean
  // #779 Phase 2 gate (llm.catalog_drafting_enabled): false hides the
  // drafting affordances and leaves the copilot panel in Q&A-only mode.
  catalog_drafting_enabled: boolean
}

let bootstrapConfig: BootstrapConfig | null = null

// loadBootstrap reads config.json from url, the mount's (lib/mount).
export async function loadBootstrap(url: string): Promise<BootstrapConfig> {
  if (bootstrapConfig) return bootstrapConfig
  const res = await fetch(url, {
    cache: "no-store",
  })
  if (!res.ok) {
    throw new Error(`admin console bootstrap failed: ${res.status}`)
  }
  bootstrapConfig = (await res.json()) as BootstrapConfig
  return bootstrapConfig
}

export function getBootstrap(): BootstrapConfig {
  if (!bootstrapConfig) throw new Error("bootstrap config not loaded")
  return bootstrapConfig
}

// The merchant every request is made as (OpenRails-Merchant), kept for the
// tab like the session.
const MERCHANT_KEY = "openrails.admin.merchant"

export function selectedMerchant(): string | undefined {
  return sessionStorage.getItem(MERCHANT_KEY) ?? undefined
}

export function setSelectedMerchant(slug: string | undefined) {
  if (slug) sessionStorage.setItem(MERCHANT_KEY, slug)
  else sessionStorage.removeItem(MERCHANT_KEY)
}

const unguarded: Guard = (action) => action()
let session: AuthClient | null = null
let stepUp: Guard = unguarded

// bindSession makes auth-ui's client the bearer of every request.
export function bindSession(client: AuthClient) {
  session = client
}

// bindStepUp routes writes through auth-ui's step-up dialog: a 403
// step_up_required re-authenticates and the write is retried.
export function bindStepUp(guard: Guard | null) {
  stepUp = guard ?? unguarded
}

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

  get stepUpRequired() {
    return this.status === 403 && this.code === "step_up_required"
  }

  get isPermissionDenied() {
    return this.status === 403 && !this.stepUpRequired
  }
}

async function parseError(res: Response): Promise<ApiError> {
  let body: ApiErrorBody | null = null
  try {
    body = (await res.json()) as ApiErrorBody
  } catch {
    // non-JSON error body
  }
  return new ApiError(res.status, body, `request failed (${res.status})`)
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

async function send<T>(
  path: string,
  opts: RequestOptions
): Promise<ApiResponse<T>> {
  if (!session) throw new Error("console session not bound")
  const headers: Record<string, string> = { ...opts.headers }
  if (opts.body !== undefined) headers["Content-Type"] = "application/json"
  const merchant = selectedMerchant()
  if (merchant) headers["OpenRails-Merchant"] = merchant
  const res = await session.authFetch(
    buildURL(getBootstrap().api_base_url, path, opts.query),
    {
      method: opts.method ?? "GET",
      headers,
      body:
        opts.rawBody ??
        (opts.body !== undefined ? JSON.stringify(opts.body) : undefined),
      signal: opts.signal,
    }
  )
  // auth-ui already refreshed a stale bearer; this one is refused for good.
  if (res.status === 401) {
    const error = await parseError(res)
    void session.signOut()
    throw error
  }
  if (!res.ok) throw await parseError(res)
  const body = res.status === 204 ? undefined : await res.json()
  return { status: res.status, body: body as T }
}

// api calls the merchant API (api_base_url-relative path, e.g.
// "/merchant/payments"). A write OpenRails refuses for a stale sign-in opens
// auth-ui's step-up dialog and runs again once the user has confirmed.
export const api = async <T>(path: string, opts: RequestOptions = {}) =>
  (await apiResponse<T>(path, opts)).body

// apiResponse is api with the success status, for routes whose 200 and 201
// mean different things.
export async function apiResponse<T>(
  path: string,
  opts: RequestOptions = {}
): Promise<ApiResponse<T>> {
  const method = (opts.method ?? "GET").toUpperCase()
  if (method === "GET" || method === "HEAD") return send<T>(path, opts)
  let refusal: ApiError | undefined
  try {
    return await stepUp(() =>
      send<T>(path, opts).catch((error: unknown) => {
        if (!(error instanceof ApiError) || !error.stepUpRequired) throw error
        refusal = error
        throw new AuthKitError(error.status, {
          type: error.type ?? "",
          code: "step_up_required",
          message: error.message,
          metadata: error.metadata,
        })
      })
    )
  } catch (error) {
    // Cancelled, or no dialog to answer it: the console sees OpenRails' 403.
    if (refusal && isAuthKitError(error)) throw refusal
    throw error
  }
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

// Offset envelope of the lists not yet on cursor pages.
export interface ListEnvelope<T> {
  object: "list"
  data: T[]
  total: number
  limit: number
  offset: number
  has_more: boolean
}

// Catalog/findings-style envelope ({items,...} instead of {data,...}).
export interface ItemsEnvelope<T> {
  items: T[]
  total: number
  limit: number
  offset: number
}
