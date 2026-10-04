import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "./client/client"
import { BillingUiProvider } from "./provider"
import {
  canAuthenticatePayment,
  cardSetupDriver,
  checkoutPsps,
  checkoutRails,
  savedMethodsFor,
  type PspConfig,
} from "./psp"
import { BillingProvider } from "./react/provider"
import { SavePaymentMethod } from "./save-payment-method"

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
  it("renders exactly the rails OpenRails advertised", () => {
    const rails = checkoutRails([
      {
        selector: "nmi",
        psp_id: "psp_nmi",
        rail: "nmi",
        mode: "subscription",
        driver: "collect_js",
        public_config: nmi.config ?? undefined,
      },
      {
        selector: "solana",
        psp_id: "psp_solana",
        rail: "solana",
        mode: "subscription",
        driver: "solana_pay",
        public_config: { token_symbol: "DUSD", network: "devnet" },
      },
      // Armed but not browser-drivable (no publishable key for a subscription).
      {
        selector: "stripe",
        psp_id: "psp_stripe",
        rail: "stripe",
        mode: "subscription",
      },
      {
        selector: "x",
        psp_id: "psp_x",
        rail: "x",
        mode: "one_off",
        driver: "wire",
      },
    ])
    expect(rails.map((r) => [r.id, r.driver, r.mode, r.psp_key])).toEqual([
      ["psp_nmi", "collect_js", "subscription", "nmi"],
      ["psp_solana", "solana_pay", "subscription", "solana"],
    ])
    expect(rails[1].public_config).toEqual({
      token_symbol: "DUSD",
      network: "devnet",
    })
    expect(
      savedMethodsFor(
        [
          { id: "pm_1", psp_id: "psp_nmi", card: { last4: "1111" } },
          { id: "pm_2", psp_id: "psp_solana" },
          { id: "pm_3", psp_id: "psp_nmi", health: { active: false } },
        ],
        rails
      ).map((m) => [m.id, m.last_four])
    ).toEqual([["pm_1", "1111"]])
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

  it("offers Stripe Elements as an in-page card rail and hides non-checkout PSPs", () => {
    const elements = { ...stripe, flow: "elements" }
    const rails = checkoutRails([
      {
        selector: "stripe",
        psp_id: "psp_stripe",
        rail: "stripe",
        mode: "one_off",
        driver: "stripe_elements",
        public_config: { publishable_key: "pk_test_1" },
      },
    ])
    expect(rails).toEqual([
      expect.objectContaining({
        id: "psp_stripe",
        driver: "stripe_elements",
        psp_key: "stripe",
      }),
    ])
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

  it("always saves the card through the PSP's setup and reports the method", async () => {
    const calls: { path: string; key: string | null; body: unknown }[] = []
    const fetch = vi.fn(async (input: string, init: RequestInit) => {
      calls.push({
        path: input,
        key: new Headers(init.headers).get("Idempotency-Key"),
        body: init.body ? JSON.parse(String(init.body)) : undefined,
      })
      return Response.json({
        id: "seti_1",
        status: "succeeded",
        payment_method_id: "pm_9",
      })
    })
    const onSaved = vi.fn()
    render(
      <BillingUiProvider>
        <BillingProvider client={createBillingClient({ fetch })}>
          <SavePaymentMethod psp={stripe} onSaved={onSaved} />
        </BillingProvider>
      </BillingUiProvider>
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
        path: "/billing/v1/me/payment-methods/stripe-setup",
        key: expect.any(String),
        body: { psp_id: "psp_stripe", consent: true },
      },
    ])
  })

  // #1129: a PSP whose card_entry is server takes the card in plain inputs;
  // it goes to OpenRails, and no gateway script is loaded.
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
      <BillingUiProvider>
        <BillingProvider client={createBillingClient({ fetch })}>
          <SavePaymentMethod
            psp={{ ...nmi, flow: "card", config: null }}
            onSaved={onSaved}
            defaultCountry="US"
          />
        </BillingProvider>
      </BillingUiProvider>
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
          provider: "nmi",
          name_on_card: "Pat Reader",
          country: "US",
          zip: "94107",
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
      <BillingUiProvider>
        <BillingProvider client={createBillingClient({ fetch: vi.fn() })}>
          <SavePaymentMethod psp={ccbill} onSaved={vi.fn()} />
        </BillingProvider>
      </BillingUiProvider>
    )
    expect(screen.getByRole("alert")).toHaveTextContent("unavailable")
  })
})
