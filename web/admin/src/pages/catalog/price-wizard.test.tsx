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
