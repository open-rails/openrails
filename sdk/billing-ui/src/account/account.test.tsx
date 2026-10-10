import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "../client/client"
import { de } from "../locales/de"
import { en } from "../locales/en"
import { es } from "../locales/es"
import { ja } from "../locales/ja"
import { ko } from "../locales/ko"
import { zh } from "../locales/zh"
import { BillingUiProvider, type BillingUiProviderProps } from "../provider"
import { BillingProvider } from "../react/provider"
import {
  apiError,
  fakeBilling,
  payment,
  paymentMethod,
  price,
  subscription,
  type FakeBilling,
} from "../test/billing-server"
import { AccountBilling } from "./account-billing"
import { PaymentHistory } from "./payment-history"
import { PaymentMethodsPanel } from "./payment-methods-panel"
import { BillingStatusBadge } from "./status-badge"
import { SubscriptionsPanel } from "./subscriptions-panel"

function mount(
  ui: ReactNode,
  server: FakeBilling,
  props: Partial<BillingUiProviderProps> = {}
) {
  const client = createBillingClient({ fetch: server.fetch })
  return render(
    <BillingUiProvider locale="en-US" {...props}>
      <BillingProvider client={client}>{ui}</BillingProvider>
    </BillingUiProvider>
  )
}

describe("AccountBilling", () => {
  it("renders subscriptions, cards and history from the wire", async () => {
    const server = fakeBilling({
      methods: [
        paymentMethod({
          subscriptions: [{ id: "sub_1", display_name: "Pro" }],
          default_currencies: ["USD"],
        }),
      ],
      payments: [
        payment({
          subscription_id: "sub_1",
          price: price(720, 720),
          product: { id: "prod_1", display_name: "Pro" },
        }),
        payment({
          id: "pay_2",
          kind: "refund",
          status: "succeeded",
          amount: "-9990000",
        }),
      ],
    })
    mount(<AccountBilling />, server)

    const sub = await screen.findByTestId("subscription-row")
    expect(sub).toHaveTextContent("Pro")
    expect(sub).toHaveTextContent("Active")
    await waitFor(() => expect(sub).toHaveTextContent("$9.99 every 30 days"))
    expect(sub).toHaveTextContent("Renews Sep 16, 2036")
    expect(sub).toHaveTextContent("Visa •••• 4242")

    const card = await screen.findByTestId("payment-method-row")
    expect(card).toHaveTextContent("Visa •••• 4242 · 12/30")
    expect(card).toHaveTextContent("Used by Pro")
    expect(card).toHaveTextContent("Default for USD")

    const rows = await screen.findAllByTestId("payment-row")
    expect(rows[0]).toHaveTextContent("Pro")
    expect(rows[0]).toHaveTextContent("every 30 days")
    expect(rows[0]).toHaveTextContent("Paid")
    expect(rows[0]).toHaveTextContent("$9.99")
    expect(rows[1]).toHaveTextContent("Refund")
    expect(rows[1]).toHaveTextContent("-$9.99")
  })

  it("shows empty states and routes the plans link through navigate", async () => {
    const server = fakeBilling({ subscriptions: [], methods: [], payments: [] })
    const navigate = vi.fn()
    mount(<AccountBilling plansHref="/plans" />, server, { navigate })
    fireEvent.click(await screen.findByRole("link", { name: "Browse plans" }))
    expect(navigate).toHaveBeenCalledWith("/plans")
    expect(await screen.findByText("No saved cards")).toBeInTheDocument()
    expect(await screen.findByText("No payments yet")).toBeInTheDocument()
  })

  it("surfaces a load failure with a retry", async () => {
    const server = fakeBilling()
    server.fail["GET /me/subscriptions"] = apiError(500, "service_unavailable")
    mount(<SubscriptionsPanel />, server)
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Billing is temporarily unavailable"
    )
    fireEvent.click(screen.getByRole("button", { name: "Try again" }))
    expect(await screen.findByTestId("subscription-row")).toBeInTheDocument()
  })
})

