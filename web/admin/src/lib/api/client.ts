// Thin fetch wrapper for the OpenRails admin API. Base URLs come from the
// mount's config.json (served by the Go binary): standalone defaults are
// auth=/auth/v1, api=/v1; embedded hosts point at their own bases. The
// session is auth-ui's: it holds the bearer, refreshes it, and steps up.
import {
  AuthKitError,
  isAuthKitError,
  isOAuthError,
  readAuthKitError,
  type ResourceRequest,
} from "@openrails/auth-ui/client"
import type { Guard } from "@openrails/auth-ui/react"

import type { ConsoleMerchant } from "@/extensions/types"

import type { OpenRailsErrorCode } from "./generated/error-codes"
import type { ListPage } from "./generated/wire"

export interface BootstrapConfig {
  auth_base_url: string
  api_base_url: string
  // #756 metrics Q&A gate (llm.ask_enabled AND an LLM key): false renders the
  // Ask panel as a pointed empty-state (the ask endpoint is not mounted).
  ask_enabled: boolean
  // #779 catalog copilot Q&A gate (llm.catalog_copilot_enabled AND an LLM
  // key): false renders the catalog copilot panel as a pointed empty-state.
  catalog_copilot_enabled: boolean
  // #779 Phase 2 gate (llm.catalog_drafting_enabled): false hides the
  // drafting affordances and leaves the copilot panel in Q&A-only mode.
  catalog_drafting_enabled: boolean
  // Host data for the console's extensions, by extension id
  // (AdminConsole.Extensions); empty on a standalone deployment.
  extensions?: Record<string, unknown>
  // The trusted issuer staff sign in at (AdminConsole.Issuer); null signs in
  // to auth_base_url's accounts.
  issuer: IssuerBootstrap | null
  // The merchant the mount serves (an embedded host's); null when staff pick
  // one.
  merchant?: ConsoleMerchant | null
  // Where the session's token is traded for one bound to the merchant staff
  // choose (RFC 8693, the merchant's id as group_id); null where the
  // session's token is bound already.
  exchange?: ExchangeBootstrap | null
}

export interface ExchangeBootstrap {
  token_endpoint: string
  client_id: string
  resource: string
  scope: string
}

