import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { Cart } from "./cart"
import { createBillingClient } from "./client/client"
import { BillingProvider } from "./react/provider"

const line = (priceId: string, description: string, refusal?: object) => ({
  price_id: priceId,
  product_id: `prod_${priceId}`,
  description,
  quantity: 1,
  unit_amount: "5000000",
  amount: "5000000",
  ownership: "unique",
  billing_interval_hours: null,
  access_duration_hours: null,
  refusal: refusal ?? null,
})

const cart = (lines: ReturnType<typeof line>[]) => ({
  currency: "USD",
  total: String(lines.filter((l) => !l.refusal).length * 5_000_000),
  lines,
  payment_options: [],
  last_changed_at: "2026-10-10T00:00:00Z",
  expires_at: "2026-11-09T00:00:00Z",
})

const config = {
  capabilities: { route_groups: {}, features: {} },
  payment: { psps: [] },
  currencies: [{ code: "USD", decimals: 6 }],
}

describe("Cart", () => {
  it("removes a refused line, then checks the cart out at its total", async () => {
    const sent: {
      method: string
      url: string
      body: unknown
      key: string | null
    }[] = []
    let lines = [
      line("price_book", "Book"),
      line("price_owned", "Owned course", {
        code: "already_owned",
        message: "You already own this.",
      }),
    ]
    const fetch = vi.fn(async (input: string, init: RequestInit) => {
      sent.push({
        method: init.method ?? "GET",
        url: input,
        body: init.body ? JSON.parse(String(init.body)) : undefined,
        key: new Headers(init.headers).get("Idempotency-Key"),
      })
      if (input.endsWith("/config")) return Response.json(config)
      if (input.endsWith("/me/cart/lines/price_owned")) {
        lines = lines.filter((l) => l.price_id !== "price_owned")
        return Response.json(cart(lines))
      }
      if (input.endsWith("/me/cart")) return Response.json(cart(lines))
      if (input.endsWith("/me/orders"))
        return Response.json(
          {
            id: "ord_1",
            customer_id: "c1",
            origin: "customer",
            status: "open",
            number: null,
            currency: "USD",
            total: "5000000",
            lines: [],
            payment: {
              status: "requires_payment_method",
              next_action: null,
              last_payment_error: null,
              payment_method_id: null,
              payment_id: null,
            },
            payment_options: [],
            expires_at: "2026-10-11T00:00:00Z",
            completed_at: null,
            canceled_at: null,
            expired_at: null,
            created_at: "2026-10-10T00:00:00Z",
          },
          { status: 201 }
        )
      return Response.json({}, { status: 404 })
    })
    const onOrder = vi.fn()
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <Cart onOrder={onOrder} />
      </BillingProvider>
    )
    expect(await screen.findByText("You already own this.")).toBeInTheDocument()
    const checkout = screen.getByRole("button", { name: "Check out" })
    expect(checkout).toBeDisabled()

    fireEvent.click(screen.getAllByRole("button", { name: "Remove" })[1])
    await waitFor(() =>
      expect(screen.queryByText("Owned course")).not.toBeInTheDocument()
    )
    expect(sent.find((s) => s.method === "PUT")?.body).toEqual({ quantity: 0 })

    fireEvent.click(screen.getByRole("button", { name: "Check out" }))
    await waitFor(() => expect(onOrder).toHaveBeenCalled())
    expect(onOrder.mock.calls[0][0]).toMatchObject({ id: "ord_1" })
    const created = sent.find((s) => s.url.endsWith("/me/orders"))
    expect(created?.body).toEqual({ cart: true, expected_total: "5000000" })
    expect(created?.key).toBeTruthy()
  })
})