describe("SubscriptionsPanel", () => {
  it("renders host footer content under each row", async () => {
    mount(
      <SubscriptionsPanel
        renderSubscriptionFooter={(s) => <span>plan for {s.id}</span>}
      />,
      fakeBilling()
    )
    const row = await screen.findByTestId("subscription-row")
    expect(within(row).getByTestId("subscription-footer")).toHaveTextContent(
      "plan for sub_"
    )
  })

  it("carries the panel appearance into its dialogs", async () => {
    mount(<SubscriptionsPanel appearance={{ theme: "dark" }} />, fakeBilling())
    const row = await screen.findByTestId("subscription-row")
    fireEvent.click(within(row).getByRole("button", { name: "Cancel Pro" }))
    expect(await screen.findByRole("alertdialog")).toHaveAttribute(
      "data-orck-theme",
      "dark"
    )
  })

  it("does not infer canceled access from a future billing date", async () => {
    mount(<SubscriptionsPanel />, fakeBilling({ subscriptions: [subscription({
      status: "canceled", current_period_ends_at: "2036-10-01T12:00:00Z",
      cancel_scheduled: true, resumable: true, access: null,
    })] }))
    const row = await screen.findByTestId("subscription-row")
    expect(row).toHaveTextContent("No further payments")
    expect(row).not.toHaveTextContent("Access until")
  })

  it.each([null, "2036-10-01T12:00:00Z"])("keeps paid access visible after cancellation with expiry %s", async (endsAt) => {
    mount(<SubscriptionsPanel />, fakeBilling({ subscriptions: [subscription({
      status: "canceled", canceled_at: "2025-01-01T00:00:00Z",
      current_period_ends_at: "2025-02-01T00:00:00Z",
      cancel_scheduled: false, resumable: false,
      access: { starts_at: "2025-01-01T00:00:00Z", ends_at: endsAt },
    })] }))
    const row = await screen.findByTestId("subscription-row")
    expect(row).toHaveTextContent(endsAt ? "Access until Oct 1, 2036" : "No further payments")
    expect(row).not.toHaveTextContent("Ended")
    expect(within(row).queryByRole("button", { name: "Resume" })).not.toBeInTheDocument()
  })

  it("cancels with a reason, then resumes", async () => {
    const server = fakeBilling()
    mount(<SubscriptionsPanel />, server)
    const row = await screen.findByTestId("subscription-row")
    fireEvent.click(within(row).getByRole("button", { name: "Cancel Pro" }))

    const dialog = await screen.findByRole("alertdialog")
    expect(dialog).toHaveTextContent("Cancel Pro?")
    const confirm = within(dialog).getByRole("button", {
      name: "Cancel subscription",
    })
    fireEvent.click(confirm)
    expect(
      await within(dialog).findByText("Enter at least 4 characters.")
    ).toBeInTheDocument()
    expect(server.calls).not.toContain(
      "POST /me/subscriptions/sub_cccccccc-cccc-4ccc-8ccc-cccccccccccc/cancel"
    )

    fireEvent.change(within(dialog).getByLabelText("Why are you cancelling?"), {
      target: { value: "Too expensive" },
    })
    fireEvent.click(confirm)
    await waitFor(() =>
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    )
    expect(await screen.findByText("Cancellation requested.")).toBeVisible()
    await waitFor(() => expect(row).toHaveTextContent("Ending"))
    expect(row).toHaveTextContent("Access until Sep 16, 2036")

    fireEvent.click(within(row).getByRole("button", { name: "Resume" }))
    await waitFor(() => expect(row).toHaveTextContent("Active"))
    expect(
      await screen.findByText("Your subscription will continue.")
    ).toBeVisible()
  })

  it("switches the card a subscription renews on", async () => {
    const server = fakeBilling({
      methods: [
        paymentMethod({ id: "pm_dddddddd-dddd-4ddd-8ddd-dddddddddddd" }),
        paymentMethod({
          id: "pm_2",
          card: {
            brand: "mastercard",
            last4: "5454",
            exp_month: 1,
            exp_year: 2031,
          },
        }),
        paymentMethod({
          id: "pm_other_psp",
          psp_id: "psp_99999999-9999-9999-9999-999999999999",
          card: { brand: "amex", last4: "0005" },
        }),
      ],
    })
    const onChange = vi.fn()
    const client = createBillingClient({ fetch: server.fetch })
    render(
      <BillingUiProvider locale="en-US">
        <BillingProvider client={client} onChange={onChange}>
          <SubscriptionsPanel />
        </BillingProvider>
      </BillingUiProvider>
    )
    const row = await screen.findByTestId("subscription-row")
    fireEvent.click(
      within(row).getByRole("button", { name: "Change card for Pro" })
    )
    const dialog = await screen.findByRole("dialog")
    await waitFor(() =>
      expect(within(dialog).getAllByTestId("change-card-option")).toHaveLength(
        2
      )
    )
    expect(dialog).not.toHaveTextContent("0005")
    const use = within(dialog).getByRole("button", { name: "Use this card" })
    expect(use).toBeDisabled()
    fireEvent.click(within(dialog).getByText(/^Mastercard •••• 5454/))
    fireEvent.click(use)
    await waitFor(() =>
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument()
    )
    expect(await screen.findByText("Payment card updated.")).toBeVisible()
    expect(server.calls).toContain(
      "PUT /me/subscriptions/sub_cccccccc-cccc-4ccc-8ccc-cccccccccccc/payment-method"
    )
    await waitFor(() => expect(row).toHaveTextContent("Mastercard •••• 5454"))
    expect(onChange).toHaveBeenCalledWith({
      type: "subscription.payment_method_changed",
      subscriptionId: "sub_cccccccc-cccc-4ccc-8ccc-cccccccccccc",
      paymentMethodId: "pm_2",
    })
  })

  it("moves a subscription back onto the default card", async () => {
    const server = fakeBilling({
      subscriptions: [subscription({ payment_method_id: "pm_2" })],
      methods: [
        paymentMethod({
          id: "pm_dddddddd-dddd-4ddd-8ddd-dddddddddddd",
          default_currencies: ["USD"],
        }),
        paymentMethod({
          id: "pm_2",
          card: { brand: "mastercard", last4: "5454" },
        }),
      ],
    })
    mount(<SubscriptionsPanel />, server)
    const row = await screen.findByTestId("subscription-row")
    fireEvent.click(
      within(row).getByRole("button", { name: "Change card for Pro" })
    )
    const dialog = await screen.findByRole("dialog")
    fireEvent.click(
      await within(dialog).findByText(/^Your default card \(Visa •••• 4242/)
    )
    fireEvent.click(within(dialog).getByRole("button", { name: "Use this card" }))
    await waitFor(() =>
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument()
    )
    await waitFor(() => expect(row).toHaveTextContent("Visa •••• 4242"))
    expect(server.calls).toContain(
      "PUT /me/subscriptions/sub_cccccccc-cccc-4ccc-8ccc-cccccccccccc/payment-method"
    )
  })

  it("scopes no palette under the inherit theme", async () => {
    const server = fakeBilling()
    mount(<SubscriptionsPanel />, server, { appearance: { theme: "inherit" } })
    const panel = await screen.findByTestId("subscriptions-panel")
    expect(panel).toHaveAttribute("data-orck-theme", "inherit")
  })

  it("links out for provider-managed cancellation", async () => {
    const server = fakeBilling({
      subscriptions: [
        subscription({
          rail: "ccbill",
          cancel_mode: "external_portal",
          cancel_portal_url: "https://support.ccbill.com/",
        }),
      ],
    })
    mount(<SubscriptionsPanel />, server)
    const row = await screen.findByTestId("subscription-row")
    expect(within(row).getByRole("link", { name: "Manage" })).toHaveAttribute(
      "href",
      "https://support.ccbill.com/"
    )
    expect(within(row).queryByRole("button", { name: /Cancel/ })).toBeNull()
  })

  it("cancels a Solana subscription through the wallet", async () => {
    const server = fakeBilling({
      subscriptions: [
        subscription({ id: "sub_sol", rail: "solana", card: null }),
      ],
    })
    const send = vi.fn(async () => "sig")
    mount(<SubscriptionsPanel sendSolanaTransaction={send} />, server)
    const row = await screen.findByTestId("subscription-row")
    expect(row).toHaveTextContent("Solana wallet")
    fireEvent.click(within(row).getByRole("button", { name: "Cancel Pro" }))
    const dialog = await screen.findByRole("alertdialog")
    fireEvent.change(within(dialog).getByLabelText("Why are you cancelling?"), {
      target: { value: "Too expensive" },
    })
    fireEvent.click(
      within(dialog).getByRole("button", { name: "Sign with wallet" })
    )
    await waitFor(() => expect(row).toHaveTextContent("Canceled"))
    expect(send).toHaveBeenCalledWith("dHg=")
  })
})

describe("PaymentHistory", () => {
  it("names what was bought, falling back before OpenRails sends product", async () => {
    const server = fakeBilling({
      payments: [
        payment({
          price: price(168, 168),
          product: { id: "prod_1", display_name: "Weekly pass" },
        }),
        payment({ id: "pay_2", subscription_id: "sub_1", price: price(720, 720) }),
        payment({ id: "pay_3", price: price(null, null) }),
        payment({
          id: "pay_4",
          price: price(null, null),
          product: { id: "prod_2", display_name: "Post purchase" },
        }),
      ],
    })
    mount(<PaymentHistory />, server)
    const rows = await screen.findAllByTestId("payment-row")
    const item = (i: number) => within(rows[i]).getByTestId("payment-item")
    const period = (i: number) =>
      within(rows[i]).queryByTestId("payment-period")?.textContent ?? null
    expect(rows.map((_, i) => item(i).textContent)).toEqual([
      "Weekly pass",
      "Subscription",
      "Purchase",
      "Post purchase",
    ])
    expect(rows.map((_, i) => period(i))).toEqual([
      "every week",
      "every 30 days",
      null,
      null,
    ])
  })

  it("localizes the fallback name and period", async () => {
    const server = fakeBilling({
      payments: [payment({ subscription_id: "sub_1", price: price(720, 720) })],
    })
    mount(<PaymentHistory />, server, { locale: "ja-JP", messages: ja })
    const row = await screen.findByTestId("payment-row")
    expect(within(row).getByTestId("payment-item")).toHaveTextContent(
      "サブスクリプション"
    )
    expect(within(row).getByTestId("payment-period")).toHaveTextContent(
      "30日ごと"
    )
  })
})

describe("PaymentMethodsPanel", () => {
  // An NMI PSP whose card_entry is server: the page posts the card itself.
  const cardPsp = {
    psp_id: "psp_55555555-5555-5555-5555-555555555555",
    key: "mobius",
    rail: "nmi",
    custodian: "psp",
    display_name: "Card",
    flow: "card",
    checkout: true,
  }

  it("reads the payment configuration itself and saves a card", async () => {
    const server = fakeBilling({ methods: [], psps: [cardPsp] })
    mount(<AccountBilling defaultCountry="US" />, server)
    fireEvent.click(await screen.findByRole("button", { name: "Add card" }))
    const dialog = await screen.findByRole("dialog")
    fireEvent.change(within(dialog).getByLabelText("Name on card"), {
      target: { value: "Pat Reader" },
    })
    fireEvent.change(within(dialog).getByLabelText("ZIP code"), {
      target: { value: "94107" },
    })
    fireEvent.change(within(dialog).getByLabelText("Card number"), {
      target: { value: "4111111111111111" },
    })
    fireEvent.change(within(dialog).getByLabelText("Expiry"), {
      target: { value: "1027" },
    })
    fireEvent.change(within(dialog).getByLabelText("CVC"), {
      target: { value: "999" },
    })
    const save = within(dialog).getByRole("button", { name: "Save card" })
    await waitFor(() => expect(save).toBeEnabled())
    fireEvent.click(save)
    expect(await screen.findByText("Card saved.")).toBeInTheDocument()
    expect(await screen.findByTestId("payment-method-row")).toHaveTextContent(
      "Mastercard •••• 5454"
    )
    expect(server.calls.filter((call) => call === "GET /config")).toHaveLength(
      1
    )
    expect(server.calls).toContain("POST /me/payment-methods")
  })

  it("offers no card entry while the configuration fails, and retries", async () => {
    const server = fakeBilling({ psps: [cardPsp] })
    server.fail["GET /config"] = apiError(500, "internal_error")
    mount(<PaymentMethodsPanel />, server)
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Adding a card is temporarily unavailable."
    )
    expect(screen.queryByRole("button", { name: "Add card" })).toBeNull()
    fireEvent.click(screen.getByRole("button", { name: "Try again" }))
    expect(
      await screen.findByRole("button", { name: "Add card" })
    ).toBeInTheDocument()
    expect(screen.queryByRole("alert")).toBeNull()
  })

  it("asks again after a temporarily unavailable PSP's retry_after", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const server = fakeBilling({
        psps: [
          { ...cardPsp, status: "temporarily_unavailable", retry_after: 30 },
        ],
      })
      mount(<PaymentMethodsPanel />, server)
      const note = "Adding a card is temporarily unavailable."
      expect(await screen.findByText(note)).toHaveAttribute("role", "status")
      expect(screen.queryByRole("button", { name: "Add card" })).toBeNull()
      server.psps = [cardPsp]
      await act(() => vi.advanceTimersByTimeAsync(30_000))
      expect(
        await screen.findByRole("button", { name: "Add card" })
      ).toBeInTheDocument()
      expect(screen.queryByText(note)).toBeNull()
      expect(
        server.calls.filter((call) => call === "GET /config")
      ).toHaveLength(2)
    } finally {
      vi.useRealTimers()
    }
  })

  it("explains an in-use card and removes a free one", async () => {
    const server = fakeBilling({
      methods: [paymentMethod(), paymentMethod({ id: "pm_2" })],
    })
    mount(<AccountBilling defaultCurrency="usd" />, server)
    const rows = await screen.findAllByTestId("payment-method-row")
    server.fail["DELETE /me/payment-methods/pm_1"] = apiError(
      409,
      "resource_conflict"
    )
    fireEvent.click(
      within(rows[0]).getByRole("button", { name: /^Remove Visa •••• 4242/ })
    )
    let dialog = await screen.findByRole("alertdialog")
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove card" }))
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "pays for an active subscription"
    )
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }))

    fireEvent.click(
      within(rows[1]).getByRole("button", { name: "Make default" })
    )
    await waitFor(() =>
      expect(screen.getAllByTestId("payment-method-row")[1]).toHaveTextContent(
        "Default for USD"
      )
    )

    fireEvent.click(
      within(screen.getAllByTestId("payment-method-row")[0]).getByRole(
        "button",
        { name: /^Remove Visa •••• 4242/ }
      )
    )
    dialog = await screen.findByRole("alertdialog")
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove card" }))
    await waitFor(() =>
      expect(screen.getAllByTestId("payment-method-row")).toHaveLength(1)
    )
    expect(await screen.findByText("Card removed.")).toBeVisible()
  })
})

