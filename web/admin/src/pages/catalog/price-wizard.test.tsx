// @vitest-environment jsdom
import { QueryClientProvider } from "@tanstack/react-query"
import { afterEach, expect, it } from "vitest"
import { aPrice, client, server } from "@/test/harness"
import { browserEnvironment, mount, unmount } from "@/test/mount"
import { PriceChangeWizard } from "./price-wizard"

afterEach(unmount)

it.each([
  [undefined, false, true],
  ["premium", false, false],
  ["premium", true, true],
] as const)(
  "allows a price change only after its current product is loaded (%s, archived: %s)",
  async (productKey, archived, disabled) => {
    browserEnvironment()
    await server()
    const queries = client()
    await mount(
      <QueryClientProvider client={queries}>
        <PriceChangeWizard
          price={aPrice("price_1", "prod_1", { key: "monthly", archived })}
          productName="Premium"
          productKey={productKey}
        />
      </QueryClientProvider>
    )
    const button = document.querySelector<HTMLButtonElement>("button")
    expect(button?.textContent).toBe("Change price")
    expect(button?.disabled).toBe(disabled)
    queries.clear()
  }
)

it("keeps customer-selected deposit limits out of the fixed-price wizard", async () => {
  browserEnvironment()
  await server()
  const queries = client()
  await mount(
    <QueryClientProvider client={queries}>
      <PriceChangeWizard
        price={aPrice("price_deposit", "prod_1", {
          unit_amount: "0",
          customer_amount: { min_amount: "1000000", max_amount: "500000000" },
        })}
        productName="API credit"
        productKey="api-credit"
      />
    </QueryClientProvider>
  )
  const button = document.querySelector<HTMLButtonElement>("button")
  expect(button?.disabled).toBe(true)
  expect(button?.title).toContain("Change its limits")
  queries.clear()
})
