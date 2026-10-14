import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { expect, it, vi } from "vitest"
import { createBillingClient } from "./client/client"
import { BillingProvider } from "./react/provider"
import { SavePaymentMethod } from "./save-payment-method"

const elements = vi.hoisted(() => ({ create: vi.fn() }))
vi.mock("@basis-theory/web-elements", () => ({
  default: () => ({
    createElement: () => ({ mount: async () => undefined }),
    tokenIntents: { create: elements.create },
    destroy: async () => undefined,
  }),
}))

it.each(["saved", "response lost", "declined"])(
  "platform card setup: %s",
  async (outcome) => {
    elements.create
      .mockReset()
      .mockResolvedValueOnce({ id: "first" })
      .mockResolvedValue({ id: "second" })
    const bodies: unknown[] = []
    const fetch = vi.fn(async (url: string, init: RequestInit) => {
      expect(url).toMatch(/\/me\/payment-methods$/)
      bodies.push(JSON.parse(String(init.body)))
      if (bodies.length === 1) {
        if (outcome === "response lost") throw new TypeError("connection lost")
        if (outcome === "declined")
          return Response.json(
            {
              error: {
                type: "card_error",
                code: "card_declined",
                message: "Card declined",
              },
            },
            { status: 402 }
          )
      }
      return Response.json({ id: "pm_saved", status: "active" })
    })
    const onSaved = vi.fn()
    const { container } = render(
      <BillingProvider client={createBillingClient({ fetch })}>
        <SavePaymentMethod
          psp={{
            psp_id: "psp_nmi",
            key: "nmi",
            rail: "nmi",
            custodian: "platform",
            display_name: "Card",
            flow: "tokenize",
            config: { public_api_key: "key_test_public" },
          }}
          onSaved={onSaved}
          defaultCountry="US"
        />
      </BillingProvider>
    )
    fireEvent.change(screen.getByLabelText(/full name/i), {
      target: { value: "Pat Reader" },
    })
    fireEvent.change(screen.getByLabelText(/ZIP/), {
      target: { value: "94107" },
    })
    const save = screen.getByRole("button", { name: "Save card" })
    await waitFor(() => expect(save).toBeEnabled())
    fireEvent.click(save)
    if (outcome === "response lost") {
      const retry = await screen.findByRole("button", {
        name: "Retry saving card",
      })
      expect(onSaved).not.toHaveBeenCalled()
      expect(screen.getByLabelText(/full name/i)).toBeDisabled()
      await waitFor(() => expect(retry).toBeEnabled())
      fireEvent.click(retry)
    } else if (outcome === "declined") {
      await screen.findByText("The card was declined.")
      expect(onSaved).not.toHaveBeenCalled()
      await waitFor(() => expect(save).toBeEnabled())
      fireEvent.click(save)
    }
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith("pm_saved"))
    expect(elements.create).toHaveBeenCalledTimes(
      outcome === "declined" ? 2 : 1
    )
    if (outcome === "response lost") expect(bodies[1]).toEqual(bodies[0])
    expect(
      container.querySelector('input[autocomplete="cc-number"]')
    ).toBeNull()
    expect(container.querySelector('input[autocomplete="cc-csc"]')).toBeNull()
  }
)
