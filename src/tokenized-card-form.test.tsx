import { act, fireEvent, render, screen, waitFor } from "@testing-library/react"
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
      last_four: "4242",
      card_type: "visa",
      expiry_date: "12/27",
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

it.each(["key", "url", "consent"])(
  "discards a token when %s changes while tokenization is pending",
  async (change) => {
    const save = vi.fn().mockResolvedValue(undefined)
    const props = {
      tokenizationKey: "preview_fixture",
      tokenizationURL: "https://gateway.example/Collect.js",
      onTokenized: save,
    }
    const { rerender } = render(<TokenizedCardForm {...props} />)
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
    rerender(
      <TokenizedCardForm
        {...props}
        tokenizationKey={
          change === "key" ? "preview_other" : props.tokenizationKey
        }
        tokenizationURL={
          change === "url"
            ? "https://other.example/Collect.js"
            : props.tokenizationURL
        }
        disabled={change === "consent"}
      />
    )
    await act(async () => {
      await Promise.resolve()
    })
    expect(save).not.toHaveBeenCalled()
  }
)

it("shows Collect.js field errors inline and waits for three valid fields", async () => {
  let config: Record<string, (...args: unknown[]) => void> = {}
  window.CollectJS = {
    configure: vi.fn((value: Record<string, unknown>) => {
      config = value as typeof config
    }),
    startPaymentRequest: vi.fn(),
  }
  const script = document.createElement("script")
  script.id = "openrails-collectjs"
  script.src = "https://gateway.example/Collect.js"
  document.head.appendChild(script)
  render(
    <TokenizedCardForm
      tokenizationKey="live_key"
      tokenizationURL="https://gateway.example/Collect.js"
      onTokenized={vi.fn()}
    />
  )
  await waitFor(() => expect(config.validationCallback).toBeDefined())
  act(() => config.fieldsAvailableCallback())
  const save = screen.getByRole("button", { name: "Save card" })
  expect(save).toBeDisabled()
  act(() =>
    config.validationCallback("ccnumber", false, "Card number is invalid")
  )
  expect(
    await screen.findByText("Enter a valid card number.")
  ).toBeInTheDocument()
  act(() => {
    config.validationCallback("ccnumber", true, "Success")
    config.validationCallback("ccexp", true, "Success")
    config.validationCallback("cvv", true, "Success")
  })
  expect(screen.queryByText("Enter a valid card number.")).toBeNull()
  expect(save).toBeEnabled()
})
