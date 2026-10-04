// @vitest-environment jsdom
// The payments screens mounted against the real API client: lists walk the
// server's cursors and start over on a filter change, and a payment shows
// what moved, how, on which card, and what reversed it.
import { QueryClientProvider } from "@tanstack/react-query"
import type { ReactNode } from "react"
import { MemoryRouter, Route, Routes } from "react-router-dom"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  aPayment,
  client,
  cursorPages,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "@/test/harness"
import { act, browserEnvironment, click, mount, unmount } from "@/test/mount"
import { AttemptsPage } from "./attempts"
import { PaymentDetailPage } from "./detail"
import { PaymentsPage } from "./index"

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  browserEnvironment()
  routes = {}
  requests = await server(routes)
  selectMerchant("merchant-a")
})
afterEach(unmount)

const at = (path: string, pattern: string, page: ReactNode) =>
  mount(
    <QueryClientProvider client={client()}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path={pattern} element={page} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  )
const text = () => document.body.textContent ?? ""
const pager = (label: "Previous page" | "Next page") =>
  document.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`)
const press = async (label: "Previous page" | "Next page") => {
  await vi.waitFor(() => expect(pager(label)?.disabled).toBe(false))
  await act(async () => pager(label)!.click())
}
const refundButton = () =>
  [...document.querySelectorAll("button")].find(
    (b) => b.textContent?.trim() === "Refund"
  )!
const sent = (path: string) =>
  requests.filter((r) => r.path === path).map((r) => r.query)

describe("payment list", () => {
  const manual = aPayment("pay_manual", {
    channel: "manual",
    rail: null,
    psp_id: null,
  })
  const refund = aPayment("pay_refund", {
    kind: "refund",
    amount: "-5000000",
    refunded_payment_id: "pay_1",
  })

  it("pages by the server's cursor and starts over when the kind changes", async () => {
    routes["/merchant/payments"] = cursorPages(
      [aPayment("pay_1"), manual, refund],
      2
    )
    await at("/payments?rail=nmi", "/payments", <PaymentsPage />)
    await vi.waitFor(() => expect(text()).toContain("pay_manual"))
    // An off-rail payment names its channel where a rail would be.
    expect(text()).toContain("chargemanual")
    expect(pager("Previous page")!.disabled).toBe(true)

    await press("Next page")
    await vi.waitFor(() => expect(text()).toContain("pay_refund"))
    expect(text()).toContain("-$5.00")
    expect(pager("Next page")!.disabled).toBe(true)

    await press("Previous page")
    await vi.waitFor(() => expect(text()).toContain("pay_manual"))
    await click("Refunds")
    await vi.waitFor(() =>
      expect(sent("/merchant/payments")).toContain(
        "rail=nmi&kind=refund&limit=50"
      )
    )
    expect(sent("/merchant/payments")).toContain("rail=nmi&limit=50&cursor=2")
    expect(
      sent("/merchant/payments").filter((q) => q.includes("kind=refund"))
    ).toEqual(["rail=nmi&kind=refund&limit=50"])
  })

  it("refuses a garbled cursor instead of showing another page", async () => {
    routes["/merchant/payments"] = () =>
      Response.json(
        { error: { code: "invalid_cursor", message: "bad cursor" } },
        { status: 400 }
      )
    await at("/payments?cursor=garbled", "/payments", <PaymentsPage />)
    await vi.waitFor(() =>
      expect(sent("/merchant/payments")).toEqual(["limit=50&cursor=garbled"])
    )
    expect(text()).toContain("No payments match.")
    // Previous still leads back to the first page.
    expect(pager("Previous page")!.disabled).toBe(false)
  })
})

describe("payment attempts", () => {
  it("shows the card an attempt was made with and pages on", async () => {
    const attempt = (id: string) => ({
      id,
      kind: "rebill",
      owner: "engine",
      category: "issuer_soft",
      reason: "insufficient_funds",
      response_code: "51",
      card: { brand: "visa", last4: "4242", exp_month: 1, exp_year: 2030 },
      amount: "20000000",
      currency: "USD",
      rail: "nmi",
      attempted_at: "2026-09-18T00:00:00Z",
    })
    routes["/merchant/payment-attempts"] = cursorPages(
      [attempt("att_1"), attempt("att_2")],
      1
    )
    await at(
      "/payments/attempts?owner=engine",
      "/payments/attempts",
      <AttemptsPage />
    )
    await vi.waitFor(() => expect(text()).toContain("visa ••••4242"))
    await press("Next page")
    expect(sent("/merchant/payment-attempts")).toEqual([
      "owner=engine&limit=50",
      "owner=engine&limit=50&cursor=1",
    ])
  })
})

describe("payment detail", () => {
  const charge = aPayment("pay_1", {
    status: "partially_refunded",
    amount_refunded: "5000000",
    card: { brand: "visa", last4: "4242", exp_month: 12, exp_year: 2030 },
    product: {
      id: "prod_1",
      key: "pro",
      display_name: "Pro plan",
      description: "",
      tier_rank: 1,
      archived: false,
    },
    refunds: [
      aPayment("pay_r1", {
        kind: "refund",
        amount: "-5000000",
        refunded_payment_id: "pay_1",
        reason: "requested",
      }),
    ],
  })

  it("shows the card, product and refunds, and refunds what remains", async () => {
    let refunded = false
    routes["/merchant/payments/pay_1"] = () =>
      refunded ? { ...charge, status: "refunded" } : charge
    routes["POST /merchant/payments/pay_1/refunds"] = () => {
      refunded = true
      return Response.json(
        aPayment("pay_r2", { kind: "refund", status: "pending" }),
        { status: 202 }
      )
    }
    await at("/payments/pay_1", "/payments/:id", <PaymentDetailPage />)
    await vi.waitFor(() => expect(text()).toContain("Pro plan"))
    expect(text()).toContain("visa ••••4242")
    expect(text()).toContain("pay_r1")
    expect(text()).toContain("-$5.00")

    await act(async () => refundButton().click())
    const submit = document.querySelector<HTMLButtonElement>(
      '[role="dialog"] button[type="submit"]'
    )!
    await act(async () => submit.click())
    await vi.waitFor(() =>
      expect(sent("/merchant/payments/pay_1")).toHaveLength(2)
    )
    const post = requests.find((r) => r.method === "POST")!
    expect(post.body).toEqual({ amount: "15000000", revoke_access: false })
    expect(post.headers.get("Idempotency-Key")).toMatch(/^[\da-f-]{36}$/)
    // The refund refreshed the payment it reverses.
    await vi.waitFor(() => expect(text()).not.toContain("partially_refunded"))
  })

  it("offers no refund for a payment taken off-rail", async () => {
    routes["/merchant/payments/pay_m"] = aPayment("pay_m", {
      channel: "manual",
      rail: null,
      psp_id: null,
    })
    await at("/payments/pay_m", "/payments/:id", <PaymentDetailPage />)
    await vi.waitFor(() => expect(text()).toContain("charge · manual"))
    expect(refundButton().disabled).toBe(true)
    expect(refundButton().title).toBe(
      "A manual payment is refunded where it was taken"
    )
  })
})
