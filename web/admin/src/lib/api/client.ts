// Thin fetch wrapper for the OpenRails merchant API + AuthKit authhttp.
// Base URLs come from /admin/config.json (served by the Go binary): standalone
// defaults are auth=/auth, api=/v1; embedded hosts point at their own bases.

export interface BootstrapConfig {
  auth_base_url: string
  api_base_url: string
  // #741 fail-closed LLM gate: false hides the natural-language widget box
  // entirely (the generate endpoint would answer 501).
  nl_widgets_enabled: boolean
  // #756 metrics Q&A gate (llm.ask_enabled AND an LLM key): false renders the
  // Ask panel as a pointed empty-state (the ask endpoint would answer 501).
  ask_enabled: boolean
  // #779 catalog copilot Q&A gate (llm.catalog_copilot_enabled AND an LLM
  // key): false renders the catalog copilot panel as a pointed empty-state.
  catalog_copilot_enabled: boolean
  // #779 Phase 2 gate (llm.catalog_drafting_enabled): false hides the
  // drafting affordances — the copilot panel stays Q&A-only. Stays false
  // until #781 (server-side notice-window enforcement) ships.
  catalog_drafting_enabled: boolean
}

export interface TokenPair {
  access_token: string
  refresh_token?: string
  expires_at?: number // unix ms, derived from expires_in
  merchant?: string
}

const TOKEN_KEY = "openrails.admin.tokens"

let bootstrapConfig: BootstrapConfig | null = null

