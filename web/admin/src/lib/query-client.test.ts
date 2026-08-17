import { afterEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/lib/api/client"
import { adminQueries, collectUsageMeterPages, queryKeys } from "@/lib/queries"
import { queryClient, shouldRetry } from "@/lib/query-client"
import { toastApiError } from "@/lib/toast"

vi.mock("@/lib/toast", () => ({ toastApiError: vi.fn() }))

const storage = (values: Record<string, string> = {}): Storage => ({
  get length() {
    return Object.keys(values).length
  },
  clear() {
    for (const key of Object.keys(values)) delete values[key]
  },
  getItem(key) {
    return values[key] ?? null
  },
  key(index) {
    return Object.keys(values)[index] ?? null
  },
  removeItem(key) {
    delete values[key]
  },
  setItem(key, value) {
    values[key] = value
  },
})

afterEach(() => {
  queryClient.clear()
  vi.clearAllMocks()
  vi.unstubAllGlobals()
})

describe("query retries", () => {
  it("does not retry client errors", () => {
    const error = new ApiError(400, null, "bad request")

    expect(shouldRetry(0, error)).toBe(false)
  })

  it("retries transient failures at most twice", () => {
    const serverError = new ApiError(503, null, "unavailable")

    expect(shouldRetry(0, serverError)).toBe(true)
    expect(shouldRetry(1, serverError)).toBe(true)
    expect(shouldRetry(2, serverError)).toBe(false)
    expect(shouldRetry(0, new TypeError("network error"))).toBe(true)
  })
})

describe("query errors", () => {
  it("reports errors through query metadata", async () => {
    const error = new ApiError(503, null, "unavailable")

    await expect(
      queryClient.fetchQuery({
        queryKey: ["test", "error"],
        queryFn: () => Promise.reject(error),
        retry: false,
        meta: { errorAction: "Load test data" },
      })
    ).rejects.toBe(error)

    expect(toastApiError).toHaveBeenCalledWith(error, "Load test data")
  })

  it("keeps queries without error metadata silent", async () => {
    await expect(
      queryClient.fetchQuery({
        queryKey: ["test", "silent-error"],
        queryFn: () => Promise.reject(new Error("unavailable")),
        retry: false,
      })
    ).rejects.toThrow("unavailable")

    expect(toastApiError).not.toHaveBeenCalled()
  })
})

describe("query keys", () => {
  it("isolates cached server state by selected merchant", () => {
    const tokenKey = "openrails.admin.tokens"
    const sessions = storage({
      [tokenKey]: JSON.stringify({
        access_token: "token",
        merchant: "merchant-a",
      }),
    })
    vi.stubGlobal("localStorage", storage())
    vi.stubGlobal("sessionStorage", sessions)

    expect(queryKeys.customers()).toEqual([
      "merchant",
      "merchant-a",
      "customers",
    ])

    sessions.setItem(
      tokenKey,
      JSON.stringify({ access_token: "token", merchant: "merchant-b" })
    )

    expect(queryKeys.customers()).toEqual([
      "merchant",
      "merchant-b",
      "customers",
    ])
  })

  it("uses an explicit scope before a merchant is selected", () => {
    vi.stubGlobal("localStorage", storage())
    vi.stubGlobal("sessionStorage", storage())

    expect(queryKeys.dashboard()).toEqual([
      "merchant",
      "unselected",
      "dashboard",
    ])
  })

  it("scopes metering collections and detail independently", () => {
    vi.stubGlobal("localStorage", storage())
    vi.stubGlobal("sessionStorage", storage())

    expect(queryKeys.usageMeters()).toEqual([
      "merchant",
      "unselected",
      "catalog",
      "meters",
    ])
    expect(queryKeys.usageMeter("api-tokens")).toEqual([
      "merchant",
      "unselected",
      "catalog",
      "meters",
      "api-tokens",
    ])
    expect(queryKeys.customerUsageRates("customer-1")).toEqual([
      "merchant",
      "unselected",
      "customers",
      "customer-1",
      "usage-rates",
    ])
  })

  it("collects every meter page for customer rate selection", async () => {
    const firstMeter = { key: "first" } as never
    const lastMeter = { key: "last" } as never
    const loadPage = vi
      .fn()
      .mockResolvedValueOnce({
        items: [firstMeter],
        total: 2,
        limit: 1,
        offset: 0,
        configuration_source: "api",
        writes_allowed: true,
      })
      .mockResolvedValueOnce({
        items: [lastMeter],
        total: 2,
        limit: 1,
        offset: 1,
        configuration_source: "api",
        writes_allowed: true,
      })

    const result = await collectUsageMeterPages(loadPage)

    expect(loadPage).toHaveBeenNthCalledWith(1, 200, 0, undefined)
    expect(loadPage).toHaveBeenNthCalledWith(2, 200, 1, undefined)
    expect(result.items).toEqual([firstMeter, lastMeter])
  })

  it("only reports query errors when the usage requests it", () => {
    vi.stubGlobal("localStorage", storage())
    vi.stubGlobal("sessionStorage", storage())

    expect(adminQueries.merchantSettings().meta).toBeUndefined()
    expect(adminQueries.merchantSettings("Load settings").meta).toEqual({
      errorAction: "Load settings",
    })
  })
})
