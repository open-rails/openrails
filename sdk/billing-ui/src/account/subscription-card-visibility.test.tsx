import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { createBillingClient } from "../client/client"
import { BillingProvider } from "../react/provider"
import { fakeBilling, subscription } from "../test/billing-server"
import { SubscriptionsPanel } from "./subscriptions-panel"

describe("subscription card visibility", () => {
  it.each([
    { status: "canceled", cancel_scheduled: false },
    { status: "active", cancel_scheduled: true },
  ])(
    "shows access without a card for a nonrenewing subscription: %j",
    async (state) => {
      const server = fakeBilling({ subscriptions: [subscription(state)] })
      render(
        <BillingProvider client={createBillingClient({ fetch: server.fetch })}>
          <SubscriptionsPanel />
        </BillingProvider>
      )
      const row = await screen.findByTestId("subscription-row")
      expect(row).toHaveTextContent("Access until Sep 16, 2036")
      expect(row).not.toHaveTextContent("Visa")
      expect(row).not.toHaveTextContent("4242")
    }
  )

  it.each([
    { card: { brand: "visa", last4: "4242" }, showsCard: true },
    { card: null, showsCard: false },
  ])(
    "shows only the usable renewal card: $showsCard",
    async ({ card, showsCard }) => {
      const server = fakeBilling({ subscriptions: [subscription({ card })] })
      render(
        <BillingProvider client={createBillingClient({ fetch: server.fetch })}>
          <SubscriptionsPanel />
        </BillingProvider>
      )
      const row = await screen.findByTestId("subscription-row")
      expect(row).toHaveTextContent("Renews Sep 16, 2036")
      expect(row.textContent?.includes("Visa •••• 4242")).toBe(showsCard)
    }
  )
})
