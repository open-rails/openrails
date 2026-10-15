import {
  act,
  fireEvent,
  render,
  screen,
  within,
} from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"

import { createBillingClient } from "../client/client"
import { BillingProvider } from "../react/provider"
import {
  apiError,
  fakeBilling,
  json,
  paymentMethod,
} from "../test/billing-server"
import { PaymentMethodsPanel } from "./payment-methods-panel"

afterEach(() => vi.useRealTimers())

function setup(server = fakeBilling()) {
  const onChange = vi.fn()
  render(
    <BillingProvider
      client={createBillingClient({ fetch: server.fetch })}
      onChange={onChange}
    >
      <PaymentMethodsPanel />
    </BillingProvider>
  )
  return { server, onChange }
}

async function requestRemoval() {
  const row = await screen.findByTestId("payment-method-row")
  const trash = within(row).getByRole("button", { name: /Remove Visa/ })
  expect(trash).toHaveClass("bg-red-600")
  fireEvent.click(trash)
  const dialog = await screen.findByRole("alertdialog")
  fireEvent.click(within(dialog).getByRole("button", { name: "Remove card" }))
  return dialog
}

describe("inline payment method removal", () => {
  it("keeps an unconfirmed removal retryable and never polls or retries in the background", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { server, onChange } = setup()
    server.fail["DELETE /me/payment-methods/pm_1"] = apiError(
      503,
      "service_unavailable"
    )
    const dialog = await requestRemoval()
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "Card removal could not be confirmed. Please try again."
    )
    expect(screen.getByTestId("payment-method-row")).toBeInTheDocument()
    expect(
      within(dialog).getByRole("button", { name: "Remove card" })
    ).toBeEnabled()
    expect(onChange).not.toHaveBeenCalled()
    const calls = server.calls.length
    await act(() => vi.advanceTimersByTimeAsync(60_000))
    expect(server.calls).toHaveLength(calls)
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove card" }))
    await screen.findByText("No saved cards")
    expect(onChange).toHaveBeenCalledExactlyOnceWith({
      type: "payment_method.removed",
      paymentMethodId: "pm_1",
    })
    expect(server.calls.filter((c) => c.startsWith("DELETE "))).toHaveLength(2)
  })

  it("does not announce success for an older server's 202 response", async () => {
    const { server, onChange } = setup()
    server.fail["DELETE /me/payment-methods/pm_1"] = json(202)
    const dialog = await requestRemoval()
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "Card removal could not be confirmed"
    )
    expect(screen.queryByText("Card removed.")).not.toBeInTheDocument()
    expect(onChange).not.toHaveBeenCalled()
    expect(
      within(dialog).getByRole("button", { name: "Remove card" })
    ).toBeEnabled()
  })

  it("allows explicit retry for an unavailable saved card", async () => {
    setup(
      fakeBilling({ methods: [paymentMethod({ health: { active: false } })] })
    )
    await requestRemoval()
    await screen.findByText("Card removed.")
    expect(screen.queryByTestId("payment-method-row")).not.toBeInTheDocument()
  })

  it("keeps an in-use refusal open", async () => {
    const { server, onChange } = setup()
    server.fail["DELETE /me/payment-methods/pm_1"] = apiError(
      409,
      "resource_conflict"
    )
    const dialog = await requestRemoval()
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "Switch that subscription to another card first"
    )
    expect(screen.getByTestId("payment-method-row")).toBeInTheDocument()
    expect(onChange).not.toHaveBeenCalled()
  })
})
