import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "./client/client"

// Stripe.js in the page: its element reports a complete card, and
// createPaymentMethod tokenizes it.
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
vi.mock("#orck/lib/stripe", () => ({ loadStripeFor: async () => fakeStripe }))
import {
  canAuthenticatePayment,
  cardRetryAfter,
  cardSetupDriver,
  checkoutPsps,
  railPsp,
  savedMethodsFor,
  type PspConfig,
} from "./psp"
import { BillingProvider } from "./react/provider"
import { SavePaymentMethod } from "./save-payment-method"
import type { PaymentOption } from "./types"

const nmi: PspConfig = {
  psp_id: "psp_nmi",
  key: "nmi",
  rail: "nmi",
  custodian: "psp",
  display_name: "Credit Card",
  flow: "tokenize",
  config: {
    tokenization_key: "tok",
    tokenization_url: "https://nmi.test/Collect.js",
  },
}
const stripe: PspConfig = {
  psp_id: "psp_stripe",
  key: "stripe",
  rail: "stripe",
  custodian: "psp",
  display_name: "Stripe",
  flow: "redirect",
  config: { publishable_key: "pk_test_1" },
}
const ccbill: PspConfig = {
  ...stripe,
  psp_id: "psp_ccbill",
  key: "ccbill",
  rail: "ccbill",
  config: null,
}

