// Customer support surfaces that move money or show a balance: credit grants
// and revocations, and the collection defaults a saved method carries.
import { QueryClient } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { PaymentMethodResponse } from "@/lib/api/types"
import {
  creditCustomerKey,
  creditMutations,
  creditQueries,
} from "@/lib/credit-queries"
import { adminQueries, queryKeys } from "@/lib/queries"
import {
  calls,
  client,
  exec,
  render,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "@/test/harness"
import { CollectionDefaultBadges } from "./collection-default-badges"
import { CreditPagination, CustomerCreditSupportSection } from "./credits"

const state = vi.hoisted(() => ({ merchant: "alpha" }))
vi.mock("@/lib/auth", () => ({
  useAuth: () => ({ activeMerchant: { instance_slug: state.merchant } }),
}))

const MAX_INT64 = "9223372036854775807"
const grantInput = {
  amount: "1000000",
  currency: "USD",
  source: "admin" as const,
  source_id: "stable-operation",
}

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  routes = {}
  requests = await server(routes)
  state.merchant = "alpha"
  selectMerchant("alpha")
})
afterEach(() => vi.unstubAllGlobals())

describe("credit support requests", () => {
  it("addresses the selected customer, page and currency, and keys them apart", async () => {
    const queries = client()
    await queries.fetchQuery(
      creditQueries.grants("alpha", "cus_a", "EUR", 20, 20)
    )
    await queries.fetchQuery(
      creditQueries.transactions("alpha", "cus_a", "USD", 20, 40)
    )

    expect(calls(requests)).toEqual([
      "GET /merchant/customers/cus_a/credits",
      "GET /merchant/customers/cus_a/credit-transactions",
    ])
    expect(requests[0].query).toBe("currency=EUR&limit=20&offset=20")
    expect(requests[1].query).toBe("currency=USD&limit=20&offset=40")
    for (const other of [
      creditQueries.grants("beta", "cus_a", "USD", 20, 0),
      creditQueries.grants("alpha", "cus_b", "USD", 20, 0),
      creditQueries.grants("alpha", "cus_a", "USD", 20, 20),
    ])
      expect(other.queryKey).not.toEqual(
        creditQueries.grants("alpha", "cus_a", "USD", 20, 0).queryKey
      )
    queries.clear()
  })

  it("retries a failed grant under the same operation identity", async () => {
    const queries = client()
    const options = creditMutations.grant(queries, "alpha", "cus_a")
    let attempt = 0
    routes["POST /merchant/customers/cus_a/credits"] = () =>
      attempt++ === 0
        ? Response.json({ error: { message: "network failed" } }, { status: 503 })
        : { ID: "grant-a", Replayed: true }

    await expect(exec(queries, options, grantInput)).rejects.toThrow(
      "network failed"
    )
    await exec(queries, options, grantInput)

    expect(
      requests.map((request) => (request.body as typeof grantInput).source_id)
    ).toEqual(["stable-operation", "stable-operation"])
  })

  it("refuses a stale-merchant write and never sends it as the new merchant", async () => {
    const queries = client()
    const options = creditMutations.grant(queries, "alpha", "cus_a")
    selectMerchant("beta")

    await expect(exec(queries, options, grantInput)).rejects.toThrow(
      "selected merchant changed"
    )
    expect(requests).toHaveLength(0)
  })

  it("revokes without an optimistic balance change and keeps the server's refusal", async () => {
    const queries = client()
    const key = creditCustomerKey("alpha", "cus_a")
    queries.setQueryData(key, { balance: 100 })
    const revocation = {
      grant: { id: "grant-a", revoked_amount: "70" },
      replayed: false,
    }
    routes["DELETE /merchant/customers/cus_a/credits/grant-a"] = revocation

    await expect(
      exec(queries, creditMutations.revoke(queries, "alpha", "cus_a"), {
        grant: "grant-a",
        reason: "support correction",
      })
    ).resolves.toEqual(revocation)

    expect(requests[0].body).toEqual({ reason: "support correction" })
    // The balance is only ever the server's: the row is refreshed, not edited.
    expect(queries.getQueryData(key)).toEqual({ balance: 100 })
    expect(queries.getQueryState(key)?.isInvalidated).toBe(true)

    routes["DELETE /merchant/customers/cus_a/credits/grant-a"] = () =>
      Response.json(
        { error: { message: "The remaining credit is needed by active holds" } },
        { status: 409 }
      )
    await expect(
      exec(queries, creditMutations.revoke(queries, "alpha", "cus_a"), {
        grant: "grant-a",
        reason: "support",
      })
    ).rejects.toThrow("needed by active holds")
  })
})

