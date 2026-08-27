import { act, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"

import { Checkout } from "./checkout"
import { CheckoutModal } from "./modal"
import { createFixtureSource, fixtureSession } from "./fixtures"
import type { CheckoutSource } from "./source"
import type { PaymentRailOption } from "./types"

function checkoutOption(
  rail: "nmi" | "stripe" | "ccbill" | "solana",
  publicConfig?: Record<string, string>
): PaymentRailOption {
  const driver =
    rail === "nmi"
      ? "collect_js"
      : rail === "solana"
        ? "solana_pay"
        : "redirect"
  return {
    id: `option_${rail}`,
    rail,
    mode: rail === "solana" ? "one_off" : "subscription",
    driver,
    public_config: publicConfig,
  }
}

const createQR = vi.hoisted(() =>
  vi.fn(() => ({
    append(element: HTMLElement) {
      element.append(document.createElement("canvas"))
    },
  }))
)

vi.mock("@solana/pay", () => ({ createQR }))

afterEach(() => {
  document.getElementById("openrails-collectjs")?.remove()
  Reflect.deleteProperty(window, "CollectJS")
  createQR.mockClear()
  vi.useRealTimers()
})

function ccbillSource(pay: CheckoutSource["pay"]): CheckoutSource {
  return {
    async getSession() {
      return fixtureSession({
        rails: [checkoutOption("ccbill")],
      })
    },
    pay,
  }
}

describe("Checkout", () => {
  it("renders the fixture session to ready state", async () => {
    render(<Checkout source={createFixtureSource()} />)
    await waitFor(() => {
      // Both summary variants exist in the DOM; the container query decides
      // which is visible.
      expect(screen.getAllByText("Acme Demo").length).toBeGreaterThan(0)
    })
    expect(screen.getByText("Pay $99.00")).toBeInTheDocument()
    expect(screen.getByLabelText("Payment method")).toBeInTheDocument()
    expect(screen.getAllByText("Card").length).toBeGreaterThan(0)
    expect(screen.getByText("Stripe")).toBeInTheDocument()
    expect(screen.getByText("Crypto")).toBeInTheDocument()
  })

  it("renders one native cardholder identity with browser autofill semantics", async () => {
    render(
      <Checkout
        source={createFixtureSource({
          session: {
            rails: [checkoutOption("nmi")],
            saved_methods: [],
          },
        })}
      />
    )

    const form = await screen.findByRole("form", { name: "Secure payment" })
    expect(form).toHaveAttribute("autocomplete", "on")
    expect(form).toHaveAttribute("name", "openrails-checkout")

    const names = screen.getAllByLabelText("Name on card")
    expect(names).toHaveLength(1)
    expect(names[0]).toHaveAttribute("name", "name_on_card")
    expect(names[0]).toHaveAttribute("autocomplete", "cc-name")
    expect(screen.queryByLabelText("First name")).not.toBeInTheDocument()
    expect(screen.queryByLabelText("Last name")).not.toBeInTheDocument()

    const country = screen.getByLabelText("Country")
    expect(country).toBeInstanceOf(HTMLSelectElement)
    expect(country).toHaveAttribute("name", "country")
    expect(country).toHaveAttribute("autocomplete", "billing country")

    const initialPostal = screen.getByLabelText("Postal code")
    expect(initialPostal).toHaveAttribute("name", "zip")
    expect(initialPostal).toHaveAttribute("autocomplete", "billing postal-code")
    expect(initialPostal).toBeRequired()

    fireEvent.change(country, { target: { value: "AG" } })
    expect(screen.getByLabelText("Postal code (optional)")).not.toBeRequired()

    fireEvent.change(country, { target: { value: "US" } })
    const usPostal = screen.getByLabelText("ZIP code")
    expect(usPostal).toBeRequired()
    expect(usPostal).toHaveAttribute("inputmode", "numeric")
    expect(usPostal).toHaveAttribute("pattern", "[0-9]{5}(?:-[0-9]{4})?")
  })

  it("keeps inactive rail billing controls hidden, disabled, and uniquely identified", async () => {
    render(
      <Checkout
        source={createFixtureSource({
          session: {
            rails: [checkoutOption("nmi"), checkoutOption("ccbill")],
            saved_methods: [],
          },
        })}
      />
    )

    const form = await screen.findByRole("form", { name: "Secure payment" })
    expect(screen.getAllByLabelText("Name on card")).toHaveLength(1)

    fireEvent.click(screen.getByText("CCBill"))

    const names = screen.getAllByLabelText("Name on card")
    expect(names).toHaveLength(2)
    expect(
      names.filter((field) => !field.hasAttribute("disabled"))
    ).toHaveLength(1)
    expect(
      names.find((field) => field.id.includes("-card-name-on-card"))
    ).toBeDisabled()
    expect(
      names.find((field) => field.id.includes("-ccbill-name-on-card"))
    ).not.toBeDisabled()

    const ids = [...form.querySelectorAll<HTMLElement>("[id]")].map(
      (field) => field.id
    )
    expect(new Set(ids).size).toBe(ids.length)

    fireEvent.click(screen.getByRole("button", { name: "Continue to CCBill" }))
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Enter a valid email"
    )
  })

  it("renders expired sessions terminally", async () => {
    render(
      <Checkout
        source={createFixtureSource({ session: { status: "expired" } })}
      />
    )
    await waitFor(() => {
      expect(
        screen.getByText("This checkout link has expired")
      ).toBeInTheDocument()
    })
  })

  it("renders blocked sessions with the capability message", async () => {
    render(
      <Checkout
        source={createFixtureSource({
          session: {
            status: "blocked",
            failure_message: "This purchase is not available for this account.",
            rails: [],
          },
        })}
      />
    )

    expect(
      await screen.findByText("This purchase isn’t available")
    ).toBeInTheDocument()
    expect(
      screen.getByText("This purchase is not available for this account.")
    ).toBeInTheDocument()
  })

  it("withholds a rail paired with the wrong browser driver", async () => {
    render(
      <Checkout
        source={createFixtureSource({
          session: {
            rails: [
              {
                id: "option_bad",
                rail: "nmi",
                mode: "subscription",
                driver: "redirect",
              },
            ],
          },
        })}
      />
    )

    expect(
      await screen.findByText("Checkout isn’t available right now")
    ).toBeInTheDocument()
  })

  it("reports an already-settled session to its host", async () => {
    const onComplete = vi.fn()
    render(
      <Checkout
        source={createFixtureSource({
          session: {
            status: "succeeded",
            payment_id: "pay_existing",
            subscription_id: "sub_existing",
          },
        })}
        onComplete={onComplete}
      />
    )

    expect(
      (await screen.findAllByText("Payment complete")).length
    ).toBeGreaterThan(0)
    expect(onComplete).toHaveBeenCalledOnce()
    expect(onComplete).toHaveBeenCalledWith({
      status: "succeeded",
      payment_id: "pay_existing",
      subscription_id: "sub_existing",
    })
  })

  it("keeps an empty CCBill submission client-side", async () => {
    const pay = vi.fn<CheckoutSource["pay"]>()
    render(<Checkout source={ccbillSource(pay)} />)

    fireEvent.click(
      await screen.findByRole("button", { name: "Continue to CCBill" })
    )

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Enter a valid email"
    )
    expect(pay).not.toHaveBeenCalled()
  })

  it("submits completed CCBill billing details", async () => {
    const pay = vi.fn<CheckoutSource["pay"]>().mockResolvedValue({
      status: "failed",
      failure_message: "Test stop",
    })
    render(<Checkout source={ccbillSource(pay)} />)

    await screen.findByRole("button", { name: "Continue to CCBill" })
    for (const [label, value] of [
      ["Email", "jane@example.com"],
      ["Name on card", "Jane Tester"],
      ["Address", "123 Main St"],
      ["City", "Springfield"],
      ["State / region (optional)", "IL"],
    ]) {
      fireEvent.change(screen.getByLabelText(label), { target: { value } })
    }
    fireEvent.change(screen.getByLabelText("Country"), {
      target: { value: "US" },
    })
    fireEvent.change(screen.getByLabelText("ZIP code"), {
      target: { value: "62704" },
    })

    const name = screen.getByLabelText("Name on card")
    expect(name).toHaveAttribute("name", "name_on_card")
    expect(name).toHaveAttribute("autocomplete", "cc-name")
    expect(screen.getByLabelText("Address")).toHaveAttribute(
      "autocomplete",
      "billing address-line1"
    )
    fireEvent.click(screen.getByRole("button", { name: "Continue to CCBill" }))

    await waitFor(() => {
      expect(pay).toHaveBeenCalledWith({
        option_id: "option_ccbill",
        email: "jane@example.com",
        name_on_card: "Jane Tester",
        address1: "123 Main St",
        city: "Springfield",
        state: "IL",
        zip: "62704",
        country: "US",
      })
    })
  })

  it("pays with a stored card without tokenizing a new one", async () => {
    const pay = vi.fn<CheckoutSource["pay"]>().mockResolvedValue({
      status: "succeeded",
      payment_id: "pay_saved",
    })
    const source: CheckoutSource = {
      async getSession() {
        return fixtureSession({
          rails: [
            checkoutOption("nmi", {
              tokenization_key: "preview_tokenization_key",
              tokenization_url: "preview://collect",
            }),
          ],
          saved_methods: [
            {
              id: "pm_saved_1",
              option_id: "option_nmi",
              rail: "nmi",
              brand: "visa",
              last_four: "1111",
              exp_month: 10,
              exp_year: 2029,
            },
          ],
        })
      },
      pay,
    }

    render(<Checkout source={source} />)

    const saved = await screen.findByRole("radio", { name: /Visa/ })
    fireEvent.click(saved)
    fireEvent.click(screen.getByRole("button", { name: /^Pay / }))

    await waitFor(() => {
      expect(pay).toHaveBeenCalledWith({
        option_id: "option_nmi",
        payment_method_id: "pm_saved_1",
      })
    })
  })

  it("shows a stored-card failure and keeps the card payable", async () => {
    const pay = vi.fn<CheckoutSource["pay"]>().mockResolvedValue({
      status: "failed",
      failure_message:
        "That saved card is no longer available. Choose another card.",
    })
    const source: CheckoutSource = {
      async getSession() {
        return fixtureSession({
          rails: [
            checkoutOption("nmi", {
              tokenization_key: "preview_tokenization_key",
              tokenization_url: "preview://collect",
            }),
          ],
          saved_methods: [
            {
              id: "pm_saved_1",
              option_id: "option_nmi",
              rail: "nmi",
              brand: "visa",
              last_four: "1111",
            },
          ],
        })
      },
      pay,
    }

    render(<Checkout source={source} />)

    // The stored card is selected by default, so the button is live even
    // though the new-card fields are hidden.
    const button = await screen.findByRole("button", { name: /^Pay / })
    expect(button).not.toBeDisabled()
    fireEvent.click(button)

    // The failure must be visible: the card-field slot is hidden in this state.
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "That saved card is no longer available. Choose another card."
    )
  })

  it("falls back to new-card entry when the customer picks it", async () => {
    const pay = vi.fn<CheckoutSource["pay"]>().mockResolvedValue({
      status: "succeeded",
    })
    const source: CheckoutSource = {
      async getSession() {
        return fixtureSession({
          rails: [
            checkoutOption("nmi", {
              tokenization_key: "preview_tokenization_key",
              tokenization_url: "preview://collect",
            }),
          ],
          saved_methods: [
            {
              id: "pm_saved_1",
              option_id: "option_nmi",
              rail: "nmi",
              brand: "visa",
              last_four: "1111",
            },
          ],
        })
      },
      pay,
    }

    render(<Checkout source={source} />)

    fireEvent.click(
      await screen.findByRole("radio", { name: "Use a new card" })
    )
    fireEvent.change(screen.getByLabelText("Name on card"), {
      target: { value: "李 小龍" },
    })
    fireEvent.change(screen.getByLabelText("Country"), {
      target: { value: "JP" },
    })
    fireEvent.change(screen.getByLabelText("Postal code"), {
      target: { value: "100-0001" },
    })
    fireEvent.click(screen.getByRole("button", { name: /^Pay / }))

    await waitFor(() => {
      expect(pay).toHaveBeenCalledWith(
        expect.objectContaining({
          option_id: "option_nmi",
          payment_token: expect.any(String),
          name_on_card: "李 小龍",
          country: "JP",
          zip: "100-0001",
        })
      )
    })
  })

  it("creates a Solana transfer request and renders the official QR", async () => {
    const transactionURL =
      "solana:recipient?amount=19.99&spl-token=mint&reference=reference"
    const pay = vi.fn<CheckoutSource["pay"]>().mockResolvedValue({
      status: "requires_action",
      transaction_url: transactionURL,
    })
    const source: CheckoutSource = {
      async getSession() {
        return fixtureSession({
          rails: [checkoutOption("solana", { token_symbol: "usd1" })],
        })
      },
      pay,
    }

    render(<Checkout source={source} />)

    await waitFor(() => {
      expect(pay).toHaveBeenCalledWith({
        option_id: "option_solana",
        token_symbol: "USD1",
      })
    })
    expect(
      await screen.findByRole("img", { name: "Solana Pay QR code" })
    ).toBeInTheDocument()
    await waitFor(() => {
      expect(createQR).toHaveBeenCalledWith(
        transactionURL,
        208,
        "#ffffff",
        "#18181b"
      )
    })
    expect(
      screen.getByRole("link", { name: "Open in wallet" })
    ).toHaveAttribute("href", transactionURL)
    expect(
      screen.getByText("19.99 USD1 · Watching for payment…")
    ).toBeInTheDocument()
  })

  it("resumes a pending Solana request without creating another", async () => {
    const transactionURL =
      "solana:recipient?amount=8.50&spl-token=mint&reference=reference"
    const pay = vi.fn<CheckoutSource["pay"]>()
    const source: CheckoutSource = {
      async getSession() {
        return fixtureSession({
          status: "requires_action",
          transaction_url: transactionURL,
          rails: [checkoutOption("solana")],
        })
      },
      pay,
    }

    render(<Checkout source={source} />)

    expect(
      await screen.findByRole("img", { name: "Solana Pay QR code" })
    ).toBeInTheDocument()
    expect(
      screen.getByText("8.50 USDC · Watching for payment…")
    ).toBeInTheDocument()
    expect(pay).not.toHaveBeenCalled()
  })

  it("polls a pending Solana request through settlement", async () => {
    vi.useFakeTimers()
    const transactionURL =
      "solana:recipient?amount=5.00&spl-token=mint&reference=reference"
    const created = fixtureSession({
      rails: [checkoutOption("solana")],
    })
    const succeeded = fixtureSession({
      status: "succeeded",
      payment_id: "pay_solana",
      rails: [],
    })
    const getSession = vi
      .fn<CheckoutSource["getSession"]>()
      .mockResolvedValueOnce(created)
      .mockResolvedValueOnce(succeeded)
    const pay = vi.fn<CheckoutSource["pay"]>().mockResolvedValue({
      status: "requires_action",
      transaction_url: transactionURL,
    })
    const onComplete = vi.fn()

    render(<Checkout source={{ getSession, pay }} onComplete={onComplete} />)

    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(pay).toHaveBeenCalledOnce()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_000)
    })

    expect(screen.getAllByText("Payment complete").length).toBeGreaterThan(0)
    expect(onComplete).toHaveBeenCalledWith({
      status: "succeeded",
      payment_id: "pay_solana",
      subscription_id: undefined,
    })
  })
})

