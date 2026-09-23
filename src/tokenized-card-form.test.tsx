import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { TokenizedCardForm } from "./tokenized-card-form"

afterEach(() => {
  document.getElementById("openrails-collectjs")?.remove()
  Reflect.deleteProperty(window, "CollectJS")
})

describe("TokenizedCardForm", () => {
  it("uses the existing hosted tokenization driver and emits only tokenized billing fields", async () => {
    const save = vi.fn().mockResolvedValue(undefined)
    const { container } = render(
      <TokenizedCardForm
        tokenizationKey="preview_fixture"
        tokenizationURL="https://gateway.example/Collect.js"
        onTokenized={save}
      />
    )
    fireEvent.change(screen.getByLabelText("Name on card"), {
      target: { value: "Pat Reader" },
    })
    fireEvent.change(screen.getByLabelText("Country"), {
      target: { value: "US" },
    })
    fireEvent.change(screen.getByLabelText(/ZIP/), {
      target: { value: "94107" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Save card" }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    expect(save).toHaveBeenCalledWith({
      payment_token: "preview_payment_token",
      name_on_card: "Pat Reader",
      country: "US",
      zip: "94107",
    })
    expect(
      container.querySelector('input[autocomplete="cc-number"]')
    ).toBeNull()
    expect(container.querySelector('input[autocomplete="cc-csc"]')).toBeNull()
    expect(screen.queryByText("Payment complete")).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Save card" })).toBeDisabled()
  })
  it("does not tokenize or submit without host consent and does not repeat an ambiguous save", async () => {
    const save = vi.fn().mockRejectedValue(new Error("connection lost"))
    const { rerender } = render(
      <TokenizedCardForm
        tokenizationKey="preview_fixture"
        tokenizationURL="https://gateway.example/Collect.js"
        onTokenized={save}
        disabled
      />
    )
    expect(screen.getByRole("button", { name: "Save card" })).toBeDisabled()
    rerender(
      <TokenizedCardForm
        tokenizationKey="preview_fixture"
        tokenizationURL="https://gateway.example/Collect.js"
        onTokenized={save}
      />
    )
    fireEvent.change(screen.getByLabelText("Name on card"), {
      target: { value: "Pat Reader" },
    })
    fireEvent.change(screen.getByLabelText("Country"), {
      target: { value: "US" },
    })
    fireEvent.change(screen.getByLabelText(/ZIP/), {
      target: { value: "94107" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Save card" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("not confirmed")
    expect(screen.getByRole("button", { name: "Save card" })).toBeDisabled()
    expect(save).toHaveBeenCalledTimes(1)
  })
})
