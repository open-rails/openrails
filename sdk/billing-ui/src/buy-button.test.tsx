import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import { BuyButton } from "./buy-button"
import { createBillingClient } from "./client/client"
import type { Order, Price } from "./client/types"
import type { EmbeddedModalProps, OrderModalProps } from "./modal"
import { BillingProvider } from "./react/provider"
import {
  apiError,
  fakeBilling,
  json,
  order,
  price,
  product,
  type FakeBilling,
} from "./test/billing-server"

// Paying is OrderCheckout's (order-checkout.test.tsx) and the checkout host's
// (EmbeddedCheckout); here the dialogs record what BuyButton hands them.
const modals = vi.hoisted(() => [] as OrderModalProps[])
const frames = vi.hoisted(() => [] as EmbeddedModalProps[])
vi.mock("./modal", () => ({
  OrderModal: (props: OrderModalProps) => {
    modals.push(props)
    return <div role="dialog" />
  },
  EmbeddedModal: (props: EmbeddedModalProps) => {
    frames.push(props)
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
  }) as unknown as { prices: Price[] }

// The host page's prices of the course: buy and rent.
const purchase = () => course().prices[0]!
const rent = () => course().prices[1]!

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
  it("labels itself from the host's price, reading no catalog", async () => {
    const server = fakeBilling()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton price={rent()} onPaid={vi.fn()} onSignInRequired={vi.fn()} />
      )
    )
    expect(
      await screen.findByRole("button", { name: "Rent for 3 days, $1.99" })
    ).toBeEnabled()
    expect(server.calls.filter((c) => c.includes("product"))).toEqual([])
  })

  it("asks the host to sign in, without an order, when it says the visitor is signed out", async () => {
    const server = fakeBilling()
    const onSignInRequired = vi.fn()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          price={rent()}
          label="Rent"
          signedIn={false}
          onPaid={vi.fn()}
          onSignInRequired={onSignInRequired}
        />
      )
    )
    fireEvent.click(screen.getByRole("button", { name: "Rent" }))
    expect(onSignInRequired).toHaveBeenCalledOnce()
    expect(server.calls).not.toContain("POST /me/orders")
  })

  it("asks the host to sign in on a 401", async () => {
    const server = fakeBilling()
    server.fail["POST /me/orders"] = apiError(401, "authentication_required")
    const onSignInRequired = vi.fn()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          price={rent()}
          label="Rent"
          onPaid={vi.fn()}
          onSignInRequired={onSignInRequired}
        />
      )
    )
    fireEvent.click(await enabled("Rent"))
    await waitFor(() => expect(onSignInRequired).toHaveBeenCalledOnce())
    expect(screen.queryByRole("dialog")).toBeNull()
  })

  it("creates the customer's order and calls onPaid once it is complete", async () => {
    modals.length = 0
    const server = fakeBilling()
    const onPaid = vi.fn()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          price={rent()}
          label="Rent"
          returnUrl="https://app.test/back"
          onPaid={onPaid}
          onSignInRequired={vi.fn()}
        />
      )
    )
    fireEvent.click(await enabled("Rent"))
    await screen.findByRole("dialog")
    const create = server.fetch.mock.calls.find(
      ([, init]) => init?.method === "POST"
    )!
    expect(String(create[0])).toContain("/me/orders")
    expect(JSON.parse(String(create[1]!.body))).toEqual({
      lines: [{ price_id: "price_rent" }],
    })
    const modal = modals.at(-1)!
    expect(modal.orderId).toBe("ord_price_rent")
    expect(modal.returnUrl).toBe("https://app.test/back")

    const paid = (status: string) => order({ status }) as unknown as Order
    modal.onSettled!(paid("processing"))
    expect(onPaid).not.toHaveBeenCalled()
    modal.onSettled!(paid("complete"))
    expect(onPaid).toHaveBeenCalledOnce()
  })

  it("buys a customer-chosen deposit for exactly its amount", async () => {
    const deposit = product({
      id: "prod_credit",
      key: "api-credits",
      prices: [
        price(null, null, {
          id: "price_deposit",
          key: "deposit",
          product_id: "prod_credit",
          unit_amount: "0",
        }),
      ],
    }) as unknown as { prices: Price[] }
    const server = fakeBilling()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          price={deposit.prices[0]!}
          amount="100000000"
          label="Add credits"
          onPaid={vi.fn()}
          onSignInRequired={vi.fn()}
        />
      )
    )
    fireEvent.click(await enabled("Add credits"))
    await screen.findByRole("dialog")
    const create = server.fetch.mock.calls.find(
      ([, init]) => init?.method === "POST"
    )!
    expect(JSON.parse(String(create[1]!.body))).toEqual({
      lines: [{ price_id: "price_deposit", amount: "100000000" }],
    })
  })

  it("resumes the unpaid order that already holds the purchase", async () => {
    modals.length = 0
    const server = fakeBilling()
    server.fail["POST /me/orders"] = json(409, {
      error: {
        type: "invalid_request_error",
        code: "already_owned",
        message: "Another purchase of this awaits its payment.",
        metadata: { owned_by: "ord_open", hint: "resume" },
      },
    })
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          price={purchase()}
          label="Buy"
          onPaid={vi.fn()}
          onSignInRequired={vi.fn()}
        />
      )
    )
    fireEvent.click(await enabled("Buy"))
    await screen.findByRole("dialog")
    expect(modals.at(-1)!.orderId).toBe("ord_open")
  })

  it("answers a second click with the same order", async () => {
    const server = fakeBilling()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          price={rent()}
          label="Rent"
          onPaid={vi.fn()}
          onSignInRequired={vi.fn()}
        />
      )
    )
    fireEvent.click(await enabled("Rent"))
    await screen.findByRole("dialog")
    modals.at(-1)!.onOpenChange(false)
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
    fireEvent.click(await enabled("Rent"))
    await screen.findByRole("dialog")
    const keys = server.fetch.mock.calls
      .filter(([, init]) => init?.method === "POST")
      .map(([, init]) => new Headers(init!.headers).get("Idempotency-Key"))
    expect(keys).toHaveLength(2)
    expect(keys[0]).toBe(keys[1])
  })

  it("frames the checkout host on a site the merchant registered to frame it", async () => {
    frames.length = 0
    const server = fakeBilling({ embedOrigins: [window.location.origin] })
    const onPaid = vi.fn()
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          price={rent()}
          label="Rent"
          onPaid={onPaid}
          onSignInRequired={vi.fn()}
        />
      )
    )
    fireEvent.click(await enabled("Rent"))
    await screen.findByRole("dialog")
    // The merchant's origins decide, before the order: one call makes both.
    expect(server.calls.indexOf("GET /me/config")).toBeGreaterThanOrEqual(0)
    expect(server.calls.indexOf("GET /me/config")).toBeLessThan(
      server.calls.indexOf("POST /me/orders")
    )
    expect(server.calls.filter((c) => c.startsWith("POST"))).toEqual([
      "POST /me/orders",
    ])
    const create = server.fetch.mock.calls.find(
      ([, init]) => init?.method === "POST"
    )!
    expect(JSON.parse(String(create[1]!.body))).toEqual({
      lines: [{ price_id: "price_rent" }],
      checkout: { ui_mode: "embedded" },
    })
    const frame = frames.at(-1)!
    expect(frame.url).toBe("https://pay.test/c/ord_price_rent#cks_test")

    // The frame's word is a hint: the order says whether it is paid.
    frame.onComplete!({ orderId: "ord_price_rent", successUrl: "" })
    await waitFor(() =>
      expect(server.calls).toContain("GET /me/orders/ord_price_rent")
    )
    expect(onPaid).not.toHaveBeenCalled()
    server.orderStatus = "complete"
    frame.onComplete!({ orderId: "ord_price_rent", successUrl: "" })
    await waitFor(() => expect(onPaid).toHaveBeenCalledOnce())
  })

  it("pays on the page anywhere else", async () => {
    modals.length = 0
    frames.length = 0
    const server = fakeBilling({ embedOrigins: ["https://elsewhere.test"] })
    const { tree, rerender } = mount(server)
    rerender(
      tree(
        <BuyButton
          price={rent()}
          label="Rent"
          onPaid={vi.fn()}
          onSignInRequired={vi.fn()}
        />
      )
    )
    fireEvent.click(await enabled("Rent"))
    await screen.findByRole("dialog")
    expect(modals.at(-1)!.orderId).toBe("ord_price_rent")
    expect(frames).toHaveLength(0)
    const create = server.fetch.mock.calls.find(
      ([, init]) => init?.method === "POST"
    )!
    expect(JSON.parse(String(create[1]!.body))).not.toHaveProperty("checkout")
  })
})

// The button, once enabled.
async function enabled(name: string) {
  const button = await screen.findByRole("button", { name })
  await waitFor(() => expect(button).toBeEnabled())
  return button
}
