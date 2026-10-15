import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "../client/client"
import { BillingProvider } from "../react/provider"
import {
  apiError,
  fakeBilling,
  paymentMethod,
  subscription,
  type FakeBilling,
} from "../test/billing-server"
import { SubscriptionsPanel } from "./subscriptions-panel"

vi.mock("../save-payment-method", () => ({
  SavePaymentMethod: ({
    onSaved,
    psp,
  }: {
    onSaved: (id: string) => void
    psp: { psp_id: string }
  }) => (
    <button
      data-testid="secure-save"
      data-provider={psp.psp_id}
      onClick={() => onSaved("pm_new")}
    >
      Complete secure save
    </button>
  ),
}))

const sid = "sub_cccccccc-cccc-4ccc-8ccc-cccccccccccc"
const psp = "psp_55555555-5555-5555-5555-555555555555"
const resumeCall = `POST /me/subscriptions/${sid}/resume`
const methodCall = `PUT /me/subscriptions/${sid}/payment-method`

function serverWithCards(
  methods = [
    paymentMethod({ id: "pm_old" }),
    paymentMethod({
      id: "pm_new",
      card: { brand: "mastercard", last4: "4444" },
    }),
  ]
) {
  return fakeBilling({
    subscriptions: [
      subscription({
        id: sid,
        status: "canceled",
        resumable: true,
        cancel_scheduled: true,
        payment_method_id: "pm_old",
        collection_policy: "engine",
      }),
    ],
    methods,
    psps: [
      {
        psp_id: psp,
        key: "card",
        rail: "nmi",
        custodian: "psp",
        display_name: "Cards",
        flow: "card",
      },
    ],
  })
}

async function openResume(server: FakeBilling) {
  render(
    <BillingProvider client={createBillingClient({ fetch: server.fetch })}>
      <SubscriptionsPanel />
    </BillingProvider>
  )
  const row = await screen.findByTestId("subscription-row")
  fireEvent.click(within(row).getByRole("button", { name: "Resume" }))
  return screen.findByRole("dialog")
}

