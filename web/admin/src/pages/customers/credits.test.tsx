// Customer support surfaces that move money or show a balance: credit grants
// and revocations, and the collection defaults a saved method carries.
import type { QueryClient } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { creditCustomerKey, creditMutations, creditQueries } from "@/lib/credit-queries"
import { adminQueries, queryKeys } from "@/lib/queries"
import {
  aPaymentMethod, calls, client, cursorPages, exec, MAX_INT64, render, selectMerchant, server,
  type Recorded, type Reply,
} from "@/test/harness"
import { CollectionDefaultBadges } from "./collection-default-badges"
import { CustomerCreditSupportSection } from "./credits"

const state = vi.hoisted(() => ({ merchant: "alpha" }))
vi.mock("@/lib/auth", () => ({
  useAuth: () => ({ activeMerchant: { slug: state.merchant } }),
}))

const grantInput = { amount: "1000000", currency: "USD", source: "admin" as const, source_id: "stable-operation" }

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
  it("addresses the selected customer, page and currency", async () => {
    const queries = client()
    await queries.fetchQuery(creditQueries.grants("alpha", "cus_a", "EUR", 20, "c2"))
    await queries.fetchQuery(creditQueries.transactions("alpha", "cus_a", "USD", 20, ""))
    expect(calls(requests)).toEqual([
      "GET /merchant/customers/cus_a/credit-grants",
      "GET /merchant/customers/cus_a/transactions",
    ])
    expect(requests.map((request) => request.query)).toEqual([
      "currency=EUR&limit=20&cursor=c2",
      "currency=USD&limit=20",
    ])
    queries.clear()
  })

  it("retries a failed grant under the same operation identity", async () => {
    const queries = client()
    const options = creditMutations.grant(queries, "alpha", "cus_a")
    let attempt = 0
    routes["POST /merchant/customers/cus_a/credit-grants"] = () =>
      attempt++ === 0
        ? Response.json({ error: { message: "network failed" } }, { status: 503 })
        : { id: "cgr_a", replayed: true }
    await expect(exec(queries, options, grantInput)).rejects.toThrow("network failed")
    await exec(queries, options, grantInput)
    expect(requests.map((r) => (r.body as typeof grantInput).source_id)).toEqual([
      "stable-operation",
      "stable-operation",
    ])
  })

  it("refuses a stale-merchant write and never sends it as the new merchant", async () => {
    const queries = client()
    const options = creditMutations.grant(queries, "alpha", "cus_a")
    selectMerchant("beta")
    await expect(exec(queries, options, grantInput)).rejects.toThrow("selected merchant changed")
    expect(requests).toHaveLength(0)
  })

  it("revokes without an optimistic balance change and keeps the server's refusal", async () => {
    const queries = client()
    const key = creditCustomerKey("alpha", "cus_a")
    queries.setQueryData(key, { balance: 100 })
    const revocation = { id: "grant-a", revoked_amount: "70", replayed: false }
    routes["POST /merchant/customers/cus_a/credit-grants/grant-a/revoke"] = revocation
    const revoke = (reason: string) =>
      exec(queries, creditMutations.revoke(queries, "alpha", "cus_a"), { grant: "grant-a", reason })

    await expect(revoke("support correction")).resolves.toEqual(revocation)
    expect(requests[0].body).toEqual({ reason: "support correction" })
    // The balance is only ever the server's: the row is refreshed, not edited.
    expect(queries.getQueryData(key)).toEqual({ balance: 100 })
    expect(queries.getQueryState(key)?.isInvalidated).toBe(true)

    routes["POST /merchant/customers/cus_a/credit-grants/grant-a/revoke"] = () =>
      Response.json({ error: { message: "The remaining credit is needed by active holds" } }, { status: 409 })
    await expect(revoke("support")).rejects.toThrow("needed by active holds")
  })
})

