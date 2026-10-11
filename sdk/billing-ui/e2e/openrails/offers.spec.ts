// <Offers merchant keys> against the real server: the public catalog looks
// the merchant's seeded products up by key in one read with no credential, a
// BuyButton per price, and a signed-out visitor is sent to sign in. The
// catalog takes its merchant and keys alone.
import { expect, test, type APIRequestContext } from "@playwright/test"

// The merchant the server sells for.
async function merchantId(request: APIRequestContext) {
  const seeded = (await (await request.get("/__test/health")).json()) as {
    merchant_id: string
  }
  return seeded.merchant_id
}

test("Offers looks a merchant's products up by key", async ({
  page,
  request,
}) => {
  const merchant = await merchantId(request)
  const reads: { search: string; authorization: string | null }[] = []
  page.on("request", (r) => {
    const url = new URL(r.url())
    if (url.pathname.endsWith("/products"))
      reads.push({
        search: url.search,
        authorization: r.headers()["authorization"] ?? null,
      })
  })
  await page.goto(
    `/offers.html#merchant=${merchant}&keys=e2e-membership,e2e-lifetime`
  )

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
  expect(reads).toEqual([
    {
      search: `?merchant=${merchant}&keys=e2e-membership&keys=e2e-lifetime`,
      authorization: null,
    },
  ])
})

test("the public catalog takes its merchant and product keys alone", async ({
  request,
}) => {
  const merchant = await merchantId(request)
  const named = `merchant=${merchant}`
  const found = await request.get(
    `/billing/v1/products?${named}&keys=e2e-lifetime&keys=no-such-product`
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
    [named, "keys"],
    [`${named}&entitlement=e2e-lifetime`, "entitlement"],
    [`${named}&keys=e2e-lifetime&limit=1`, "limit"],
    [`${named}&${tooMany}`, "keys"],
    ["keys=e2e-lifetime", "merchant"],
    ["merchant=billing-ui-e2e&keys=e2e-lifetime", "merchant"],
  ]) {
    const refused = await request.get(`/billing/v1/products?${query}`)
    expect(refused.status(), query).toBe(400)
    expect((await refused.json()).error, query).toMatchObject({
      code: "invalid_query",
      param,
    })
  }

  const unknown = await request.get(
    "/billing/v1/products?merchant=0190a000-0000-7000-8000-00000000000a&keys=e2e-lifetime"
  )
  expect(unknown.status()).toBe(404)
  expect((await unknown.json()).error).toMatchObject({
    code: "resource_not_found",
  })
})