describe("resuming with a selected payment method", () => {
  it.each([
    {
      rail: null,
      collection_policy: "engine",
      free: true,
    },
    { rail: "stripe", collection_policy: "provider", free: false },
    { rail: "nmi", collection_policy: "nmi_schedule", free: false },
  ])(
    "does not require a card outside the server card-subscription branch: %j",
    async (fields) => {
      const server = serverWithCards([])
      Object.assign(server.subscriptions[0], {
        rail: fields.rail,
        collection_policy: fields.collection_policy,
      })
      if (fields.free)
        (server.subscriptions[0].price as Record<string, unknown>).unit_amount =
          "0"
      render(
        <BillingProvider client={createBillingClient({ fetch: server.fetch })}>
          <SubscriptionsPanel />
        </BillingProvider>
      )
      fireEvent.click(
        within(await screen.findByTestId("subscription-row")).getByRole(
          "button",
          { name: "Resume" }
        )
      )
      const dialog = await screen.findByRole("alertdialog")
      expect(server.calls).not.toContain("GET /me/payment-methods")
      expect(server.calls).not.toContain(resumeCall)
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Resume subscription" })
      )
      await waitFor(() => expect(server.subscriptions[0].status).toBe("active"))
      expect(server.calls).not.toContain(methodCall)
    }
  )

  it("opens eligible card selection without resuming, then assigns before explicit resume", async () => {
    const server = serverWithCards()
    server.methods.push(
      paymentMethod({
        id: "pm_bad",
        health: { active: false },
        card: { last4: "1111" },
      })
    )
    const dialog = await openResume(server)
    const select = await within(dialog).findByRole("combobox")
    expect(server.calls).not.toContain(resumeCall)
    expect(within(select).queryByText(/1111/)).not.toBeInTheDocument()
    fireEvent.change(select, { target: { value: "pm_new" } })
    fireEvent.click(
      within(dialog).getByRole("button", { name: "Resume subscription" })
    )
    await waitFor(() => expect(server.subscriptions[0].status).toBe("active"))
    expect(server.subscriptions[0].payment_method_id).toBe("pm_new")
    expect(server.calls.indexOf(methodCall)).toBeLessThan(
      server.calls.indexOf(resumeCall)
    )
  })

  it.each([false, true])(
    "uses secure card entry with existing cards=%s and waits for final resume",
    async (existing) => {
      const server = serverWithCards(
        existing ? [paymentMethod({ id: "pm_old" })] : []
      )
      const dialog = await openResume(server)
      if (existing)
        fireEvent.click(
          await within(dialog).findByRole("button", { name: "Add card" })
        )
      const save = await within(dialog).findByTestId("secure-save")
      expect(save).toHaveAttribute("data-provider", psp)
      expect(server.calls).not.toContain(resumeCall)
      server.methods.push(paymentMethod({ id: "pm_new" }))
      fireEvent.click(save)
      const select = await within(dialog).findByRole("combobox")
      await waitFor(() => expect(select).toHaveValue("pm_new"))
      expect(server.subscriptions[0].status).toBe("canceled")
      expect(server.calls).not.toContain(resumeCall)
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Resume subscription" })
      )
      await waitFor(() => expect(server.subscriptions[0].status).toBe("active"))
    }
  )

  it.each(["assignment", "resume"])(
    "keeps cancellation and paid access when %s fails",
    async (failure) => {
      const server = serverWithCards()
      const access = server.subscriptions[0].access
      server.fail[failure === "assignment" ? methodCall : resumeCall] =
        apiError(
          failure === "assignment" ? 409 : 402,
          failure === "assignment"
            ? "resource_conflict"
            : "payment_method_stale"
        )
      const dialog = await openResume(server)
      fireEvent.change(await within(dialog).findByRole("combobox"), {
        target: { value: "pm_new" },
      })
      fireEvent.click(
        within(dialog).getByRole("button", { name: "Resume subscription" })
      )
      expect(await within(dialog).findByRole("alert")).toBeVisible()
      expect(server.subscriptions[0].status).toBe("canceled")
      expect(server.subscriptions[0].access).toEqual(access)
      if (failure === "assignment")
        expect(server.calls).not.toContain(resumeCall)
    }
  )

  it("offers a retry when refreshing a newly saved card fails", async () => {
    const server = serverWithCards([paymentMethod({ id: "pm_old" })])
    const dialog = await openResume(server)
    fireEvent.click(
      await within(dialog).findByRole("button", { name: "Add card" })
    )
    server.methods.push(paymentMethod({ id: "pm_new" }))
    server.fail["GET /me/payment-methods"] = apiError(
      503,
      "service_unavailable"
    )
    server.fail["GET /me/payment-methods"].headers.set("Retry-After", "30")
    fireEvent.click(await within(dialog).findByTestId("secure-save"))
    expect(await within(dialog).findByRole("alert")).toBeVisible()
    expect(
      within(dialog).getByRole("button", { name: "Resume subscription" })
    ).toBeDisabled()
    expect(server.calls).not.toContain(resumeCall)
    delete server.fail["GET /me/payment-methods"]
    fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }))
    await waitFor(() =>
      expect(
        within(dialog).getByRole("button", { name: "Resume subscription" })
      ).toBeEnabled()
    )
    expect(server.calls).not.toContain(resumeCall)
  })

  it("does not silently substitute a card when a saved card is absent after refresh", async () => {
    const server = serverWithCards([paymentMethod({ id: "pm_old" })])
    const dialog = await openResume(server)
    fireEvent.click(
      await within(dialog).findByRole("button", { name: "Add card" })
    )
    fireEvent.click(await within(dialog).findByTestId("secure-save"))
    expect(await within(dialog).findByRole("status")).toHaveTextContent(
      "This saved card is not available"
    )
    expect(within(dialog).getByRole("combobox")).toHaveValue("")
    expect(
      within(dialog).getByRole("button", { name: "Resume subscription" })
    ).toBeDisabled()
    expect(server.calls).not.toContain(resumeCall)
  })
})
