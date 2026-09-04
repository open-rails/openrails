import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderToStaticMarkup } from "react-dom/server"
import { beforeEach, describe, expect, it, vi } from "vitest"
import { creditQueries } from "@/lib/credit-queries"
import { CreditPagination, CustomerCreditSupportSection } from "./credits"

const state = vi.hoisted(() => ({ merchant: "alpha" }))
vi.mock("@/lib/auth", () => ({
  useAuth: () => ({ activeMerchant: { instance_slug: state.merchant } }),
}))
vi.mock("@/lib/api/client", () => ({
  getTokens: () => ({ merchant: state.merchant }),
  api: vi.fn(),
}))
beforeEach(() => {
  state.merchant = "alpha"
})

function renderCredits(client: QueryClient, customer = "customer-a") {
  return renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <CustomerCreditSupportSection
        customerId={customer}
        currencies={["USD"]}
      />
    </QueryClientProvider>
  )
}

function seed(client: QueryClient, allowed: boolean) {
  client.setQueryData(
    creditQueries.grants("alpha", "customer-a", "USD", 20, 0).queryKey,
    {
      grants: [
        {
          id: "grant-private-alpha",
          customer_id: "customer-a",
          currency: "USD",
          amount: 1000000,
          remaining_amount: 700000,
          spent_amount: 300000,
          expired_amount: 0,
          revoked_amount: 0,
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
  client.setQueryData(
    creditQueries.transactions("alpha", "customer-a", "USD", 20, 0).queryKey,
    { unit_decimals: 6, transactions: [], total: 0, limit: 20, offset: 0 }
  )
}

describe("customer credit support", () => {
  it("disables write actions from server permissions while rendering balances and history", () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retryOnMount: false, retry: false } },
    })
    seed(client, false)
    const html = renderCredits(client)
    expect(html).toContain("read-only credit access")
    expect(html).toContain("grant-private-alpha")
    expect(html).toMatch(/<button[^>]*disabled=""[^>]*>Grant credit<\/button>/)
    expect(html).toMatch(/<button[^>]*disabled=""[^>]*>Revoke<\/button>/)
    expect(html).toContain("Transaction ledger")
    client.clear()
  })
  it("does not carry another merchant or customer's rows across selection changes", () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retryOnMount: false, retry: false } },
    })
    seed(client, true)
    expect(renderCredits(client, "customer-b")).not.toContain(
      "grant-private-alpha"
    )
    state.merchant = "beta"
    const html = renderCredits(client)
    expect(html).not.toContain("grant-private-alpha")
    expect(html).toContain("Loading grants")
    client.clear()
  })
  it("shows empty and error states", () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retryOnMount: false, retry: false } },
    })
    seed(client, true)
    const options = creditQueries.grants("alpha", "customer-a", "USD", 20, 0)
    const query = client.getQueryCache().find({ queryKey: options.queryKey })!
    query.setState({
      data: undefined,
      status: "error",
      error: new Error("Credit service unavailable"),
      fetchStatus: "idle",
    })
    const html = renderCredits(client)
    expect(html).toContain("Credit service unavailable")
    expect(html).toContain("No transactions in this currency")
    client.clear()
  })
  it("renders bounded page navigation and disables controls while loading", () => {
    const html = renderToStaticMarkup(
      <CreditPagination
        total={41}
        offset={20}
        count={20}
        busy={false}
        onPage={() => {}}
      />
    )
    expect(html).toContain("21–40 of 41")
    expect(html).not.toContain('disabled=""')
    const busy = renderToStaticMarkup(
      <CreditPagination
        total={41}
        offset={0}
        count={20}
        busy
        onPage={() => {}}
      />
    )
    expect(busy.match(/ disabled=""/g)).toHaveLength(2)
  })
})
