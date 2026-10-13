import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "./client/client"
import { OrderPayment } from "./order-payment"
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
  collection_owner: "engine",
  collection_psp_id: null,
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

// The deployment's configuration, and the customer's merchant's.
const deployment = {
  capabilities: { route_groups: {}, features: {} },
  currencies: [{ code: "USD", decimals: 6 }],
}
const config = {
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
  merchant: { display_name: "Shop" },
}

describe("OrderPayment", () => {
  it.each(["provider", "nmi_schedule"])(
    "shows %s bills without another collector or confirmation",
    async (collectionOwner) => {
      fakeStripe.handleNextAction.mockClear()
      const fetch = vi.fn(async (input: string) => {
        if (input.endsWith("/me/config")) return Response.json(config)
        if (input.endsWith("/config")) return Response.json(deployment)
        if (input.endsWith("/me/orders/ord_1"))
          return Response.json({
            ...order({
              status: "requires_action",
              next_action: {
                type: "authenticate",
                psp_id: "psp_stripe",
                payload: { client_secret: "must_not_use" },
              },
            }),
            collection_owner: collectionOwner,
          })
        return Response.json({}, { status: 404 })
      })
      render(
        <BillingProvider client={createBillingClient({ fetch })}>
          <OrderPayment orderId="ord_1" />
        </BillingProvider>
      )
      expect(await screen.findByText(/This bill is collected by the payment provider/)).toBeTruthy()
      expect(screen.queryByRole("button", { name: /Pay |Confirm payment/ })).toBeNull()
      expect(fakeStripe.handleNextAction).not.toHaveBeenCalled()
      expect(fetch.mock.calls.some(([path]) => /payment-methods|\/pay$|\/confirm$/.test(path))).toBe(false)
    }
  )

  it("pays from the host's documents without reading them", async () => {
    const fetch = vi.fn(async (input: string) => {
      if (input.includes("/me/payment-methods"))
        return Response.json({ data: [], next_cursor: null })
      if (input.endsWith("/me/orders/ord_1")) return Response.json(order({}))
      return Response.json({}, { status: 404 })
    })
    render(
      <BillingProvider
        client={createBillingClient({ fetch })}
        config={
          { ...deployment, rails: [], captcha: null, network: null } as never
        }
        customerConfig={config as never}
      >
        <OrderPayment orderId="ord_1" />
      </BillingProvider>
    )
    expect(
      await screen.findByRole("button", { name: "Pay $15.00" })
    ).toBeTruthy()
    expect(fetch.mock.calls.filter(([path]) => /\/config$/.test(path))).toEqual(
      []
    )
  })

  it("has the customer authenticate a renewal their bank challenged", async () => {
    const renewal = (payment: Record<string, unknown>, status = "open") => ({
      ...order(payment, status),
      origin: "subscription",
      expires_at: null,
      renewal: {
        subscription_id: "sub_1",
        period_starts_at: "2026-10-10T00:00:00Z",
        period_ends_at: "2026-11-09T00:00:00Z",
        due_at: "2026-10-10T00:00:00Z",
      },
      payment_options: [
        { psp_id: "psp_stripe", rail: "stripe", accepts: ["saved_card"] },
      ],
    })
    fakeStripe.handleNextAction.mockClear()
    const fetch = vi.fn(async (input: string) => {
      if (input.endsWith("/me/config")) return Response.json(config)
      if (input.endsWith("/config")) return Response.json(deployment)
      if (input.includes("/me/payment-methods"))
        return Response.json({ data: [], next_cursor: null })
      if (input.endsWith("/me/orders/ord_1"))
        return Response.json(
          renewal({
            status: "requires_action",
            next_action: {
              type: "authenticate",
              psp_id: "psp_stripe",
              payload: {
                client_secret: "pi_2_secret_x",
                payment_intent_id: "pi_2",
              },
            },
          })
        )
      if (input.endsWith("/me/orders/ord_1/confirm"))
        return Response.json(renewal({ status: "succeeded" }, "complete"))
      return Response.json({}, { status: 404 })
    })
    const onSettled = vi.fn()
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <OrderPayment orderId="ord_1" onSettled={onSettled} />
      </BillingProvider>
    )
    fireEvent.click(
      await screen.findByRole("button", { name: "Confirm payment" })
    )
    await waitFor(() => expect(onSettled).toHaveBeenCalled(), {
      timeout: 5000,
    })
    expect(fakeStripe.handleNextAction).toHaveBeenCalledWith({
      clientSecret: "pi_2_secret_x",
    })
    expect(onSettled.mock.calls[0][0]).toMatchObject({ status: "complete" })
  })

  it("pays an order with a new card: a decline, a challenge, then complete", async () => {
    const pays: { body: unknown; key: string | null }[] = []
    const fetch = vi.fn(async (input: string, init: RequestInit) => {
      if (input.endsWith("/me/config")) return Response.json(config)
      if (input.endsWith("/config")) return Response.json(deployment)
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
        <OrderPayment orderId="ord_1" onSettled={onSettled} />
      </BillingProvider>
    )
    const pay = await screen.findByRole("button", { name: "Pay $15.00" })
    fireEvent.click(pay)
    expect(
      await screen.findByText(
        "Your card has insufficient funds.",
        {},
        { timeout: 5000 }
      )
    ).toBeInTheDocument()
    fireEvent.click(screen.getByRole("button", { name: "Pay $15.00" }))
    await waitFor(() => expect(onSettled).toHaveBeenCalled(), {
      timeout: 5000,
    })
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
      if (input.endsWith("/me/config")) return Response.json(config)
      if (input.endsWith("/config")) return Response.json(deployment)
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
        <OrderPayment orderId="ord_1" />
      </BillingProvider>
    )
    expect((await screen.findAllByText(/4242/)).length).toBeGreaterThan(0)
    expect(screen.queryByText(/1881/)).not.toBeInTheDocument()
  })

  it("subscribes on an order the merchant's server created, with a saved card", async () => {
    const membership = (status = "open") => {
      const base = order(
        status === "complete"
          ? { status: "succeeded", payment_method_id: "pm_kept", payment_id: "pay_1" }
          : {},
        status
      )
      return {
        ...base,
        lines: [
          {
            ...base.lines[0],
            description: "Pro",
            quantity: null,
            billing_interval_hours: 720,
            access_duration_hours: 720,
            subscription_id: status === "complete" ? "sub_1" : null,
          },
        ],
      }
    }
    const paid: Record<string, unknown>[] = []
    const fetch = vi.fn(async (input: string, init?: RequestInit) => {
      if (input.endsWith("/me/config")) return Response.json(config)
      if (input.endsWith("/config")) return Response.json(deployment)
      if (input.includes("/me/payment-methods"))
        return Response.json({
          data: [
            {
              id: "pm_kept",
              customer_id: "c1",
              rail: "stripe",
              psp_id: "psp_stripe",
              status: "active",
              card: { brand: "visa", last4: "4242", exp_month: 12, exp_year: 2035 },
              reusable: true,
            },
          ],
          next_cursor: null,
        })
      if (input.endsWith("/me/orders/ord_1")) return Response.json(membership())
      if (input.endsWith("/me/orders/ord_1/pay")) {
        paid.push(JSON.parse(String(init?.body)))
        return Response.json(membership("complete"))
      }
      return Response.json({}, { status: 404 })
    })
    const onSettled = vi.fn()
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <OrderPayment orderId="ord_1" onSettled={onSettled} />
      </BillingProvider>
    )
    fireEvent.click(await screen.findByRole("radio", { name: /4242/ }))
    fireEvent.click(await screen.findByRole("button", { name: "Pay $15.00" }))
    await waitFor(() => expect(onSettled).toHaveBeenCalled())
    expect(paid).toEqual([
      expect.objectContaining({ payment: { payment_method_id: "pm_kept" }, expected_total: "15000000" }),
    ])
    expect(onSettled.mock.calls[0][0].lines[0].subscription_id).toBe("sub_1")
  })

  it("pays what a usage order still owes with a saved card", async () => {
    const usage = (status = "open") => ({
      ...order({ status: status === "complete" ? "succeeded" : "requires_payment_method" }, status),
      origin: "usage",
      expires_at: null,
      usage: {
        period_starts_at: "2026-09-01T00:00:00Z",
        period_ends_at: "2026-10-01T00:00:00Z",
        due_at: "2026-10-01T00:00:00Z",
        amount_paid: "5000000",
        amount_due: status === "complete" ? "0" : "10000000",
        collection_method: "charge_automatically",
        delinquent: false,
      },
      payment_options: [
        { psp_id: "psp_stripe", rail: "stripe", accepts: ["saved_card"] },
      ],
    })
    const paid: Record<string, unknown>[] = []
    const fetch = vi.fn(async (input: string, init?: RequestInit) => {
      if (input.endsWith("/me/config")) return Response.json(config)
      if (input.endsWith("/config")) return Response.json(deployment)
      if (input.includes("/me/payment-methods"))
        return Response.json({
          data: [
            {
              id: "pm_kept",
              customer_id: "c1",
              rail: "stripe",
              psp_id: "psp_stripe",
              status: "active",
              card: { brand: "visa", last4: "4242", exp_month: 12, exp_year: 2035 },
              reusable: true,
            },
          ],
          next_cursor: null,
        })
      if (input.endsWith("/me/orders/ord_1")) return Response.json(usage())
      if (input.endsWith("/me/orders/ord_1/pay")) {
        paid.push(JSON.parse(String(init?.body)))
        return Response.json(usage("complete"))
      }
      return Response.json({}, { status: 404 })
    })
    const onSettled = vi.fn()
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <OrderPayment orderId="ord_1" onSettled={onSettled} />
      </BillingProvider>
    )
    expect(await screen.findByText("Due")).toBeInTheDocument()
    fireEvent.click(await screen.findByRole("radio", { name: /4242/ }))
    fireEvent.click(await screen.findByRole("button", { name: "Pay $10.00" }))
    await waitFor(() => expect(onSettled).toHaveBeenCalled())
    expect(paid).toEqual([
      expect.objectContaining({ payment: { payment_method_id: "pm_kept" }, expected_total: "15000000" }),
    ])
  })

  it("pays from a Solana wallet: a Solana Pay QR for the order", async () => {
    const solanaConfig = {
      ...config,
      payment: {
        psps: [
          {
            psp_id: "psp_sol",
            key: "solana",
            rail: "solana",
            custodian: "self",
            display_name: "Solana",
            flow: "wallet",
            config: {},
          },
        ],
        solana: {
          network: "devnet",
          chain: "solana:devnet",
          tokens: [{ symbol: "USDC", mint: "mint_usdc", decimals: 6 }],
        },
      },
    }
    const solanaOrder = (payment: Record<string, unknown>) => ({
      ...order(payment),
      payment_options: [
        { psp_id: "psp_sol", rail: "solana", accepts: ["solana_pay"] },
      ],
    })
    const pays: unknown[] = []
    const fetch = vi.fn(async (input: string, init: RequestInit) => {
      if (input.endsWith("/me/config")) return Response.json(solanaConfig)
      if (input.endsWith("/config")) return Response.json(deployment)
      if (input.includes("/me/payment-methods"))
        return Response.json({ data: [], next_cursor: null })
      if (input.endsWith("/me/orders/ord_1"))
        return Response.json(solanaOrder({}))
      if (input.endsWith("/me/orders/ord_1/pay")) {
        pays.push(JSON.parse(String(init.body)))
        return Response.json(
          solanaOrder({
            status: "requires_action",
            next_action: {
              type: "solana_pay",
              url: "solana:https://billing.test/v1/orders/ord_1/solana-pay",
              psp_id: "psp_sol",
              payload: { token_symbol: "USDC", token_amount: "15.000000" },
            },
          })
        )
      }
      return Response.json({}, { status: 404 })
    })
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <OrderPayment orderId="ord_1" />
      </BillingProvider>
    )
    fireEvent.click(
      await screen.findByRole("button", { name: "Pay with USDC on Solana" })
    )
    expect(
      await screen.findByRole("img", { name: "Solana Pay QR code" })
    ).toBeInTheDocument()
    expect(screen.getByText(/15.000000 USDC/)).toBeInTheDocument()
    expect(
      screen.getByRole("link", { name: "Open in wallet" })
    ).toHaveAttribute(
      "href",
      "solana:https://billing.test/v1/orders/ord_1/solana-pay"
    )
    expect(pays).toEqual([
      {
        payment: { token_symbol: "USDC", psp_id: "psp_sol" },
        expected_total: "15000000",
      },
    ])
  })

  it("pays on the PSP's hosted page when it has no fields here", async () => {
    const hostedConfig = {
      ...config,
      payment: {
        psps: [
          {
            psp_id: "psp_stripe",
            key: "stripe",
            rail: "stripe",
            custodian: "psp",
            display_name: "Stripe",
            flow: "redirect",
            config: {},
          },
        ],
      },
    }
    const hostedOrder = (payment: Record<string, unknown>) => ({
      ...order(payment),
      payment_options: [
        {
          psp_id: "psp_stripe",
          rail: "stripe",
          accepts: ["saved_card", "new_card", "redirect"],
        },
      ],
    })
    const pays: unknown[] = []
    const fetch = vi.fn(async (input: string, init: RequestInit) => {
      if (input.endsWith("/me/config")) return Response.json(hostedConfig)
      if (input.endsWith("/config")) return Response.json(deployment)
      if (input.includes("/me/payment-methods"))
        return Response.json({ data: [], next_cursor: null })
      if (input.endsWith("/me/orders/ord_1"))
        return Response.json(hostedOrder({}))
      if (input.endsWith("/me/orders/ord_1/pay")) {
        pays.push(JSON.parse(String(init.body)))
        return Response.json(hostedOrder({}))
      }
      return Response.json({}, { status: 404 })
    })
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <OrderPayment orderId="ord_1" returnUrl="https://shop.test/done" />
      </BillingProvider>
    )
    fireEvent.click(
      await screen.findByRole("button", { name: "Pay $15.00 with Stripe" })
    )
    await waitFor(() => expect(pays).toHaveLength(1))
    expect(pays[0]).toEqual({
      payment: { return_url: "https://shop.test/done", psp_id: "psp_stripe" },
      expected_total: "15000000",
    })
  })

  describe("fallbacks", () => {
    const fallbackConfig = {
      ...config,
      payment: {
        psps: [
          ...config.payment.psps,
          {
            psp_id: "psp_ccbill",
            key: "ccbill",
            rail: "ccbill",
            custodian: "psp",
            display_name: "CCBill",
            flow: "redirect",
          },
        ],
      },
    }
    const card = (failed: number) => ({
      psp_id: "psp_stripe",
      rail: "stripe",
      accepts: ["saved_card", "new_card"],
      failed_attempts: failed,
    })
    const ccbill = {
      psp_id: "psp_ccbill",
      rail: "ccbill",
      accepts: ["redirect"],
      failed_attempts: 0,
    }
    const fallbacks = { ccbill: { for: ["stripe"], afterFailures: 2 } }
    const flexForm =
      "https://sandbox-api.ccbill.com/wap-frontflex/flexforms/flex-1?reservationId=oat_1"

    // The order as OpenRails answers it, with its options, and every pay.
    const server = (options: () => unknown[]) => {
      const pays: { payment: Record<string, unknown> }[] = []
      let failed = 0
      const fetch = vi.fn(async (input: string, init: RequestInit) => {
        if (input.endsWith("/me/config")) return Response.json(fallbackConfig)
        if (input.endsWith("/config")) return Response.json(deployment)
        if (input.includes("/me/payment-methods"))
          return Response.json({ data: [], next_cursor: null })
        if (input.endsWith("/me/orders/ord_1"))
          return Response.json({ ...order({}), payment_options: options() })
        if (input.endsWith("/me/orders/ord_1/pay")) {
          const body = JSON.parse(String(init.body))
          pays.push(body)
          if (body.payment.psp_id === "psp_stripe") {
            failed++
            return Response.json(
              {
                error: {
                  type: "card_error",
                  code: "card_declined",
                  message: "Your card was declined.",
                  order: {
                    ...order({
                      last_payment_error: {
                        reason: "generic_decline",
                        message: "Your card was declined.",
                      },
                    }),
                    payment_options: [card(failed), ccbill],
                  },
                },
              },
              { status: 402 }
            )
          }
          return Response.json({
            ...order({
              status: "requires_action",
              next_action: {
                type: "redirect_to_url",
                url: flexForm,
                psp_id: "psp_ccbill",
                payload: {},
              },
            }),
            payment_options: [card(failed), ccbill],
          })
        }
        return Response.json({}, { status: 404 })
      })
      return { fetch, pays }
    }

    it("keeps CCBill back until the card fails afterFailures times, then continues on its page", async () => {
      const { fetch, pays } = server(() => [card(0), ccbill])
      const assign = vi.fn()
      const location = window.location
      Object.defineProperty(window, "location", {
        configurable: true,
        value: { ...location, href: "https://shop.test/pay", assign },
      })
      try {
        render(
          <BillingProvider client={createBillingClient({ fetch })}>
            <OrderPayment
              orderId="ord_1"
              returnUrl="https://shop.test/done"
              fallbacks={fallbacks}
            />
          </BillingProvider>
        )
        const pay = await screen.findByRole("button", { name: "Pay $15.00" })
        expect(
          screen.queryByRole("button", { name: /CCBill/ })
        ).not.toBeInTheDocument()

        fireEvent.click(pay)
        expect(
          await screen.findByText(
            "Your card was declined.",
            {},
            { timeout: 5000 }
          )
        ).toBeInTheDocument()
        expect(
          screen.queryByText("Card payment isn't working?")
        ).not.toBeInTheDocument()
        expect(
          screen.queryByRole("button", { name: /CCBill/ })
        ).not.toBeInTheDocument()

        fireEvent.click(screen.getByRole("button", { name: "Pay $15.00" }))
        expect(
          await screen.findByText(
            "Card payment isn't working?",
            {},
            { timeout: 5000 }
          )
        ).toBeInTheDocument()
        expect(
          screen.queryByRole("button", { name: "Pay $15.00 with CCBill" })
        ).not.toBeInTheDocument()
        fireEvent.click(screen.getByRole("button", { name: "Pay with CCBill" }))
        await waitFor(() => expect(assign).toHaveBeenCalledWith(flexForm))
        expect(pays.map((p) => p.payment.psp_id)).toEqual([
          "psp_stripe",
          "psp_stripe",
          "psp_ccbill",
        ])
        expect(pays[2].payment).toEqual({
          return_url: "https://shop.test/done",
          psp_id: "psp_ccbill",
        })
        fireEvent.click(
          await screen.findByRole("button", { name: "Continue to CCBill" })
        )
        expect(assign).toHaveBeenLastCalledWith(flexForm)
      } finally {
        Object.defineProperty(window, "location", {
          configurable: true,
          value: location,
        })
      }
    })

    it.each([
      ["no PSP it stands in for is offered", () => [ccbill]],
      ["the order already failed often enough", () => [card(2), ccbill]],
    ])("offers CCBill at once when %s", async (_, options) => {
      const { fetch } = server(options)
      render(
        <BillingProvider client={createBillingClient({ fetch })}>
          <OrderPayment orderId="ord_1" fallbacks={fallbacks} />
        </BillingProvider>
      )
      expect(
        await screen.findByText("Card payment isn't working?")
      ).toBeInTheDocument()
      expect(
        screen.getByRole("button", { name: "Pay with CCBill" })
      ).toBeInTheDocument()
    })

    it("offers every PSP as itself without fallbacks", async () => {
      const { fetch } = server(() => [card(0), ccbill])
      render(
        <BillingProvider client={createBillingClient({ fetch })}>
          <OrderPayment orderId="ord_1" />
        </BillingProvider>
      )
      expect(
        await screen.findByRole("button", { name: "Pay $15.00 with CCBill" })
      ).toBeInTheDocument()
      expect(
        screen.getByRole("button", { name: "Pay $15.00" })
      ).toBeInTheDocument()
      expect(
        screen.queryByText("Card payment isn't working?")
      ).not.toBeInTheDocument()
    })

    it("never offers a fallback the order does not list", async () => {
      const { fetch } = server(() => [card(5)])
      render(
        <BillingProvider client={createBillingClient({ fetch })}>
          <OrderPayment orderId="ord_1" fallbacks={fallbacks} />
        </BillingProvider>
      )
      expect(
        await screen.findByRole("button", { name: "Pay $15.00" })
      ).toBeInTheDocument()
      expect(
        screen.queryByText("Card payment isn't working?")
      ).not.toBeInTheDocument()
      expect(
        screen.queryByRole("button", { name: /CCBill/ })
      ).not.toBeInTheDocument()
    })
  })
})

