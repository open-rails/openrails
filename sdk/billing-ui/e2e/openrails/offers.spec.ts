// <Offers keys> against the real server: the public catalog looks the
// seeded products up by key in one read, a BuyButton per price, and a
// signed-out visitor is sent to sign in. The catalog takes keys alone.
import { expect, test } from "@playwright/test"

test("Offers looks products up by key", async ({ page }) => {
  const reads: string[] = []
  page.on("request", (r) => {
    const url = new URL(r.url())
    if (url.pathname.endsWith("/catalog/products")) reads.push(url.search)
  })
  await page.goto("/offers.html#keys=e2e-membership,e2e-lifetime")

  await expect(page.getByTestId("offer")).toHaveCount(2)
  await expect(page.getByRole("heading", { name: "Membership" })).toBeVisible()
  await expect(
    page.getByRole("heading", { name: "Lifetime pass" })
  ).toBeVisible()
  await expect(
    page.getByRole("button", { name: "$9.99 every 30 days" })
  ).toBeVisible()
  await page.getByRole("button", { name: "$19.99" }).click()
  await expect
    .poll(() =>
      page.evaluate(() => (window as unknown as { signIns?: string[] }).signIns)
    )
    .toEqual(["sign-in"])
  expect(reads).toEqual(["?keys=e2e-membership&keys=e2e-lifetime"])
})

test("the public catalog takes product keys alone", async ({ request }) => {
  const found = await request.get(
    "/billing/v1/catalog/products?keys=e2e-lifetime&keys=no-such-product"
  )
  expect(found.status()).toBe(200)
  const page = (await found.json()) as {
    data: { key: string }[]
    next_cursor: string | null
  }
  expect(page.data.map((p) => p.key)).toEqual(["e2e-lifetime"])
  expect(page.next_cursor).toBeNull()

  const tooMany = Array.from({ length: 101 }, (_, i) => `keys=k${i}`).join("&")
  for (const [query, param] of [
    ["", "keys"],
    ["entitlement=e2e-lifetime", "entitlement"],
    ["keys=e2e-lifetime&limit=1", "limit"],
    [tooMany, "keys"],
  ]) {
    const refused = await request.get(`/billing/v1/catalog/products?${query}`)
    expect(refused.status(), query).toBe(400)
    expect((await refused.json()).error, query).toMatchObject({
      code: "invalid_query",
      param,
    })
  }
})