describe("credit support rendering", () => {
  const seed = (queries: QueryClient, allowed: boolean) => {
    queries.setQueryData(
      creditQueries.grants("alpha", "cus_a", "USD", 20, 0).queryKey,
      {
        grants: [
          {
            id: "grant-private-alpha",
            customer_id: "cus_a",
            currency: "USD",
            amount: "1000000",
            remaining_amount: MAX_INT64,
            spent_amount: "300000",
            expired_amount: "0",
            revoked_amount: "0",
            state: "active",
            source_type: "admin",
            source_id: "test",
            starts_at: "2026-01-01T00:00:00Z",
            created_at: "2026-01-01T00:00:00Z",
          },
        ],
        total: 21,
        limit: 20,
        offset: 0,
        unit_decimals: 6,
        can_grant: allowed,
        can_revoke: allowed,
      }
    )
    queries.setQueryData(
      creditQueries.transactions("alpha", "cus_a", "USD", 20, 0).queryKey,
      { unit_decimals: 6, transactions: [], total: 0, limit: 20, offset: 0 }
    )
  }
  const section = (queries: QueryClient, customerId = "cus_a") =>
    render(
      <CustomerCreditSupportSection
        customerId={customerId}
        currencies={["USD"]}
      />,
      queries
    )

  it("shows the exact balance and takes write permission from the server", () => {
    const queries = client()
    seed(queries, false)

    const html = section(queries)

    expect(html).toContain("read-only credit access")
    expect(html.replace(/\D/g, "")).toContain(MAX_INT64)
    expect(html).toMatch(/<button[^>]*disabled=""[^>]*>Grant credit<\/button>/)
    expect(html).toMatch(/<button[^>]*disabled=""[^>]*>Revoke<\/button>/)
    queries.clear()
  })

  it("never carries another merchant's or customer's rows across a change", () => {
    const queries = client()
    seed(queries, true)

    expect(section(queries, "cus_b")).not.toContain("grant-private-alpha")
    state.merchant = "beta"
    const html = section(queries)

    expect(html).not.toContain("grant-private-alpha")
    expect(html).toContain("Loading grants")
    queries.clear()
  })

  it("surfaces the load failure instead of an empty ledger", () => {
    const queries = client()
    seed(queries, true)
    const options = creditQueries.grants("alpha", "cus_a", "USD", 20, 0)
    queries.getQueryCache().find({ queryKey: options.queryKey })!.setState({
      data: undefined,
      status: "error",
      error: new Error("Credit service unavailable"),
      fetchStatus: "idle",
    })

    const html = section(queries)

    expect(html).toContain("Credit service unavailable")
    expect(html).toContain("No transactions in this currency")
    queries.clear()
  })

  it("bounds page navigation and disables it while loading", () => {
    const page = (offset: number, busy: boolean) =>
      render(
        <CreditPagination
          total={41}
          offset={offset}
          count={20}
          busy={busy}
          onPage={() => {}}
        />
      )
    expect(page(20, false)).toContain("21–40 of 41")
    expect(page(20, false)).not.toContain('disabled=""')
    expect(page(0, true).match(/ disabled=""/g)).toHaveLength(2)
  })
})

describe("collection defaults", () => {
  it("labels each default with its currency and drops a cleared one", () => {
    const badges = (currencies?: string[]) =>
      render(<CollectionDefaultBadges currencies={currencies} />)
    expect(badges(["EUR", "USD"])).toContain("Collection default · EUR")
    expect(badges(["EUR", "USD"])).toContain("Collection default · USD")
    expect(badges([])).not.toContain("Collection default")
    expect(badges()).not.toContain("Collection default")
  })

  it("refreshes the profile and saved-method views together", async () => {
    const method: PaymentMethodResponse = {
      id: "pm_a",
      object: "payment_method",
      type: "card",
      rail: "nmi",
      created_at: "2026-09-16T00:00:00Z",
      collection_default_currencies: ["USD"],
    }
    let methods = [method]
    routes["/merchant/customers/cus_a"] = () => ({
      customer_id: "cus_a",
      subscriptions: [],
      entitlements: [],
      payments: [],
      payment_methods: methods,
      credit_balance: [],
      product_access: [],
    })
    routes["/merchant/customers/cus_a/payment-methods"] = () => ({
      object: "list",
      data: methods,
    })
    const queries = client()
    const profile = adminQueries.customer("cus_a")
    const saved = adminQueries.customerPaymentMethods("cus_a")
    await Promise.all([
      queries.fetchQuery(profile),
      queries.fetchQuery(saved),
    ])

    // The same customer subtree the Refresh payment methods action invalidates.
    methods = [{ ...method, collection_default_currencies: [] }]
    await queries.invalidateQueries({ queryKey: queryKeys.customer("cus_a") })
    await Promise.all([
      queries.fetchQuery(profile),
      queries.fetchQuery(saved),
    ])

    const current = queries.getQueryData(profile.queryKey)!
    expect(current.payment_methods[0].collection_default_currencies).toEqual([])
    expect(
      queries.getQueryData(saved.queryKey)!.data[0]
        .collection_default_currencies
    ).toEqual([])
    expect(
      render(<CollectionDefaultBadges currencies={[]} />)
    ).not.toContain("Collection default")
    queries.clear()
  })
})