describe("CheckoutModal", () => {
  it("hosts the flow with inline callbacks without update loops", async () => {
    render(
      <CheckoutModal
        open
        onOpenChange={() => {}}
        source={createFixtureSource()}
        onPhaseChange={() => {}}
      />
    )
    await waitFor(() => {
      expect(screen.getByText("Pay $99.00")).toBeInTheDocument()
    })
    const dialog = screen.getByRole("dialog")
    const overlay = document.querySelector('[data-slot="dialog-overlay"]')
    expect(overlay?.parentElement).toBe(dialog.parentElement)
    expect(dialog.parentElement).toHaveAttribute("data-slot", "dialog-portal")
    expect(dialog).toHaveClass(
      "orck",
      "w-[860px]",
      "max-h-[calc(100dvh-2rem)]",
      "overflow-hidden",
      "ring-0",
      "[&>[data-slot=dialog-close]]:top-5",
      "[&>[data-slot=dialog-close]]:right-5"
    )
    expect(dialog.querySelector(".overflow-y-auto")).toHaveClass(
      "max-h-[calc(100dvh-6rem)]",
      "touch-pan-y",
      "overscroll-contain",
      "pr-6",
      "[scrollbar-width:none]",
      "[-webkit-overflow-scrolling:touch]",
      "[&::-webkit-scrollbar]:hidden"
    )
  })

  it("themes the modal shell and close control in dark mode", async () => {
    render(
      <CheckoutModal
        open
        onOpenChange={() => {}}
        source={createFixtureSource()}
        appearance={{
          theme: "dark",
          variables: {
            primary: "oklch(0.7 0.2 250)",
            radius: "1rem",
          },
        }}
      />
    )

    await screen.findByText("Pay $99.00")
    const dialog = screen.getByRole("dialog")
    expect(dialog).toHaveAttribute("data-orck-theme", "dark")
    expect(dialog.style.getPropertyValue("--primary")).toBe(
      "oklch(0.7 0.2 250)"
    )
    expect(dialog.style.getPropertyValue("--radius")).toBe("1rem")
    expect(dialog).toHaveClass(
      "bg-background",
      "text-foreground",
      "[&>[data-slot=dialog-close]]:text-foreground"
    )
    expect(screen.getByRole("button", { name: "Close" })).toBeVisible()
  })

  it("preloads and remounts aligned Collect.js fields when Card is reselected", async () => {
    const configure = vi.fn()
    configure.mockImplementation((config: Record<string, unknown>) => {
      const fieldsAvailable = config.fieldsAvailableCallback
      if (typeof fieldsAvailable === "function") fieldsAvailable()
    })
    window.CollectJS = {
      configure,
      startPaymentRequest: vi.fn(),
    }
    const source: CheckoutSource = {
      async getSession() {
        return fixtureSession({
          rails: [
            checkoutOption("stripe"),
            checkoutOption("nmi", {
              tokenization_key: "public_test_key",
              tokenization_url: "https://secure.nmi.com/token/Collect.js",
            }),
          ],
        })
      },
      async pay() {
        return { status: "succeeded" }
      },
    }

    render(<CheckoutModal open onOpenChange={() => {}} source={source} />)

    expect(await screen.findByText("Stripe")).toBeInTheDocument()
    expect(screen.getByText("Card").closest("label")).not.toHaveClass(
      "font-semibold"
    )
    const script = await waitFor(() => {
      const element = document.getElementById("openrails-collectjs")
      expect(element).toBeInstanceOf(HTMLScriptElement)
      return element as HTMLScriptElement
    })
    expect(Object.keys(script.dataset)).toEqual(["tokenizationKey"])
    fireEvent.load(script)

    expect(configure).toHaveBeenLastCalledWith(
      expect.objectContaining({
        customCss: expect.objectContaining({
          height: "38px",
          "line-height": "38px",
          padding: "0 8px",
          "background-color": expect.any(String),
          color: expect.any(String),
        }),
        fields: {
          ccnumber: {
            selector: expect.stringMatching(/^#orck-.+-cc-number$/),
            title: "Card number",
            placeholder: "1234 1234 1234 1234",
          },
          ccexp: {
            selector: expect.stringMatching(/^#orck-.+-cc-expiry$/),
            title: "Expiration date",
            placeholder: "MM / YY",
          },
          cvv: {
            selector: expect.stringMatching(/^#orck-.+-cc-cvv$/),
            title: "Card security code",
            placeholder: "CVC",
          },
        },
      })
    )
    expect(configure).toHaveBeenCalledTimes(1)

    fireEvent.click(screen.getByText("Card"))
    await waitFor(() => expect(configure).toHaveBeenCalledTimes(2))

    fireEvent.click(screen.getByText("Stripe"))
    expect(configure).toHaveBeenCalledTimes(2)

    fireEvent.click(screen.getByText("Card"))
    await waitFor(() => expect(configure).toHaveBeenCalledTimes(3))
  })

  it("finishes loading Collect.js after the modal closes and reopens", async () => {
    const configure = vi.fn()
    const source: CheckoutSource = {
      async getSession() {
        return fixtureSession({
          rails: [
            checkoutOption("nmi", {
              tokenization_key: "public_test_key",
              tokenization_url: "https://payments.example.test/collect.js",
            }),
          ],
        })
      },
      async pay() {
        return { status: "succeeded" }
      },
    }

    const view = render(
      <CheckoutModal open onOpenChange={() => {}} source={source} />
    )
    const script = await waitFor(() => {
      const element = document.getElementById("openrails-collectjs")
      expect(element).toBeInstanceOf(HTMLScriptElement)
      return element as HTMLScriptElement
    })

    view.rerender(
      <CheckoutModal open={false} onOpenChange={() => {}} source={source} />
    )
    view.rerender(
      <CheckoutModal open onOpenChange={() => {}} source={source} />
    )
    await screen.findByText("Card")

    window.CollectJS = {
      configure,
      startPaymentRequest: vi.fn(),
    }
    fireEvent.load(script)

    await waitFor(() => expect(configure).toHaveBeenCalledOnce())
    expect(
      screen.queryByText("Card fields failed to load")
    ).not.toBeInTheDocument()
  })

  it("uses settled copy in a modal instead of claiming it will redirect", async () => {
    const source = createFixtureSource({
      session: { rails: [checkoutOption("stripe")] },
      payResult: { status: "succeeded" },
      payDelayMs: 1,
    })

    render(<CheckoutModal open onOpenChange={() => {}} source={source} />)
    fireEvent.click(
      await screen.findByRole("button", { name: "Continue to Stripe" })
    )

    expect(await screen.findByText("You're all set.")).toBeInTheDocument()
    expect(screen.queryByText(/Returning to/)).not.toBeInTheDocument()
  })
})