export async function loadBootstrap(): Promise<BootstrapConfig> {
  if (bootstrapConfig) return bootstrapConfig
  const res = await fetch(`${import.meta.env.BASE_URL}config.json`, {
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

export function getTokens(): TokenPair | null {
  localStorage.removeItem(TOKEN_KEY)
  const raw = sessionStorage.getItem(TOKEN_KEY)
  if (!raw) return null
  try {
    return JSON.parse(raw) as TokenPair
  } catch {
    return null
  }
}

export function setTokens(tokens: TokenPair | null) {
  localStorage.removeItem(TOKEN_KEY)
  if (tokens) sessionStorage.setItem(TOKEN_KEY, JSON.stringify(tokens))
  else sessionStorage.removeItem(TOKEN_KEY)
}

function sameStoredSession(
  left: TokenPair | null,
  right: TokenPair | null
): boolean {
  if (!left || !right) return left === right
  return (
    left.access_token === right.access_token &&
    left.refresh_token === right.refresh_token &&
    left.merchant === right.merchant
  )
}

function tokenSessionID(tokens: TokenPair | null): string | undefined {
  const payload = tokens?.access_token.split(".")[1]
  if (!payload) return undefined
  try {
    const base64 = payload.replaceAll("-", "+").replaceAll("_", "/")
    const decoded = JSON.parse(
      atob(base64.padEnd(Math.ceil(base64.length / 4) * 4, "="))
    ) as { sid?: unknown }
    return typeof decoded.sid === "string" ? decoded.sid : undefined
  } catch {
    return undefined
  }
}

export function sameTokenSession(
  left: TokenPair | null,
  right: TokenPair | null
): boolean {
  if (!left || !right) return left === right
  if (left.merchant !== right.merchant) return false
  if (left.access_token === right.access_token) return true
  const leftID = tokenSessionID(left)
  return !!leftID && leftID === tokenSessionID(right)
}

export function setTokensIfCurrent(
  tokens: TokenPair,
  expected: TokenPair | null
): boolean {
  if (!sameStoredSession(expected, getTokens())) return false
  setTokens(tokens)
  return true
}

export function clearTokensIfCurrent(expected: TokenPair): boolean {
  if (!sameStoredSession(expected, getTokens())) return false
  setTokens(null)
  return true
}

// Stripe-shaped error envelope (pkg/api).
export interface ApiErrorBody {
  error?: {
    type?: string
    code?: string
    message?: string
    param?: string
  }
}

export class ApiError extends Error {
  status: number
  type?: string
  code?: string
  param?: string

  constructor(status: number, body: ApiErrorBody | null, fallback: string) {
    super(body?.error?.message || fallback)
    this.status = status
    this.type = body?.error?.type
    this.code = body?.error?.code
    this.param = body?.error?.param
  }

  get isPermissionDenied() {
    return this.status === 403
  }
}

let onUnauthorized: (() => void) | null = null
export function setUnauthorizedHandler(fn: (() => void) | null) {
  onUnauthorized = fn
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

async function refreshTokens(tokens: TokenPair): Promise<TokenPair | null> {
  if (!tokens.refresh_token) return null
  const { auth_base_url } = getBootstrap()
  const res = await fetch(`${auth_base_url}/token`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      grant_type: "refresh_token",
      refresh_token: tokens.refresh_token,
    }),
  })
  if (!res.ok) return null
  const body = (await res.json()) as {
    access_token?: string
    refresh_token?: string
    expires_in?: number
  }
  if (!body.access_token) return null
  if (!sameStoredSession(tokens, getTokens())) return null
  const refreshed = {
    access_token: body.access_token,
    refresh_token: body.refresh_token ?? tokens.refresh_token,
    expires_at: body.expires_in
      ? Date.now() + body.expires_in * 1000
      : undefined,
    merchant: tokens.merchant,
  }
  setTokens(refreshed)
  return refreshed
}

export interface RequestOptions {
  method?: string
  body?: unknown
  headers?: Record<string, string>
  query?: Record<string, string | number | boolean | undefined>
  signal?: AbortSignal
}

interface FetchResult {
  response: Response
  tokens: TokenPair | null
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

async function doFetch(
  url: string,
  opts: RequestOptions,
  retry: boolean,
  selectMerchant: boolean,
  tokens: TokenPair | null = getTokens()
): Promise<FetchResult> {
  const headers: Record<string, string> = { ...opts.headers }
  if (opts.body !== undefined) headers["Content-Type"] = "application/json"
  if (tokens) headers["Authorization"] = `Bearer ${tokens.access_token}`
  if (selectMerchant && tokens?.merchant)
    headers["X-OpenRails-Merchant"] = tokens.merchant
  const res = await fetch(url, {
    method: opts.method ?? "GET",
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    signal: opts.signal,
  })
  if (res.status === 401 && retry && tokens?.refresh_token) {
    const refreshed = await refreshTokens(tokens)
    if (refreshed) return doFetch(url, opts, false, selectMerchant, refreshed)
  }
  return { response: res, tokens }
}

// api calls the merchant API (api_base_url-relative path, e.g. "/merchant/payments").
export async function api<T>(
  path: string,
  opts: RequestOptions = {}
): Promise<T> {
  const { api_base_url } = getBootstrap()
  const { response: res, tokens } = await doFetch(
    buildURL(api_base_url, path, opts.query),
    opts,
    true,
    true
  )
  if (res.status === 401) {
    if (sameStoredSession(tokens, getTokens())) {
      setTokens(null)
      onUnauthorized?.()
    }
    throw await parseError(res)
  }
  if (!res.ok) throw await parseError(res)
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

// authApi calls the AuthKit authhttp surface (auth_base_url-relative path).
export async function authApi<T>(
  path: string,
  opts: RequestOptions = {}
): Promise<T> {
  const { auth_base_url } = getBootstrap()
  const { response: res } = await doFetch(
    buildURL(auth_base_url, path, opts.query),
    opts,
    true,
    false
  )
  if (!res.ok) throw await parseError(res)
  return (await res.json()) as T
}

export async function logoutSession(tokens: TokenPair): Promise<void> {
  const { auth_base_url } = getBootstrap()
  const { response } = await doFetch(
    buildURL(auth_base_url, "/logout"),
    { method: "DELETE" },
    false,
    false,
    tokens
  )
  if (!response.ok) throw await parseError(response)
}

// Standard list envelope used by most /v1/merchant list endpoints.
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