describe("messages", () => {
  type Tree = { [key: string]: string | Tree }
  const plural = (v: Tree) => typeof v.other === "string"
  // Plural nodes count as one key; their forms are checked per locale below.
  const keys = (tree: Tree, prefix = ""): string[] =>
    Object.entries(tree).flatMap(([k, v]) =>
      typeof v === "string" || plural(v)
        ? [`${prefix}${k}`]
        : keys(v, `${prefix}${k}.`)
    )
  const plurals = (tree: Tree): Tree[] =>
    Object.values(tree).flatMap((v) =>
      typeof v === "string" ? [] : plural(v) ? [v] : plurals(v)
    )
  const english = keys(en).sort()
  const bundles = [
    ["en", en],
    ["de", de],
    ["es", es],
    ["ja", ja],
    ["ko", ko],
    ["zh", zh],
  ] as const

  it.each(bundles)("%s covers every English key", (_, bundle) => {
    expect(keys(bundle as Tree).sort()).toEqual(english)
  })

  // A form English has but the locale lacks would fall back to English.
  it.each(bundles)("%s spells each plural form it uses", (locale, bundle) => {
    const categories = new Intl.PluralRules(locale).resolvedOptions()
      .pluralCategories
    for (const node of plurals(bundle as Tree)) {
      expect(node.other).toContain("{count}")
      expect("one" in node).toBe(categories.includes("one"))
    }
  })

  it("renders a locale bundle and the host override", async () => {
    const server = fakeBilling()
    mount(<SubscriptionsPanel />, server, {
      locale: "de-DE",
      messages: [de, { subscriptions: { title: "Mitgliedschaft" } }],
    })
    expect(await screen.findByText("Mitgliedschaft")).toBeInTheDocument()
    const row = await screen.findByTestId("subscription-row")
    expect(row).toHaveTextContent("Aktiv")
    await waitFor(() => expect(row).toHaveTextContent("alle 30 Tage"))
  })

  it("labels unknown statuses readably", () => {
    render(<BillingStatusBadge status="on_hold" />)
    expect(screen.getByText("on hold")).toBeInTheDocument()
  })
})
