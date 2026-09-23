import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "./client/client"
import { BillingUiProvider } from "./provider"
import {
  canAuthenticatePayment,
  cardSetupDriver,
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
  it("maps checkout rails from each PSP's flow, never its name", () => {
    const offers = [
      nmi,
      stripe,
      ccbill,
      {
        ...nmi,
        psp_id: "psp_preview",
        config: {
          tokenization_key: "preview_x",
          tokenization_url: "https://x",
        },
      },
    ].map((psp) => ({ psp_id: psp.psp_id, rail: psp.rail, mode: "one_off" }))
    const rails = checkoutRails(offers, [
      nmi,
      stripe,
      ccbill,
      {
        ...nmi,
        psp_id: "psp_preview",
        config: {
          tokenization_key: "preview_x",
          tokenization_url: "https://x",
        },
      },
    ])
    expect(rails.map((r) => [r.id, r.driver])).toEqual([
      ["psp_nmi", "collect_js"],
      ["psp_stripe", "redirect"],
      ["psp_ccbill", "redirect"],
    ])
    expect(
      savedMethodsFor(
        [
          { id: "pm_1", psp_id: "psp_nmi", card: { last4: "1111" } },
          { id: "pm_2", psp_id: "psp_stripe" },
          { id: "pm_3", psp_id: "psp_nmi", health: { active: false } },
        ],
        rails
      ).map((m) => [m.id, m.last_four])
    ).toEqual([["pm_1", "1111"]])
  })

  it("picks the in-page card setup from the PSP configuration", () => {
    expect(cardSetupDriver(nmi)).toBe("collect_js")
    expect(cardSetupDriver(stripe)).toBe("stripe_elements")
    expect(cardSetupDriver(ccbill)).toBeNull()
    expect(cardSetupDriver({ ...nmi, custodian: "basis_theory" })).toBeNull()
    expect(canAuthenticatePayment(stripe)).toBe(true)
    expect(canAuthenticatePayment(nmi)).toBe(false)
  })

  it("saves with consent through the PSP's setup and reports the method", async () => {
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
    const start = screen.getByRole("button", {
      name: "Enter card details securely",
    })
    expect(start).toBeDisabled()
    fireEvent.click(screen.getByRole("checkbox"))
    fireEvent.click(start)
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith("pm_9"))
    expect(calls).toEqual([
      {
        path: "/billing/v1/me/payment-methods/stripe-setup",
        key: expect.any(String),
        body: { psp_id: "psp_stripe", consent: true },
      },
    ])
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
