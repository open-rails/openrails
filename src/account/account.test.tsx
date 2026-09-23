import {
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
  subscription,
  type FakeBilling,
} from "../test/billing-server"
import { AccountBilling } from "./account-billing"
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
      <BillingProvider client={client} settle={{ intervalMs: 1, attempts: 5 }}>
        {ui}
      </BillingProvider>
    </BillingUiProvider>
  )
}

describe("AccountBilling", () => {
  it("renders subscriptions, cards and history from the wire", async () => {
    const server = fakeBilling({
      methods: [
        paymentMethod({
          subscriptions: [{ id: "sub_1", display_name: "Pro" }],
          collection_default_currencies: ["USD"],
        }),
      ],
      payments: [
        payment(),
        payment({
          id: "pay_2",
          object: "refund",
          status: "succeeded",
          amount: "-9990000",
        }),
      ],
    })
    mount(<AccountBilling />, server)

    const sub = await screen.findByTestId("subscription-row")
    expect(sub).toHaveTextContent("Pro")
    expect(sub).toHaveTextContent("Active")
    await waitFor(() => expect(sub).toHaveTextContent("$9.99 every month"))
    expect(sub).toHaveTextContent("Renews Sep 16, 2036")
    expect(sub).toHaveTextContent("Visa •••• 4242")

    const card = await screen.findByTestId("payment-method-row")
    expect(card).toHaveTextContent("Visa ending 4242")
    expect(card).toHaveTextContent("Expires 12/30 · Used by Pro")
    expect(card).toHaveTextContent("Default for USD")

    const rows = await screen.findAllByTestId("payment-row")
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
    server.fail["GET /me/subscriptions"] = apiError(503, "service_unavailable")
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

  it("cancels with feedback, then resumes", async () => {
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
          psp_id: "99999999-9999-9999-9999-999999999999",
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
    fireEvent.click(within(dialog).getByText("Mastercard ending 5454"))
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
    expect(within(dialog).queryByRole("textbox")).toBeNull()
    fireEvent.click(
      within(dialog).getByRole("button", { name: "Sign with wallet" })
    )
    await waitFor(() => expect(row).toHaveTextContent("Cancelled"))
    expect(send).toHaveBeenCalledWith("dHg=")
  })
})

describe("PaymentMethodsPanel", () => {
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
      within(rows[0]).getByRole("button", { name: "Remove Visa ending 4242" })
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
        { name: "Remove Visa ending 4242" }
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
  const keys = (tree: Tree, prefix = ""): string[] =>
    Object.entries(tree).flatMap(([k, v]) =>
      typeof v === "string" ? [`${prefix}${k}`] : keys(v, `${prefix}${k}.`)
    )
  const english = keys(en).sort()

  it.each([
    ["de", de],
    ["es", es],
    ["ja", ja],
    ["ko", ko],
    ["zh", zh],
  ])("%s covers every English key", (_, bundle) => {
    expect(keys(bundle as Tree).sort()).toEqual(english)
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
    await waitFor(() => expect(row).toHaveTextContent("monatlich"))
  })

  it("labels unknown statuses readably", () => {
    render(<BillingStatusBadge status="on_hold" />)
    expect(screen.getByText("on hold")).toBeInTheDocument()
  })
})