describe("credit support rendering", () => {
  const seed = (queries: QueryClient, remaining = MAX_INT64) => {
    queries.setQueryData(creditQueries.grants("alpha", "cus_a", "USD", 20, "").queryKey, {
      data: [{
        id: "grant-private-alpha", customer_id: "cus_a", currency: "USD",
        amount: "1000000", remaining_amount: remaining, spent_amount: "300000",
        expired_amount: "0", revoked_amount: "0", state: "active",
        source_type: "admin", source_id: "test", description: null,
        starts_at: "2026-01-01T00:00:00Z", expires_at: null, created_at: "2026-01-01T00:00:00Z",
        terminated_at: null, termination_reason: null, replayed: false,
      }],
      next_cursor: "next",
    })
    queries.setQueryData(creditQueries.transactions("alpha", "cus_a", "USD", 20, "").queryKey, {
      data: [], next_cursor: null,
    })
  }
  const section = (queries: QueryClient, customerId = "cus_a") =>
    render(<CustomerCreditSupportSection customerId={customerId} currencies={["USD"]} />, queries)

  it("shows the exact balance and offers revoke only while credit remains", () => {
    const queries = client()
    seed(queries)
    const html = section(queries)
    expect(html.replace(/\D/g, "")).toContain(MAX_INT64)
    expect(html).not.toMatch(/<button[^>]*disabled=""[^>]*>Grant credit<\/button>/)
    expect(html).not.toMatch(/<button[^>]*disabled=""[^>]*>Revoke<\/button>/)
    expect(html).toMatch(/<button[^>]*aria-label="Next page"[^>]*>/)
    queries.clear()

    const spent = client()
    seed(spent, "0")
    expect(section(spent)).toMatch(/<button[^>]*disabled=""[^>]*>Revoke<\/button>/)
    spent.clear()
  })

  it("never carries another merchant's or customer's rows across a change", () => {
    const queries = client()
    seed(queries)
    expect(section(queries, "cus_b")).not.toContain("grant-private-alpha")
    state.merchant = "beta"
    const html = section(queries)
    expect(html).not.toContain("grant-private-alpha")
    expect(html).toContain("Loading grants")
    queries.clear()
  })

  it("surfaces the load failure instead of an empty ledger", () => {
    const queries = client()
    seed(queries)
    const key = creditQueries.grants("alpha", "cus_a", "USD", 20, "").queryKey
    queries.getQueryCache().find({ queryKey: key })!.setState({
      data: undefined, status: "error", fetchStatus: "idle",
      error: new Error("Credit service unavailable"),
    })
    const html = section(queries)
    expect(html).toContain("Credit service unavailable")
    expect(html).toContain("No transactions in this currency")
    queries.clear()
  })
})

describe("collection defaults", () => {
  const badges = (currencies?: string[]) => render(<CollectionDefaultBadges currencies={currencies} />)

  it("labels each default with its currency and drops a cleared one", () => {
    expect(badges(["EUR", "USD"])).toContain("Collection default · EUR")
    expect(badges(["EUR", "USD"])).toContain("Collection default · USD")
    expect(badges([])).not.toContain("Collection default")
    expect(badges()).not.toContain("Collection default")
  })

  it("refreshes the profile and saved-method views together", async () => {
    const method = aPaymentMethod("pm_a", { collection_currencies: ["USD"] })
    let methods = [method, aPaymentMethod("pm_b")]
    routes["/merchant/customers/cus_a/billing-profile"] = () => ({
      customer: { id: "cus_a", email: null, created_at: "2026-09-16T00:00:00Z", last_seen_at: "2026-09-16T00:00:00Z" },
      balances: [], subscriptions: [], entitlements: [], payments: [],
      payment_methods: methods, product_access: [],
    })
    // One method per page: the picker list walks every page.
    routes["/merchant/customers/cus_a/payment-methods"] = (request) => cursorPages(methods, 1)(request)
    const queries = client()
    const profile = adminQueries.customer("cus_a")
    const saved = adminQueries.customerPaymentMethods("cus_a")
    const load = () => Promise.all([queries.fetchQuery(profile), queries.fetchQuery(saved)])
    await load()

    // The same customer subtree the Refresh payment methods action invalidates.
    methods = [{ ...method, collection_currencies: [] }, aPaymentMethod("pm_b")]
    await queries.invalidateQueries({ queryKey: queryKeys.customer("cus_a") })
    await load()

    expect(queries.getQueryData(profile.queryKey)!.payment_methods![0].collection_currencies).toEqual([])
    expect(queries.getQueryData(saved.queryKey)!.map((m) => [m.id, m.collection_currencies])).toEqual([["pm_a", []], ["pm_b", null]])
    expect(requests.filter((r) => r.path.endsWith("/payment-methods")).map((r) => r.query)).toEqual([
      "limit=100", "limit=100&cursor=1", "limit=100", "limit=100&cursor=1",
    ])
    queries.clear()
  })
})
