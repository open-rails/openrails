// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest"

import { createAuthClient } from "@openrails/auth-ui/client"

import {
  ApiError,
  createAdminClient,
  type RequestOptions,
} from "#orck/admin/lib/api/client"
import { accessToken } from "#orck/admin/test/harness"

let authClient: ReturnType<typeof createAuthClient>
let selected: ReturnType<typeof createAdminClient>
const api = (path: string, options?: RequestOptions) =>
  selected.api(path, options)
function setSelectedMerchant(slug: string, id: string) {
  selected = createAdminClient({
    baseUrl: "/v1",
    auth: authClient,
    merchant: { slug, id },
    exchange: { resource: EXCHANGE.resource, scope: EXCHANGE.scope },
  })
}

const A = "11111111-1111-4111-8111-111111111111"
const B = "22222222-2222-4222-8222-222222222222"
const C = "33333333-3333-4333-8333-333333333333"

const EXCHANGE = {
  token_endpoint: "/auth/oauth2/token",
  client_id: "openrails-console",
  resource: "https://billing.example",
  scope: "openrails:merchant",
}

interface Exchanged {
  group: string
  client: string
  resource: string
  scope: string
}

// The console's deployment: config.json names the exchange, the token
// endpoint grants a merchant the person holds a role at (A and B) and
// refuses another (C), and the admin API records the bearer it is sent.
function deployment(expiresIn = 900) {
  const exchanged: Exchanged[] = []
  const bearers: string[] = []
  const merchantHeaders: (string | null)[] = []
  let issued = 0
  const storage = () => {
    const values = new Map<string, string>()
    return {
      get length() {
        return values.size
      },
      clear: () => values.clear(),
      getItem: (key: string) => values.get(key) ?? null,
      key: (index: number) => [...values.keys()][index] ?? null,
      removeItem: (key: string) => void values.delete(key),
      setItem: (key: string, value: string) => void values.set(key, value),
      values,
    }
  }
  const local = storage()
  const session = storage()
  vi.stubGlobal("localStorage", local)
  vi.stubGlobal("sessionStorage", session)
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      if (url.endsWith("config.json"))
        return Response.json({
          api_base_url: "/v1",
          auth_base_url: "/auth/v1",
          extensions: {},
          issuer: null,
          exchange: EXCHANGE,
        })
      if (url === EXCHANGE.token_endpoint) {
        const form = new URLSearchParams(String(init.body))
        const group = form.get("group_id") ?? ""
        exchanged.push({
          group,
          client: form.get("client_id") ?? "",
          resource: form.get("resource") ?? "",
          scope: form.get("scope") ?? "",
        })
        if (group === C)
          return Response.json(
            {
              error: "invalid_target",
              error_description: "no role in that group",
            },
            { status: 400 }
          )
        issued++
        return Response.json({
          access_token: `merchant-token-${group}-${issued}`,
          token_type: "Bearer",
          expires_in: expiresIn,
          scope: EXCHANGE.scope,
        })
      }
      const headers = new Headers(init.headers)
      bearers.push(headers.get("Authorization") ?? "")
      merchantHeaders.push(headers.get("OpenRails" + "-Merchant"))
      return Response.json({ scope_id: "" })
    })
  )
  return { exchanged, bearers, merchantHeaders, local, session }
}

async function signedIn() {
  const client = createAuthClient({
    baseUrl: "/auth/v1",
    sessionHint: false,
    resourceTokens: {
      clientId: EXCHANGE.client_id,
      tokenEndpoint: EXCHANGE.token_endpoint,
    },
    storage: {
      get: () => sessionStorage.getItem("openrails.admin.refresh"),
      set: (token) =>
        token
          ? sessionStorage.setItem("openrails.admin.refresh", token)
          : sessionStorage.removeItem("openrails.admin.refresh"),
    },
  })
  await client.completeSignIn(async () => ({
    status: "complete",
    token_set: {
      access_token: accessToken("console-test"),
      token_type: "Bearer",
      expires_in: 3600,
      refresh_token: "refresh-token",
    },
  }))
  authClient = client
  return client
}

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe("the console's merchant switch", () => {
  it("acts on the chosen merchant through a token exchanged for it, naming none", async () => {
    const d = deployment()
    await signedIn()
    setSelectedMerchant("shop-a", A)
    await api("/admin/access")
    expect(d.exchanged).toEqual([
      {
        group: A,
        client: EXCHANGE.client_id,
        resource: EXCHANGE.resource,
        scope: EXCHANGE.scope,
      },
    ])
    expect(d.bearers.at(-1)).toBe(`Bearer merchant-token-${A}-1`)

    setSelectedMerchant("shop-b", B)
    await api("/admin/access")
    expect(d.exchanged.map((x) => x.group)).toEqual([A, B])
    expect(d.bearers.at(-1)).toBe(`Bearer merchant-token-${B}-2`)
    expect(d.merchantHeaders.every((h) => h === null)).toBe(true)

    // Back to A: its token, still fresh, is A's own, never B's.
    setSelectedMerchant("shop-a", A)
    await api("/admin/access")
    expect(d.bearers.at(-1)).toBe(`Bearer merchant-token-${A}-1`)
  })

  it("exchanges again before the merchant's token expires", async () => {
    const d = deployment(120)
    await signedIn()
    vi.useFakeTimers({ toFake: ["Date"] })
    vi.setSystemTime(new Date("2026-10-11T12:00:00Z"))
    setSelectedMerchant("shop-a", A)
    await api("/admin/access")
    await api("/admin/access")
    expect(d.exchanged).toHaveLength(1)
    vi.setSystemTime(new Date("2026-10-11T12:01:40Z")) // 20 s before it expires
    await api("/admin/access")
    expect(d.exchanged).toHaveLength(2)
    expect(d.bearers.at(-1)).toBe(`Bearer merchant-token-${A}-2`)
  })

  it("answers a merchant the person holds no role at as refused", async () => {
    const d = deployment()
    await signedIn()
    setSelectedMerchant(C, C)
    const refused = await api("/admin/access").catch((e: unknown) => e)
    expect(refused).toBeInstanceOf(ApiError)
    expect((refused as ApiError).code).toBe("merchant_unresolved")
    expect(d.bearers).toHaveLength(0)
  })

  it("keeps the merchant token in memory: storage holds the refresh token alone", async () => {
    const d = deployment()
    await signedIn()
    setSelectedMerchant("shop-a", A)
    await api("/admin/access")
    setSelectedMerchant("shop-b", B)
    await api("/admin/access")
    const stored = [
      ...d.session.values.values(),
      ...d.local.values.values(),
    ].join(" ")
    expect(stored).not.toContain("merchant-token")
    expect([...d.local.values.keys()]).toEqual([])
    expect([...d.session.values.keys()]).toEqual(["openrails.admin.refresh"])
  })
})
