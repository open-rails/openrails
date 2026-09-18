// A mutation's callbacks run after its request returns. If the operator
// switched merchants in the meantime, cache keys built inside those callbacks
// would name the merchant selected now instead of the one the write was made
// under: the initiating merchant keeps showing stale money and an untouched
// merchant's cache is invalidated. These tests switch the stored merchant while
// the request is in flight and pin the resulting invalidations to merchant A.
import { QueryClient } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  putCustomerUsageRateOverride,
  putUsageMeter,
} from "@/lib/api/endpoints"
import { adminMutations } from "@/lib/mutations"
import { merchantQueryKeys, queryKeys } from "@/lib/queries"

vi.mock("@/lib/api/endpoints", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api/endpoints")>()),
  putUsageMeter: vi.fn(),
  putCustomerUsageRateOverride: vi.fn(),
}))

const memoryStorage = (): Storage => {
  const values = new Map<string, string>()
  return {
    get length() {
      return values.size
    },
    clear: () => values.clear(),
    getItem: (key) => values.get(key) ?? null,
    key: (index) => [...values.keys()][index] ?? null,
    removeItem: (key) => values.delete(key),
    setItem: (key, value) => values.set(key, value),
  }
}

const selectMerchant = (merchant: string) =>
  sessionStorage.setItem(
    "openrails.admin.tokens",
    JSON.stringify({ access_token: "token", merchant })
  )

// A request the test holds open, so the merchant can change mid-flight.
const deferred = <T>() => {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((r) => {
    resolve = r
  })
  return { promise, resolve }
}

const MERCHANT_A = "merchant-a"
const MERCHANT_B = "merchant-b"

// Seeds one query per key under both merchants and reports how each side fared.
const seedBothMerchants = (
  queryClient: QueryClient,
  keysOf: (keys: ReturnType<typeof merchantQueryKeys>) => readonly unknown[][]
) => {
  selectMerchant(MERCHANT_A)
  const aKeys = keysOf(queryKeys)
  selectMerchant(MERCHANT_B)
  const bKeys = keysOf(queryKeys)
  for (const key of [...aKeys, ...bKeys]) queryClient.setQueryData(key, {})
  const invalidated = (keys: readonly unknown[][]) =>
    keys.map((key) => queryClient.getQueryState(key)?.isInvalidated)
  return {
    aKeys,
    bKeys,
    initiatingMerchant: () => invalidated(aKeys),
    otherMerchant: () => invalidated(bKeys),
  }
}

beforeEach(() => {
  vi.stubGlobal("localStorage", memoryStorage())
  vi.stubGlobal("sessionStorage", memoryStorage())
  vi.clearAllMocks()
})

afterEach(() => vi.unstubAllGlobals())

describe("merchant-scoped query keys", () => {
  it("pins the merchant selected when the snapshot is taken", () => {
    selectMerchant(MERCHANT_A)
    const pinned = merchantQueryKeys()

    selectMerchant(MERCHANT_B)

    expect(pinned.usageMeter("api-tokens")).toEqual([
      "merchant",
      MERCHANT_A,
      "catalog",
      "meters",
      "api-tokens",
    ])
    expect(pinned.dashboard()).toEqual(["merchant", MERCHANT_A, "dashboard"])
    // The live keys still follow the selection, which is what reads want.
    expect(queryKeys.dashboard()).toEqual(["merchant", MERCHANT_B, "dashboard"])
  })
})

describe("invalidation after a mid-flight merchant switch", () => {
  it("refreshes the meter written under merchant A, not merchant B", async () => {
    const queryClient = new QueryClient()
    const cache = seedBothMerchants(queryClient, (keys) => [
      [...keys.usageMeters()],
      [...keys.usageMeter("api-tokens")],
    ])

    selectMerchant(MERCHANT_A)
    const request = deferred<Awaited<ReturnType<typeof putUsageMeter>>>()
    vi.mocked(putUsageMeter).mockReturnValueOnce(request.promise)
    const meter = {
      event_type: "token.used",
      value_property: "tokens",
      aggregation: "sum" as const,
      unit: "tokens",
      group_by: {},
    }
    const running = queryClient
      .getMutationCache()
      .build(queryClient, adminMutations.putUsageMeter(queryClient))
      .execute({ key: "api-tokens", meter })

    // The operator switches merchants while the write is still open.
    selectMerchant(MERCHANT_B)
    request.resolve({} as never)
    await running

    expect(putUsageMeter).toHaveBeenCalledWith("api-tokens", meter)
    expect(cache.initiatingMerchant()).toEqual([true, true])
    expect(cache.otherMerchant()).toEqual([false, false])
  })

  it("refreshes the customer and dashboard of merchant A, not merchant B", async () => {
    const queryClient = new QueryClient()
    const cache = seedBothMerchants(queryClient, (keys) => [
      [...keys.customer("customer-1")],
      [...keys.customerUsageRates("customer-1")],
      [...keys.usageMeters()],
      [...keys.usageMeter("api-tokens")],
      [...keys.dashboard()],
    ])

    selectMerchant(MERCHANT_A)
    const request =
      deferred<Awaited<ReturnType<typeof putCustomerUsageRateOverride>>>()
    vi.mocked(putCustomerUsageRateOverride).mockReturnValueOnce(
      request.promise
    )
    const override = {
      price: {
        model: "per_unit" as const,
        currency: "USD",
        per_unit: { unit_amount: "500000", divide_by: 1 },
      },
    }
    const running = queryClient
      .getMutationCache()
      .build(
        queryClient,
        adminMutations.putCustomerUsageRateOverride(queryClient)
      )
      .execute({
        customerId: "customer-1",
        meterKey: "api-tokens",
        override,
      })

    selectMerchant(MERCHANT_B)
    request.resolve({} as never)
    await running

    expect(putCustomerUsageRateOverride).toHaveBeenCalledWith(
      "customer-1",
      "api-tokens",
      override
    )
    expect(cache.initiatingMerchant()).toEqual([true, true, true, true, true])
    expect(cache.otherMerchant()).toEqual([false, false, false, false, false])
  })
})
