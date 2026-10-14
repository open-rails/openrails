import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "./client/client"
import type { Product } from "./client/types"
import { ProductList } from "./product-list"
import { BillingProvider } from "./react/provider"
import {
  apiError,
  fakeBilling,
  price,
  product,
  type FakeBilling,
} from "./test/billing-server"

// The README's catalog, as the host's server puts it in its page: a course to
// buy or rent, a bundle, a membership.
const catalog = (): Product[] =>
  [
    product({
      id: "prod_101",
      key: "course-101",
      display_name: "Course 101",
      description: "Intro to CSS",
      entitlements: ["course:101"],
      prices: [
        price(null, null, {
          id: "price_buy",
          key: "purchase",
          product_id: "prod_101",
          unit_amount: "4990000",
        }),
        price(72, null, {
          id: "price_rent",
          key: "rent",
          product_id: "prod_101",
          unit_amount: "1990000",
        }),
      ],
    }),
    product({
      id: "prod_bundle",
      key: "course-bundle",
      display_name: "Both courses",
      entitlements: ["course:101", "course:102"],
      prices: [
        price(null, null, {
          id: "price_bundle",
          key: "purchase",
          product_id: "prod_bundle",
          unit_amount: "8990000",
        }),
      ],
    }),
    product({
      id: "prod_member",
      key: "channel-membership",
      display_name: "Membership",
      entitlements: ["channel:membership"],
      prices: [
        price(720, 720, {
          id: "price_monthly",
          key: "monthly",
          product_id: "prod_member",
          unit_amount: "10000000",
        }),
      ],
    }),
  ] as unknown as Product[]

function mount(ui: ReactNode, server: FakeBilling) {
  return render(
    <BillingProvider
      locale="en-US"
      client={createBillingClient({ fetch: server.fetch })}
    >
      {ui}
    </BillingProvider>
  )
}

