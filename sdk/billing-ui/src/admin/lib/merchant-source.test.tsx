import { render, screen, within } from "@testing-library/react"
import { expect, it } from "vitest"
import { BillingAdmin } from "../billing-admin"

it("uses the supplied merchant directory independently in each mounted instance", async () => {
  const auth = {
    authFetch: async () =>
      Response.json({
        admin: "none",
        catalog: false,
        merchant_config: false,
        metrics: false,
        scope_id: "",
        assistants: {},
      }),
  }
  render(
    <>
      <section data-testid="acme">
        <BillingAdmin
          baseUrl="/first/v1"
          auth={auth}
          merchants={[{ id: "a", slug: "acme", display_name: "Acme shop" }]}
        />
      </section>
      <section data-testid="bravo">
        <BillingAdmin
          baseUrl="/second/v1"
          auth={auth}
          merchants={[{ id: "b", slug: "bravo", display_name: "Bravo shop" }]}
        />
      </section>
    </>
  )
  expect(
    await within(screen.getByTestId("acme")).findByText("Acme shop")
  ).toBeVisible()
  expect(
    await within(screen.getByTestId("bravo")).findByText("Bravo shop")
  ).toBeVisible()
  expect(
    within(screen.getByTestId("acme")).queryByText("Bravo shop")
  ).toBeNull()
  expect(
    within(screen.getByTestId("bravo")).queryByText("Acme shop")
  ).toBeNull()
})