export interface IssuerBootstrap {
  url: string
  client_id: string
  name: string
  resource: string
  scope: string
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

// mountMerchant is the merchant the console's mount serves, if it serves one.
export function mountMerchant(): ConsoleMerchant | undefined {
  return bootstrapConfig?.merchant ?? undefined
}

// The merchant the console acts on, in memory for the tab: its key (the
// slug staff see) and its id, which a token is exchanged for. Nothing names
// the merchant in a request; the token is bound to it.
let chosen: { key: string; id?: string } | undefined
const listeners = new Set<() => void>()

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export const isMerchantID = (s: string) => UUID.test(s)

export function selectedMerchant(): string | undefined {
  return chosen?.key
}

export function selectedMerchantID(): string | undefined {
  return chosen?.id
}

// setSelectedMerchant chooses the merchant requests act on; an id names the
// merchant a token is exchanged for (a key that is an id is one).
export function setSelectedMerchant(key: string | undefined, id?: string) {
  const next = key
    ? { key, id: id || (isMerchantID(key) ? key : undefined) }
    : undefined
  if (next?.key === chosen?.key && next?.id === chosen?.id) return
  chosen = next
  listeners.forEach((l) => l())
}

export function subscribeMerchant(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

// takeMerchantFromHash consumes #merchant=<id>, the link a host opens the
// console with (e.g. right after creating that merchant), and drops it from
// the address bar: a request to act on that merchant, which the token
// exchange grants or refuses.
export function takeMerchantFromHash(
  location: Location = window.location,
  history: History = window.history
): string | undefined {
  const params = new URLSearchParams(location.hash.slice(1))
  const slug = params.get("merchant")?.trim()
  if (!params.has("merchant")) return undefined
  params.delete("merchant")
  const rest = params.toString()
  history.replaceState(
    history.state,
    "",
    `${location.pathname}${location.search}${rest ? `#${rest}` : ""}`
  )
  return slug || undefined
}

// ConsoleTransport is what requests need of a session: auth-ui's AuthKit
// client or its issuer client. resourceFetch sends a token exchanged for the
// merchant chosen (auth-ui keeps it in memory and exchanges again before it
// expires); getResourceToken asks for one.
export interface ConsoleTransport {
  authFetch: (input: string, init?: RequestInit) => Promise<Response>
  signOut: () => Promise<void>
  resourceFetch?: (
    input: string,
    init: RequestInit & ResourceRequest
  ) => Promise<Response>
  getResourceToken?: (r: ResourceRequest) => Promise<unknown>
}

// merchantRefused is the answer for a merchant the person holds no role at:
// the exchange is refused (invalid_target).
const merchantRefused = () =>
  new ApiError(
    403,
    {
      error: {
        code: "merchant_unresolved",
        message: "You hold no role at this merchant.",
      },
    },
    "merchant refused"
  )

// exchangeFor is the exchange request for merchant id, when the mount
// exchanges.
function exchangeFor(id: string | undefined): ResourceRequest | undefined {
  const exchange = bootstrapConfig?.exchange
  if (!exchange) return undefined
  if (!id) throw merchantRefused()
  return { resource: exchange.resource, scope: exchange.scope, group: id }
}

// canActAt reports whether the session can get a token bound to merchant id:
// false when the exchange refuses it. Mounts that do not exchange can.
export async function canActAt(id: string): Promise<boolean> {
  const request = exchangeFor(id)
  if (!request || !session?.getResourceToken) return true
  try {
    await session.getResourceToken(request)
    return true
  } catch (error) {
    if (isOAuthError(error) && error.error === "invalid_target") return false
    throw error
  }
}

const unguarded: Guard = (action) => action()
let session: ConsoleTransport | null = null
let stepUp: Guard = unguarded

// bindSession makes auth-ui's client the bearer of every request.
export function bindSession(client: ConsoleTransport) {
  session = client
}

// bindStepUp routes writes through auth-ui's step-up dialog: a 401
// step_up_required (RFC 9470) re-authenticates and the write is retried.
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
    return this.code === "step_up_required"
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
  const url = buildURL(getBootstrap().api_base_url, path, opts.query)
  const init: RequestInit = {
    method: opts.method ?? "GET",
    headers,
    body:
      opts.rawBody ??
      (opts.body !== undefined ? JSON.stringify(opts.body) : undefined),
    signal: opts.signal,
  }
  const exchange = exchangeFor(selectedMerchantID())
  let res: Response
  if (exchange && session.resourceFetch) {
    try {
      res = await session.resourceFetch(url, { ...init, ...exchange })
    } catch (error) {
      if (isOAuthError(error) && error.error === "invalid_target")
        throw merchantRefused()
      throw error
    }
  } else {
    res = await session.authFetch(url, init)
  }
  // auth-ui already refreshed a stale bearer; this one is refused for good,
  // unless it asks for a fresher sign-in, which the step-up dialog answers.
  if (res.status === 401) {
    const error = await parseError(res)
    if (!error.stepUpRequired) void session.signOut()
    throw error
  }
  if (!res.ok) throw await parseError(res)
  const body = res.status === 204 ? undefined : await res.json()
  return { status: res.status, body: body as T }
}

// api calls the admin API (api_base_url-relative path, e.g.
// "/admin/payments"). A write OpenRails refuses for a stale sign-in opens
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
    // Canceled, or no dialog to answer it: the console sees OpenRails' 401.
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

// The cursor list envelope: next_cursor is null on the last page.
export interface CursorEnvelope<T> {
  data: T[]
  next_cursor: string | null
}

// authKit calls the AuthKit JSON API staff sign in through (auth_base_url):
// the group routes of the scope their permissions are held in. It throws
// AuthKitError; a write refused for a stale sign-in opens the step-up dialog
// and runs again.
export async function authKit<T>(
  path: string,
  opts: Pick<RequestOptions, "method" | "body" | "query" | "signal"> = {}
): Promise<T> {
  const method = (opts.method ?? "GET").toUpperCase()
  const run = async () => {
    if (!session) throw new Error("console session not bound")
    const res = await session.authFetch(
      buildURL(getBootstrap().auth_base_url, path, opts.query),
      {
        method,
        headers:
          opts.body === undefined
            ? undefined
            : { "Content-Type": "application/json" },
        body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
        signal: opts.signal,
      }
    )
    if (!res.ok) throw await readAuthKitError(res)
    return (res.status === 204 ? undefined : await res.json()) as T
  }
  return method === "GET" ? run() : stepUp(run)
}
