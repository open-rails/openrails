import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import { BuyButton } from "./buy-button"
import { createBillingClient } from "./client/client"
import type { CheckoutModalProps } from "./modal"
import { BillingProvider } from "./react/provider"
import {
  apiError,
  fakeBilling,
  price,
  product,
  type FakeBilling,
} from "./test/billing-server"

// The checkout itself is Checkout's (checkout.test.tsx); here it records what
// BuyButton hands it.
const modals = vi.hoisted(() => [] as CheckoutModalProps[])
vi.mock("./modal", () => ({
  CheckoutModal: (props: CheckoutModalProps) => {
    modals.push(props)
    return <div role="dialog" />
  },
}))

const course = () =>
  product({
    id: "prod_101",
    key: "course-101",
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
  })

function mount(server: FakeBilling) {
  const client = createBillingClient({ fetch: server.fetch })
  const tree = (ui: ReactNode) => (
    <BillingProvider client={client} locale="en-US">
      {ui}
    </BillingProvider>
  )
  return { tree, ...render(<></>) }
}

describe("BuyButton", () => {
  it("labels itself from the catalog", async () => {
    const server = fakeBilling({ products: [course()] })
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          product="course-101"
          price="rent"
          onPaid={vi.fn()}
          onSignInRequired={vi.fn()}
        />
      )
    )
    expect(
      await screen.findByRole("button", { name: "Rent for 3 days, $1.99" })
    ).toBeEnabled()
    expect(server.calls).toContain("GET /catalog/products")
  })

  it("asks the host to sign in, without a request, when it says the visitor is signed out", async () => {
    const server = fakeBilling({ products: [] })
    const onSignInRequired = vi.fn()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          product="course-101"
          price="rent"
          label="Rent"
          signedIn={false}
          onPaid={vi.fn()}
          onSignInRequired={onSignInRequired}
        />
      )
    )
    fireEvent.click(screen.getByRole("button", { name: "Rent" }))
    expect(onSignInRequired).toHaveBeenCalledOnce()
    expect(server.calls).toEqual([])
  })

  it("asks the host to sign in on a 401", async () => {
    const server = fakeBilling({ products: [] })
    server.fail["POST /me/checkout-sessions"] = apiError(
      401,
      "authentication_required"
    )
    const onSignInRequired = vi.fn()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          product="course-101"
          price="rent"
          label="Rent"
          onPaid={vi.fn()}
          onSignInRequired={onSignInRequired}
        />
      )
    )
    fireEvent.click(screen.getByRole("button", { name: "Rent" }))
    await waitFor(() => expect(onSignInRequired).toHaveBeenCalledOnce())
    expect(screen.queryByRole("dialog")).toBeNull()
  })

  it("checks out on the customer's surface and calls onPaid only on success", async () => {
    modals.length = 0
    const server = fakeBilling({ products: [] })
    const onPaid = vi.fn()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          product="course-101"
          price="rent"
          label="Rent"
          onPaid={onPaid}
          onSignInRequired={vi.fn()}
        />
      )
    )
    fireEvent.click(screen.getByRole("button", { name: "Rent" }))
    await screen.findByRole("dialog")
    const mint = server.fetch.mock.calls.find(
      ([, init]) => init?.method === "POST"
    )!
    expect(JSON.parse(String(mint[1]!.body))).toMatchObject({
      product_key: "course-101",
      price_key: "rent",
    })

    // The source reads the session as the signed-in customer.
    await modals.at(-1)!.source.getSession()
    expect(server.calls).toContain("GET /me/checkout-sessions/ocs_rent")

    modals.at(-1)!.onComplete!({ status: "requires_action" })
    expect(onPaid).not.toHaveBeenCalled()
    modals.at(-1)!.onComplete!({ status: "succeeded" })
    expect(onPaid).toHaveBeenCalledExactlyOnceWith({ status: "succeeded" })
  })

  it("keeps one checkout source while the tree re-renders", async () => {
    modals.length = 0
    const server = fakeBilling({ products: [] })
    const { tree, rerender } = mount(server)
    const button = (onPaid: () => void) =>
      tree(
        <BuyButton
          product="course-101"
          price="rent"
          label="Rent"
          onPaid={onPaid}
          onSignInRequired={vi.fn()}
        />
      )
    rerender(button(vi.fn()))
    fireEvent.click(screen.getByRole("button", { name: "Rent" }))
    await screen.findByRole("dialog")
    rerender(button(vi.fn()))
    rerender(button(vi.fn()))
    expect(modals.length).toBeGreaterThan(1)
    expect(new Set(modals.map((m) => m.source)).size).toBe(1)
  })
})