describe("ProductList", () => {
  it("displays each product the host names, a button per price, and reads no catalog", async () => {
    const server = fakeBilling()
    mount(
      <ProductList
        products={catalog().slice(0, 2)}
        onPaid={vi.fn()}
        onSignInRequired={vi.fn()}
      />,
      server
    )

    expect(await screen.findAllByTestId("product")).toHaveLength(2)
    expect(
      screen.getByRole("heading", { name: "Course 101" })
    ).toBeInTheDocument()
    expect(screen.getByText("Intro to CSS")).toBeInTheDocument()
    expect(
      screen.getByRole("heading", { name: "Both courses" })
    ).toBeInTheDocument()
    for (const label of ["$4.99", "Rent for 3 days, $1.99", "$8.99"])
      expect(screen.getByRole("button", { name: label })).toBeInTheDocument()
    expect(screen.queryByText("Membership")).toBeNull()
    expect(server.calls.filter((c) => c.includes("product"))).toEqual([])
  })

  it("labels a recurring price with its cadence", async () => {
    const server = fakeBilling()
    mount(
      <ProductList
        products={catalog().slice(2)}
        onPaid={vi.fn()}
        onSignInRequired={vi.fn()}
      />,
      server
    )
    expect(
      await screen.findByRole("button", { name: "$10.00 every 30 days" })
    ).toBeInTheDocument()
  })

  it.each([null, ["solana"]])(
    "deduplicates only a card-eligible matching CCBill offer (%j)",
    async (sellVia) => {
      const server = fakeBilling()
      const member = catalog()[2]!
      const own = { ...member.prices[0]!, sell_via: sellVia }
      const provider = { ...own, id: "price_ccbill", key: "ccbill", sell_via: null,
        provider: { psp: "ccbill", psp_id: "psp_ccbill", rail: "ccbill", ids: {}, sync_status: "unknown" as const, drift: [], access_duration_default: false } }
      mount(<ProductList products={[{ ...member, prices: [own, provider] }]} onPaid={vi.fn()} onSignInRequired={vi.fn()} />, server)
      expect(await screen.findAllByRole("button", { name: "$10.00 every 30 days" })).toHaveLength(sellVia ? 2 : 1)
    }
  )

  it("keeps an identical trial provider offer because it cannot be an order fallback", async () => {
    const server = fakeBilling()
    const member = catalog()[2]!
    const own = { ...member.prices[0]!, trial_unit_amount: "0", trial_duration_hours: 24 }
    const provider = { ...own, id: "price_same_trial", key: "same-trial", provider: { psp: "ccbill", psp_id: "psp_ccbill", rail: "ccbill", ids: {}, sync_status: "unknown" as const, drift: [], access_duration_default: false } }
    mount(<ProductList products={[{ ...member, prices: [own, provider] }]} onPaid={vi.fn()} onSignInRequired={vi.fn()} />, server)
    expect(await screen.findAllByRole("button")).toHaveLength(2)
  })

  it("keeps a distinct trial and hides legacy provider prices", async () => {
    const server = fakeBilling()
    const member = catalog()[2]!
    const own = member.prices[0]!
    const ccbill = { ...own, id: "price_trial", key: "ccbill-trial", trial_unit_amount: "0", trial_duration_hours: 24,
      provider: { psp: "ccbill", psp_id: "psp_ccbill", rail: "ccbill", ids: {}, sync_status: "unknown" as const, drift: [], access_duration_default: false } }
    const legacy = { ...own, id: "price_legacy", key: "legacy", provider: { ...ccbill.provider, rail: "nmi" } }
    mount(<ProductList products={[{ ...member, prices: [own, ccbill, legacy] }]} onPaid={vi.fn()} onSignInRequired={vi.fn()} />, server)
    expect(await screen.findAllByRole("button")).toHaveLength(2)
  })

  it("orders a price and calls onPaid once the order is complete", async () => {
    const server = fakeBilling()
    server.orderStatus = "complete"
    const onPaid = vi.fn()
    mount(
      <ProductList
        products={catalog()}
        onPaid={onPaid}
        onSignInRequired={vi.fn()}
      />,
      server
    )
    fireEvent.click(
      await screen.findByRole("button", { name: "Rent for 3 days, $1.99" })
    )

    await waitFor(() =>
      expect(onPaid).toHaveBeenCalledWith(
        expect.objectContaining({ status: "complete" })
      )
    )
    expect(server.calls).toContain("POST /me/orders")
    expect(server.calls).toContain("GET /me/orders/ord_price_rent")
    const create = server.fetch.mock.calls.find(
      ([, init]) => init?.method === "POST"
    )!
    expect(JSON.parse(String(create[1]!.body))).toEqual({
      lines: [{ price_id: "price_rent" }],
    })
  })

  it("asks the host to sign the visitor in when it says they are signed out", async () => {
    const server = fakeBilling()
    const onSignInRequired = vi.fn()
    mount(
      <ProductList
        products={catalog()}
        signedIn={false}
        onPaid={vi.fn()}
        onSignInRequired={onSignInRequired}
      />,
      server
    )
    fireEvent.click(await screen.findByRole("button", { name: "$4.99" }))

    expect(onSignInRequired).toHaveBeenCalledOnce()
    expect(server.calls).not.toContain("POST /me/orders")
  })

  it("asks the host to sign the visitor in on a 401", async () => {
    const server = fakeBilling()
    server.fail["POST /me/orders"] = apiError(401, "authentication_required")
    const onSignInRequired = vi.fn()
    mount(
      <ProductList
        products={catalog()}
        onPaid={vi.fn()}
        onSignInRequired={onSignInRequired}
      />,
      server
    )
    fireEvent.click(await screen.findByRole("button", { name: "$4.99" }))

    await waitFor(() => expect(onSignInRequired).toHaveBeenCalledOnce())
    expect(screen.queryByRole("dialog")).toBeNull()
  })

  it("shows any other refusal", async () => {
    const server = fakeBilling()
    server.fail["POST /me/orders"] = apiError(
      404,
      "price_not_found",
      "That price is no longer on sale."
    )
    const onSignInRequired = vi.fn()
    mount(
      <ProductList
        products={catalog()}
        onPaid={vi.fn()}
        onSignInRequired={onSignInRequired}
      />,
      server
    )
    fireEvent.click(await screen.findByRole("button", { name: "$4.99" }))

    expect(await screen.findByRole("alert")).toBeInTheDocument()
    expect(onSignInRequired).not.toHaveBeenCalled()
  })

  it("hides an archived product even when its prices remain current", () => {
    const server = fakeBilling()
    const products = catalog().slice(0, 1)
    products[0].archived = true
    mount(
      <ProductList
        products={products}
        onPaid={vi.fn()}
        onSignInRequired={vi.fn()}
      />,
      server
    )
    expect(screen.queryByRole("button", { name: "$4.99" })).toBeNull()
    expect(screen.queryByTestId("product")).toBeNull()
  })

  it("says when none of its products is on sale", async () => {
    mount(
      <ProductList products={[]} onPaid={vi.fn()} onSignInRequired={vi.fn()} />,
      fakeBilling()
    )
    expect(
      await screen.findByText("Nothing on sale unlocks this")
    ).toBeInTheDocument()
  })
})
