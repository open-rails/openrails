import { test, expect, type Page } from "@playwright/test"

// Each pay's Idempotency-Key, and each read back of one.
function watch(page: Page) {
  const seen = { pays: [] as string[], attempts: [] as string[], confirms: 0, reads: 0 }
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname
    const key = request.headers()["idempotency-key"] ?? ""
    if (request.method() === "POST" && path.endsWith("/pay")) seen.pays.push(key)
    if (request.method() === "POST" && path.endsWith("/confirm")) seen.confirms++
    if (request.method() === "GET" && path.endsWith("/payment-attempt")) seen.attempts.push(key)
    if (request.method() === "GET" && /\/v1\/me\/invoices\/[^/]+$/.test(path)) seen.reads++
  })
  return seen
}

test("OR-ORDERPAY-01: a lost pay answer is read back under its key, and a retry keeps the key", async ({
  page,
}) => {
  const seen = watch(page)
  let pays = 0
  await page.route("**/v1/me/invoices/*/pay", async (route) => {
    pays++
    // The first never reaches OpenRails.
    if (pays === 1) return route.abort("failed")
    // The second reaches it, and its answer is lost.
    if (pays === 2) {
      await route.fetch()
      return route.abort("failed")
    }
    return route.continue()
  })
  await page.goto("/")
  await expect(page.locator("body")).toHaveAttribute("data-ready", "true")
  const pay = page.getByRole("button", { name: "Pay with USDC on Solana" })
  await pay.click()
  await expect(page.getByRole("alert")).toBeVisible()
  await expect.poll(() => seen.attempts.length).toBe(1)
  expect(seen.attempts).toEqual([seen.pays[0]])
  await pay.click()
  await expect(page.getByText("Waiting for your wallet's payment")).toBeVisible()
  expect(seen.pays).toHaveLength(2)
  expect(seen.pays[1]).toBe(seen.pays[0])
  expect(seen.attempts).toEqual([seen.pays[0], seen.pays[0]])
})

test("OR-ORDERPAY-02: an invoice awaiting its wallet is read, and confirmed twice a minute at most", async ({
  page,
}) => {
  await page.clock.install()
  const seen = watch(page)
  await page.goto("/")
  await expect(page.locator("body")).toHaveAttribute("data-ready", "true")
  await page.getByRole("button", { name: "Pay with USDC on Solana" }).click()
  await expect(page.getByText("Waiting for your wallet's payment")).toBeVisible()
  const confirms = seen.confirms
  const reads = seen.reads
  await page.clock.runFor(60_000)
  await expect.poll(() => seen.reads - reads).toBeGreaterThanOrEqual(15)
  expect(seen.confirms - confirms).toBeLessThanOrEqual(2)
  await expect(page.getByText("Waiting for your wallet's payment")).toBeVisible()
})
