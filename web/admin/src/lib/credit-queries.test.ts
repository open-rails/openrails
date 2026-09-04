import { QueryClient } from "@tanstack/react-query"
import { beforeEach, describe, expect, it, vi } from "vitest"
import {
  creditCustomerKey,
  creditMutations,
  creditQueries,
} from "./credit-queries"

const state = vi.hoisted(() => ({ merchant: "alpha", api: vi.fn() }))
vi.mock("@/lib/api/client", () => ({
  getTokens: () => ({ merchant: state.merchant }),
  api: state.api,
}))

beforeEach(() => {
  state.merchant = "alpha"
  state.api.mockReset()
})

const input = {
  amount: 1000000,
  currency: "USD",
  source: "admin" as const,
  source_id: "stable-operation",
}

describe("credit support queries", () => {
  it("addresses the selected customer and the requested page and currency", async () => {
    const client = new QueryClient()
    state.api.mockResolvedValue({ grants: [], total: 0, limit: 20, offset: 20 })
    await client.fetchQuery(
      creditQueries.grants("alpha", "customer-a", "EUR", 20, 20)
    )
    expect(state.api).toHaveBeenCalledWith(
      "/merchant/customers/customer-a/credits",
      expect.objectContaining({
        query: { currency: "EUR", limit: 20, offset: 20 },
      })
    )
    expect(
      creditQueries.grants("alpha", "customer-a", "USD", 20, 0).queryKey
    ).not.toEqual(
      creditQueries.grants("beta", "customer-a", "USD", 20, 0).queryKey
    )
    expect(
      creditQueries.grants("alpha", "customer-a", "USD", 20, 0).queryKey
    ).not.toEqual(
      creditQueries.grants("alpha", "customer-b", "USD", 20, 0).queryKey
    )
    client.clear()
  })

  it("loads the transaction ledger with actual server pagination", async () => {
    const client = new QueryClient()
    state.api.mockResolvedValue({
      transactions: [],
      total: 41,
      limit: 20,
      offset: 40,
    })
    await client.fetchQuery(
      creditQueries.transactions("alpha", "customer-a", "USD", 20, 40)
    )
    expect(state.api).toHaveBeenCalledWith(
      "/merchant/customers/customer-a/credit-transactions",
      expect.objectContaining({
        query: { currency: "USD", limit: 20, offset: 40 },
      })
    )
    client.clear()
  })

  it("uses the existing creation endpoint and invalidates the customer's balances and histories", async () => {
    const client = new QueryClient()
    const invalidate = vi.spyOn(client, "invalidateQueries").mockResolvedValue()
    state.api.mockResolvedValue({ id: "grant-a" })
    await client
      .getMutationCache()
      .build(client, creditMutations.grant(client, "alpha", "customer-a"))
      .execute(input)
    expect(state.api).toHaveBeenCalledWith(
      "/merchant/customers/customer-a/credits",
      { method: "POST", body: input }
    )
    expect(invalidate).toHaveBeenCalledWith({
      queryKey: creditCustomerKey("alpha", "customer-a"),
    })
    client.clear()
  })

  it("retries a failed creation with the same operation identity", async () => {
    const client = new QueryClient()
    state.api
      .mockRejectedValueOnce(new Error("network failed"))
      .mockResolvedValue({ id: "grant-a", replayed: true })
    const options = creditMutations.grant(client, "alpha", "customer-a")
    await expect(
      client.getMutationCache().build(client, options).execute(input)
    ).rejects.toThrow("network failed")
    await client.getMutationCache().build(client, options).execute(input)
    expect(state.api.mock.calls.map((call) => call[1].body.source_id)).toEqual([
      "stable-operation",
      "stable-operation",
    ])
    client.clear()
  })

  it("revokes the addressed grant and refreshes without an optimistic balance change", async () => {
    const client = new QueryClient()
    const key = creditCustomerKey("alpha", "customer-a")
    client.setQueryData(key, { balance: 100 })
    const invalidate = vi.spyOn(client, "invalidateQueries").mockResolvedValue()
    const result = {
      grant: { id: "grant-a", revoked_amount: 70 },
      replayed: false,
    }
    state.api.mockResolvedValue(result)
    await expect(
      client
        .getMutationCache()
        .build(client, creditMutations.revoke(client, "alpha", "customer-a"))
        .execute({ grant: "grant-a", reason: "support correction" })
    ).resolves.toEqual(result)
    expect(state.api).toHaveBeenCalledWith(
      "/merchant/customers/customer-a/credits/grant-a",
      { method: "DELETE", body: { reason: "support correction" } }
    )
    expect(client.getQueryData(key)).toEqual({ balance: 100 })
    expect(invalidate).toHaveBeenCalledWith({ queryKey: key })
    client.clear()
  })

  it.each([
    "The remaining credit is needed by active holds",
    "Credit grant expired",
    "Service unavailable",
  ])("preserves the server refusal: %s", async (message) => {
    const client = new QueryClient()
    state.api.mockRejectedValue(new Error(message))
    await expect(
      client
        .getMutationCache()
        .build(client, creditMutations.revoke(client, "alpha", "customer-a"))
        .execute({ grant: "grant-a", reason: "support" })
    ).rejects.toThrow(message)
    client.clear()
  })

  it("refuses a stale merchant action and never sends it under the new merchant", async () => {
    const client = new QueryClient()
    const options = creditMutations.grant(client, "alpha", "customer-a")
    state.merchant = "beta"
    await expect(
      client.getMutationCache().build(client, options).execute(input)
    ).rejects.toThrow("selected merchant changed")
    expect(state.api).not.toHaveBeenCalled()
    client.clear()
  })
})
