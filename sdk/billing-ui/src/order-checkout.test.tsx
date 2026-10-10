import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "./client/client"
import { OrderCheckout } from "./order-checkout"
import { BillingProvider } from "./react/provider"

// Stripe.js in the page: its element reports a complete card,
// createPaymentMethod tokenizes it and handleNextAction answers a challenge.
const fakeStripe = vi.hoisted(() => ({
  elements: () => ({
    create: () => ({
      on: (_: string, handler: (event: { complete: boolean }) => void) =>
        handler({ complete: true }),
      mount: () => undefined,
      destroy: () => undefined,
    }),
    submit: async () => ({}),
  }),
  createPaymentMethod: async () => ({ paymentMethod: { id: "pm_entered" } }),
  handleNextAction: vi.fn(async () => ({})),
}))
vi.mock("#orck/lib/stripe", () => ({
  loadStripeFor: async () => fakeStripe,
}))

const order = (payment: Record<string, unknown>, status = "open") => ({
  id: "ord_1",
  customer_id: "c1",
  origin: "merchant",
  status,
  number: status === "complete" ? "000001" : null,
  currency: "USD",
  total: "15000000",
  lines: [
    {
      id: "oli_1",
      price_id: "price_1",
      product_id: "prod_1",
      description: "Lifetime",
      quantity: 1,
      unit_amount: "15000000",
      amount: "15000000",
      billing_interval_hours: null,
      access_duration_hours: null,
      subscription_id: null,
      product_access_id: null,
    },
  ],
  payment: {
    status: "requires_payment_method",
    next_action: null,
    last_payment_error: null,
    payment_method_id: null,
    payment_id: null,
    ...payment,
  },
  payment_options: [
    {
      psp_id: "psp_stripe",
      rail: "stripe",
      accepts: ["saved_card", "new_card"],
    },
  ],
  expires_at: "2026-11-09T00:00:00Z",
  completed_at: null,
  canceled_at: null,
  expired_at: null,
  created_at: "2026-10-10T00:00:00Z",
})

const config = {
  capabilities: { route_groups: {}, features: {} },
  payment: {
    psps: [
      {
        psp_id: "psp_stripe",
        key: "stripe",
        rail: "stripe",
        custodian: "psp",
        display_name: "Card",
        flow: "elements",
        config: { publishable_key: "pk_test_1" },
      },
    ],
  },
  currencies: [{ code: "USD", decimals: 6 }],
}

describe("OrderCheckout", () => {
  it("pays an order with a new card: a decline, a challenge, then complete", async () => {
    const pays: { body: unknown; key: string | null }[] = []
    const fetch = vi.fn(async (input: string, init: RequestInit) => {
      if (input.endsWith("/config")) return Response.json(config)
      if (
        input.endsWith("/me/payment-methods?limit=100") ||
        input.includes("/me/payment-methods")
      )
        return Response.json({ data: [], next_cursor: null })
      if (input.endsWith("/me/orders/ord_1")) return Response.json(order({}))
      if (input.endsWith("/me/orders/ord_1/pay")) {
        pays.push({
          body: JSON.parse(String(init.body)),
          key: new Headers(init.headers).get("Idempotency-Key"),
        })
        if (pays.length === 1)
          return Response.json(
            {
              error: {
                type: "card_error",
                code: "card_declined",
                message: "Your card has insufficient funds.",
                order: order({
                  last_payment_error: {
                    reason: "insufficient_funds",
                    message: "Your card has insufficient funds.",
                  },
                }),
              },
            },
            { status: 402 }
          )
        return Response.json(
          order({
            status: "requires_action",
            next_action: {
              type: "authenticate",
              psp_id: "psp_stripe",
              payload: {
                client_secret: "pi_1_secret_x",
                payment_intent_id: "pi_1",
              },
            },
          })
        )
      }
      if (input.endsWith("/me/orders/ord_1/confirm"))
        return Response.json(order({ status: "succeeded" }, "complete"))
      return Response.json({}, { status: 404 })
    })
    const onSettled = vi.fn()
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <OrderCheckout orderId="ord_1" onSettled={onSettled} />
      </BillingProvider>
    )
    const pay = await screen.findByRole("button", { name: "Pay $15.00" })
    fireEvent.click(pay)
    expect(
      await screen.findByText("Your card has insufficient funds.")
    ).toBeInTheDocument()
    fireEvent.click(screen.getByRole("button", { name: "Pay $15.00" }))
    await waitFor(() => expect(onSettled).toHaveBeenCalled())
    expect(onSettled.mock.calls[0][0]).toMatchObject({ status: "complete" })
    expect(fakeStripe.handleNextAction).toHaveBeenCalledWith({
      clientSecret: "pi_1_secret_x",
    })
    expect(pays.map((p) => p.body)).toEqual([
      {
        payment: { token: "pm_entered", psp_id: "psp_stripe" },
        expected_total: "15000000",
      },
      {
        payment: { token: "pm_entered", psp_id: "psp_stripe" },
        expected_total: "15000000",
      },
    ])
    expect(pays[0].key).toBeTruthy()
    expect(pays[1].key).not.toEqual(pays[0].key)
  })

  it("offers only the saved cards kept for one-click buys", async () => {
    const method = (id: string, last4: string, reusable: boolean) => ({
      id,
      customer_id: "c1",
      rail: "stripe",
      psp_id: "psp_stripe",
      status: "active",
      card: { brand: "visa", last4, exp_month: 12, exp_year: 2035 },
      reusable,
    })
    const fetch = vi.fn(async (input: string) => {
      if (input.endsWith("/config")) return Response.json(config)
      if (input.includes("/me/payment-methods"))
        return Response.json({
          data: [
            method("pm_kept", "4242", true),
            method("pm_sub", "1881", false),
          ],
          next_cursor: null,
        })
      if (input.endsWith("/me/orders/ord_1")) return Response.json(order({}))
      return Response.json({}, { status: 404 })
    })
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <OrderCheckout orderId="ord_1" />
      </BillingProvider>
    )
    expect((await screen.findAllByText(/4242/)).length).toBeGreaterThan(0)
    expect(screen.queryByText(/1881/)).not.toBeInTheDocument()
  })
})