const platformElements = vi.hoisted(() => ({
  create: vi.fn(async () => ({ id: "ti_platform" })),
  destroy: vi.fn(async () => undefined),
}))
vi.mock("@basis-theory/web-elements", () => ({
  default: () => ({
    createElement: () => ({ mount: async () => undefined }),
    tokenIntents: { create: platformElements.create },
    destroy: platformElements.destroy,
  }),
}))

it.each(["unconfirmed", "server error", "in flight", "declined", "rejected"])("platform card %s retry preserves the right payment identity", async (outcome) => {
  platformElements.create.mockReset()
    .mockResolvedValueOnce({ id: "ti_first" })
    .mockResolvedValue({ id: "ti_next" })
  const uncertain = ["unconfirmed", "server error", "in flight"].includes(outcome)
  const requests: RequestInit[] = []
  const platformOrder = (payment: Record<string, unknown>, status = "open") => ({
    ...order(payment, status),
    payment_options: [{ psp_id: "psp_nmi", rail: "nmi", accepts: ["new_card"] }],
  })
  const fetch = vi.fn(async (input: string, init?: RequestInit) => {
    if (input.endsWith("/me/config")) return Response.json({ ...config, payment: { psps: [{
      psp_id: "psp_nmi", key: "nmi", rail: "nmi", custodian: "platform", display_name: "Card", flow: "tokenize",
      config: { public_api_key: "public-platform-key" },
    }] } })
    if (input.endsWith("/config")) return Response.json(deployment)
    if (input.includes("/me/payment-methods")) return Response.json({ data: [], next_cursor: null })
    if (input.endsWith("/me/orders/ord_1")) return Response.json(platformOrder({}))
    if (input.endsWith("/pay") && init) {
      requests.push(init)
      if (requests.length === 1) {
        if (outcome === "unconfirmed") throw new TypeError("connection lost")
        if (outcome === "server error") return Response.json({ error: {
          type: "api_error", code: "service_unavailable", message: "Try again",
        } }, { status: 503 })
        if (outcome === "in flight") return Response.json({ error: {
          type: "invalid_request_error", code: "idempotency_key_in_use", message: "Still processing",
        } }, { status: 409 })
        if (outcome === "rejected") return Response.json({ error: {
          type: "invalid_request_error", code: "invalid_param", param: "payment.token",
          message: "Collect the card again",
        } }, { status: 400 })
        return Response.json({ error: { type: "card_error", code: "card_declined", message: "Card declined",
          order: platformOrder({ last_payment_error: { reason: "generic_decline", message: "Card declined" } }) },
        }, { status: 402 })
      }
      return Response.json(platformOrder({ status: "succeeded" }, "complete"))
    }
    return Response.json({}, { status: 404 })
  })
  const { container } = render(<BillingProvider client={createBillingClient({ fetch })}><OrderPayment orderId="ord_1" /></BillingProvider>)
  await screen.findByRole("form", { name: "Secure platform card payment" })
  fireEvent.change(screen.getByLabelText("Name on card"), { target: { value: "Pat Reader" } })
  fireEvent.change(screen.getByLabelText("Country"), { target: { value: "US" } })
  fireEvent.change(screen.getByLabelText(/ZIP/), { target: { value: "94107" } })
  const pay = screen.getByRole("button", { name: /Pay / })
  await waitFor(() => expect(pay).toBeEnabled())
  fireEvent.click(pay)
  if (uncertain) {
    const retry = await screen.findByRole("button", { name: "Retry payment" })
    await waitFor(() => expect(retry).toBeEnabled())
    expect(screen.getByLabelText("Name on card")).toBeDisabled()
    fireEvent.click(retry)
  } else {
    if (outcome === "declined") await screen.findByText("Card declined")
    await waitFor(() => expect(requests).toHaveLength(1))
    await waitFor(() => expect(screen.getByLabelText("Name on card")).toBeEnabled())
    await waitFor(() => expect(pay).toBeEnabled())
    fireEvent.click(pay)
  }
  await screen.findByText("Payment complete")
  expect(requests).toHaveLength(2)
  const keys = requests.map((request) => new Headers(request.headers).get("Idempotency-Key"))
  expect(keys[0]).toBeTruthy()
  if (uncertain) {
    expect(keys[1]).toEqual(keys[0])
    expect(requests[1].body).toEqual(requests[0].body)
    expect(platformElements.create).toHaveBeenCalledTimes(1)
  } else {
    expect(keys[1]).not.toEqual(keys[0])
    expect(platformElements.create).toHaveBeenCalledTimes(2)
    expect(JSON.parse(String(requests[1].body)).payment.token).toBe("ti_next")
  }
  expect(container.querySelector('input[autocomplete="cc-number"]')).toBeNull()
  expect(container.querySelector('input[autocomplete="cc-csc"]')).toBeNull()
})
