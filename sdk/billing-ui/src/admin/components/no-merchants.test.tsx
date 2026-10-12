import { renderToStaticMarkup } from "react-dom/server"
import { expect, it } from "vitest"
import { BillingAdmin } from "../billing-admin"

const auth = { authFetch: async () => Response.json({}) }
it("renders the host's empty state when its merchant directory is empty", () => {
  const html = renderToStaticMarkup(
    <BillingAdmin
      baseUrl="/billing/v1"
      auth={auth}
      merchants={[]}
      emptyState={<p>Accept your team invitation.</p>}
    />
  )
  expect(html).toContain("Accept your team invitation.")
  expect(html).not.toContain("Customers")
})
it("offers the host's merchant-creation link without a console extension", () => {
  const html = renderToStaticMarkup(
    <BillingAdmin
      baseUrl="/billing/v1"
      auth={auth}
      merchants={[]}
      newMerchantHref="/teams/new"
    />
  )
  expect(html).toContain('href="/teams/new"')
  expect(html).toContain("Select a merchant")
})
