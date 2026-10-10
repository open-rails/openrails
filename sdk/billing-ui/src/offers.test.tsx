import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "./client/client"
import type { Product } from "./client/types"
import { Offers } from "./offers"
import { BillingUiProvider } from "./provider"
import { BillingProvider } from "./react/provider"
import {
  apiError,
  fakeBilling,
  price,
  product,
  type FakeBilling,
} from "./test/billing-server"

// The README's catalog: a course to buy or rent, a bundle, a membership.
const catalog = () => [
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
]

function mount(ui: ReactNode, server: FakeBilling) {
  return render(
    <BillingUiProvider locale="en-US">
      <BillingProvider client={createBillingClient({ fetch: server.fetch })}>
        {ui}
      </BillingProvider>
    </BillingUiProvider>
  )
}

describe("Offers", () => {
  it("offers every product granting the entitlement, a button per price", async () => {
    const server = fakeBilling({ products: catalog() })
    mount(
      <Offers
        entitlement="course:101"
        onPaid={vi.fn()}
        onSignInRequired={vi.fn()}
      />,
      server
    )

    expect(await screen.findAllByTestId("offer")).toHaveLength(2)
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
    const urls = server.fetch.mock.calls.map(([url]) => String(url))
    expect(urls).toContain(
      "/billing/v1/catalog/products?entitlement=course%3A101&limit=100"
    )
  })

  it("labels a recurring price with its cadence", async () => {
    const server = fakeBilling({ products: catalog() })
    mount(
      <Offers
        entitlement="channel:membership"
        onPaid={vi.fn()}
        onSignInRequired={vi.fn()}
      />,
      server
    )
    expect(
      await screen.findByRole("button", { name: "$10.00 every 30 days" })
    ).toBeInTheDocument()
  })

  it("checks out a price and calls onPaid once it succeeds", async () => {
    const server = fakeBilling({ products: catalog() })
    const onPaid = vi.fn()
    mount(
      <Offers
        entitlement="course:101"
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
        expect.objectContaining({ status: "succeeded" })
      )
    )
    expect(server.calls).toContain("POST /me/checkout-sessions")
    expect(server.calls).toContain("GET /me/checkout-sessions/ocs_price_rent")
    const mint = server.fetch.mock.calls.find(
      ([, init]) => init?.method === "POST"
    )!
    expect(JSON.parse(String(mint[1]!.body))).toMatchObject({
      price_id: "price_rent",
    })
  })

  it("asks the host to sign the visitor in when it says they are signed out", async () => {
    const server = fakeBilling({ products: catalog() })
    const onSignInRequired = vi.fn()
    mount(
      <Offers
        entitlement="course:101"
        signedIn={false}
        onPaid={vi.fn()}
        onSignInRequired={onSignInRequired}
      />,
      server
    )
    fireEvent.click(await screen.findByRole("button", { name: "$4.99" }))

    expect(onSignInRequired).toHaveBeenCalledOnce()
    expect(server.calls).not.toContain("POST /me/checkout-sessions")
  })

  it("asks the host to sign the visitor in on a 401", async () => {
    const server = fakeBilling({ products: catalog() })
    server.fail["POST /me/checkout-sessions"] = apiError(
      401,
      "authentication_required"
    )
    const onSignInRequired = vi.fn()
    mount(
      <Offers
        entitlement="course:101"
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
    const server = fakeBilling({ products: catalog() })
    server.fail["POST /me/checkout-sessions"] = apiError(
      404,
      "price_not_found",
      "That price is no longer on sale."
    )
    const onSignInRequired = vi.fn()
    mount(
      <Offers
        entitlement="course:101"
        onPaid={vi.fn()}
        onSignInRequired={onSignInRequired}
      />,
      server
    )
    fireEvent.click(await screen.findByRole("button", { name: "$4.99" }))

    expect(await screen.findByRole("alert")).toBeInTheDocument()
    expect(onSignInRequired).not.toHaveBeenCalled()
  })

  it("offers the host's products without asking OpenRails", async () => {
    const server = fakeBilling({ products: [] })
    const products = catalog().slice(2) as unknown as Product[]
    mount(
      <Offers
        products={products}
        onPaid={vi.fn()}
        onSignInRequired={vi.fn()}
      />,
      server
    )

    expect(
      await screen.findByRole("button", { name: "$10.00 every 30 days" })
    ).toBeInTheDocument()
    expect(server.calls).not.toContain("GET /catalog/products")
  })

  it("says when nothing on sale unlocks the entitlement", async () => {
    const server = fakeBilling({ products: catalog() })
    mount(
      <Offers
        entitlement="course:999"
        onPaid={vi.fn()}
        onSignInRequired={vi.fn()}
      />,
      server
    )
    expect(
      await screen.findByText("Nothing on sale unlocks this")
    ).toBeInTheDocument()
  })
})