describe("PSP flows", () => {
  it("matches saved cards to a session's options by PSP", () => {
    const options: PaymentOption[] = [
      {
        id: "option_a",
        psp_id: "psp_nmi",
        rail: "nmi",
        mode: "subscription",
        driver: "collect_js",
      },
      {
        id: "option_b",
        psp_id: "psp_solana",
        rail: "solana",
        mode: "subscription",
        driver: "solana_pay",
      },
    ]
    expect(
      savedMethodsFor(
        [
          { id: "pm_1", psp_id: "psp_nmi", card: { last4: "1111" } },
          { id: "pm_2", psp_id: "psp_solana" },
          { id: "pm_3", psp_id: "psp_nmi", health: { active: false } },
        ],
        options
      ).map((m) => [m.id, m.option_id, m.card?.last4])
    ).toEqual([["pm_1", "option_a", "1111"]])
    expect(railPsp(options[0]).psp_id).toBe("psp_nmi")
  })

  it("picks the in-page card setup from the PSP configuration", () => {
    expect(cardSetupDriver(nmi)).toBe("collect_js")
    expect(cardSetupDriver({ ...nmi, flow: "card", config: null })).toBe("card")
    expect(cardSetupDriver({ ...stripe, flow: "card" })).toBe("stripe_elements")
    expect(cardSetupDriver(stripe)).toBe("stripe_elements")
    expect(cardSetupDriver(ccbill)).toBeNull()
    expect(cardSetupDriver({ ...nmi, custodian: "basis_theory" })).toBeNull()
    expect(canAuthenticatePayment(stripe)).toBe(true)
    expect(canAuthenticatePayment(nmi)).toBe(false)
  })

  it("saves no card with a temporarily unavailable PSP and says when to ask again", () => {
    const down = {
      ...nmi,
      flow: "card",
      config: null,
      status: "temporarily_unavailable",
      retry_after: 30,
    }
    expect(cardSetupDriver(down)).toBeNull()
    expect(cardRetryAfter([down, { ...down, retry_after: 10 }])).toBe(10)
    expect(cardRetryAfter([nmi, { ...ccbill, status: down.status }])).toBeNull()
  })

  it("offers Stripe Elements as an in-page card rail and hides non-checkout PSPs", () => {
    const elements = { ...stripe, flow: "elements" }
    const rails: PaymentOption[] = [
      {
        id: "option_stripe",
        psp_id: "psp_stripe",
        rail: "stripe",
        mode: "one_off",
        driver: "stripe_elements",
        public_config: { publishable_key: "pk_test_1" },
      },
    ]
    expect(railPsp(rails[0])).toEqual(
      expect.objectContaining({ psp_id: "psp_stripe", flow: "elements" })
    )
    expect(
      savedMethodsFor(
        [
          {
            id: "pm_old",
            psp_id: "psp_stripe",
            card: { brand: "visa", last4: "4242" },
            created_at: "2026-01-01T00:00:00Z",
          },
          {
            id: "pm_new",
            psp_id: "psp_stripe",
            card: { brand: "visa", last4: "1881" },
            created_at: "2026-02-01T00:00:00Z",
          },
        ],
        rails
      ).map((m) => m.id)
    ).toEqual(["pm_new", "pm_old"])
    expect(
      checkoutPsps([nmi, { ...elements, checkout: false }]).map((p) => p.key)
    ).toEqual(["nmi"])
  })

  it("saves a Stripe card in one call and reports the method", async () => {
    const calls: { path: string; body: unknown }[] = []
    const fetch = vi.fn(async (input: string, init: RequestInit) => {
      calls.push({
        path: input,
        body: init.body ? JSON.parse(String(init.body)) : undefined,
      })
      return Response.json({ id: "pm_9", status: "active" })
    })
    const onSaved = vi.fn()
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <SavePaymentMethod psp={stripe} onSaved={onSaved} />
      </BillingProvider>
    )
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument()
    expect(
      screen.getByText(/saved to your account for future payments/)
    ).toBeInTheDocument()
    const save = await screen.findByRole("button", { name: "Save card" })
    await waitFor(() => expect(save).toBeEnabled())
    fireEvent.click(save)
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith("pm_9"))
    expect(calls).toEqual([
      {
        path: "/billing/v1/me/payment-methods",
        body: { psp_id: "psp_stripe", token: "pm_entered" },
      },
    ])
    expect(fakeStripe.handleNextAction).not.toHaveBeenCalled()
  })

  // The bank asks for 3-D Secure before saving: Stripe.js answers it in the
  // page and OpenRails confirms the save.
  it("authenticates a Stripe card the bank challenges, then confirms it", async () => {
    const paths: string[] = []
    const fetch = vi.fn(async (input: string) => {
      paths.push(input)
      return input.endsWith("/confirm")
        ? Response.json({ id: "pm_9", status: "active" })
        : Response.json({
            id: "pm_9",
            status: "requires_action",
            next_action: {
              type: "authenticate",
              psp_id: "psp_stripe",
              payload: { client_secret: "seti_1_secret_x", setup_intent_id: "seti_1" },
            },
          })
    })
    const onSaved = vi.fn()
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <SavePaymentMethod psp={stripe} onSaved={onSaved} />
      </BillingProvider>
    )
    const save = await screen.findByRole("button", { name: "Save card" })
    await waitFor(() => expect(save).toBeEnabled())
    fireEvent.click(save)
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith("pm_9"))
    expect(fakeStripe.handleNextAction).toHaveBeenCalledWith({
      clientSecret: "seti_1_secret_x",
    })
    expect(paths).toEqual([
      "/billing/v1/me/payment-methods",
      "/billing/v1/me/payment-methods/pm_9/confirm",
    ])
  })

  // A PSP whose card_entry is server takes the card in plain inputs; it goes
  // to OpenRails, and no gateway script is loaded.
  it("saves a card with OpenRails itself for a server card-entry PSP", async () => {
    const calls: { path: string; body: unknown }[] = []
    const fetch = vi.fn(async (input: string, init: RequestInit) => {
      calls.push({ path: input, body: JSON.parse(String(init.body)) })
      return Response.json({
        id: "pm_7",
        psp_id: "psp_nmi",
        card: { brand: "visa", last4: "1111", exp_month: 10, exp_year: 2027 },
      })
    })
    const onSaved = vi.fn()
    render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <SavePaymentMethod
          psp={{ ...nmi, flow: "card", config: null }}
          onSaved={onSaved}
          defaultCountry="US"
        />
      </BillingProvider>
    )
    const save = screen.getByRole("button", { name: "Save card" })
    expect(save).toBeDisabled()
    fireEvent.change(screen.getByLabelText("Name on card"), {
      target: { value: "Pat Reader" },
    })
    fireEvent.change(screen.getByLabelText("ZIP code"), {
      target: { value: "94107" },
    })
    fireEvent.change(screen.getByLabelText("Card number"), {
      target: { value: "4111111111111111" },
    })
    fireEvent.change(screen.getByLabelText("Expiry"), {
      target: { value: "1027" },
    })
    fireEvent.change(screen.getByLabelText("CVC"), { target: { value: "999" } })
    expect(screen.getByLabelText("Card number")).toHaveValue(
      "4111 1111 1111 1111"
    )
    expect(screen.getByLabelText("Card number")).toHaveAttribute(
      "autocomplete",
      "cc-number"
    )
    await waitFor(() => expect(save).toBeEnabled())
    fireEvent.click(save)
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith("pm_7"))
    expect(calls).toEqual([
      {
        path: "/billing/v1/me/payment-methods",
        body: {
          psp_id: "psp_nmi",
          billing_details: {
            name: "Pat Reader",
            address: { postal_code: "94107", country: "US" },
          },
          card: {
            number: "4111111111111111",
            exp_month: 10,
            exp_year: 2027,
            cvc: "999",
          },
        },
      },
    ])
    expect(document.getElementById("openrails-collectjs")).toBeNull()
    expect(screen.getByLabelText("Card number")).toHaveValue("")
    expect(screen.getByLabelText("CVC")).toHaveValue("")
  })

  it("says so when a PSP cannot save cards in the page", () => {
    render(
      <BillingProvider client={createBillingClient({ fetch: vi.fn() })}>
        <SavePaymentMethod psp={ccbill} onSaved={vi.fn()} />
      </BillingProvider>
    )
    expect(screen.getByRole("alert")).toHaveTextContent("unavailable")
  })
})
