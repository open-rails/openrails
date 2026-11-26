import { test, expect, type Frame, type Page } from "@playwright/test"
import type {} from "./app"

async function post(page: Page, frame: Frame, invoice: string) {
  const before = await page.evaluate(() => window.messageSeen)
  await frame.evaluate(
    (id) =>
      window.parent.postMessage(
        {
          type: "openrails.payment.complete",
          invoice_id: id,
          success_url: "https://critical.invalid/done",
        },
        "*"
      ),
    invoice
  )
  await page.waitForFunction((before) => window.messageSeen > before, before)
}
test("OR-HOSTED-01: only the expected origin frame and invoice can complete", async ({
  page,
}) => {
  await page.route("**/*", async (route) => {
    const host = new URL(route.request().url()).hostname
    if (host === "127.0.0.1" || host === "localhost") await route.continue()
    else await route.abort()
  })
  // The real payment page reads the invoice its path names.
  const read = page.waitForResponse((response) =>
    /^\/v1\/me\/invoices\/[^/]+$/.test(new URL(response.url()).pathname)
  )
  await page.goto("/")
  await expect(page.locator("body")).toHaveAttribute("data-ready", "true")
  const fixture = await page.evaluate(() => window.hostedFixture)
  const locator = page.getByTitle("Payment", { exact: true })
  const element = await locator.elementHandle()
  expect(element).not.toBeNull()
  let frame = await element!.contentFrame()
  expect(frame).not.toBeNull()
  await expect.poll(() => frame!.url()).toContain("/pay/" + fixture.invoices[0])
  const response = await read
  expect(new URL(response.url()).pathname).toBe("/v1/me/invoices/" + fixture.invoices[0])
  expect(response.status()).toBe(200)
  const src = new URL((await locator.getAttribute("src")) ?? "")
  expect(src.search).toBe("")
  const params = new URLSearchParams(src.hash.split("&").slice(1).join("&"))
  expect(JSON.parse(params.get("fallbacks") ?? "null")).toEqual({
    ccbill: { for: ["stripe"], afterFailures: 2 },
  })
  await page.evaluate(
    ({ attacker, sibling }) => {
      for (const [id, url] of [
        ["attacker", attacker],
        ["sibling", sibling],
      ]) {
        const frame = document.createElement("iframe")
        frame.id = id
        frame.src = url
        document.body.append(frame)
      }
    },
    { attacker: fixture.attacker, sibling: fixture.urls[1] }
  )
  for (const id of ["attacker", "sibling"]) {
    const element = await page.locator("#" + id).elementHandle()
    const other = await element!.contentFrame()
    expect(other).not.toBeNull()
    await expect.poll(() => other!.url()).not.toBe("about:blank")
    await post(page, other!, fixture.invoices[0])
    expect(await page.evaluate(() => window.completions)).toEqual([])
  }
  await post(page, frame!, fixture.invoices[1])
  expect(await page.evaluate(() => window.completions)).toEqual([])
  await post(page, frame!, fixture.invoices[0])
  await post(page, frame!, fixture.invoices[0])
  expect(await page.evaluate(() => window.completions)).toEqual([
    { invoiceId: fixture.invoices[0], successUrl: "https://critical.invalid/done" },
  ])
  await page.evaluate(() => window.showInvoice(1))
  await expect(locator).toHaveAttribute(
    "src",
    new RegExp("/pay/" + fixture.invoices[1])
  )
  frame = await (await locator.elementHandle())!.contentFrame()
  await expect.poll(() => frame!.url()).toContain("/pay/" + fixture.invoices[1])
  await post(page, frame!, fixture.invoices[0])
  expect(await page.evaluate(() => window.completions.length)).toBe(1)
  await post(page, frame!, fixture.invoices[1])
  expect(
    await page.evaluate(() => window.completions.map((x) => x.invoiceId))
  ).toEqual(fixture.invoices)
  await page.clock.install()
  await page.route("**/*blocked=1*", (route) => route.abort())
  const unavailable = new URL(fixture.urls[0])
  unavailable.search = "?blocked=1"
  const failed = page.waitForEvent("requestfailed", (request) =>
    request.url().includes("blocked=1")
  )
  await page.evaluate((url) => window.showURL(url), unavailable.toString())
  await failed
  await page.clock.fastForward(12_001)
  await expect(page.getByRole("alert")).toBeVisible()
  await expect(page.getByRole("alert")).not.toContainText(
    "haven't been charged"
  )
  await page.evaluate(() => window.showInvoice(1))
  await expect(page.getByRole("alert")).toHaveCount(0)
  await expect(locator).toHaveAttribute(
    "src",
    new RegExp("/pay/" + fixture.invoices[1])
  )
  frame = await (await locator.elementHandle())!.contentFrame()
  await expect.poll(() => frame!.url()).toContain("/pay/" + fixture.invoices[1])
  await post(page, frame!, fixture.invoices[1])
  expect(
    await page.evaluate(() => window.completions.map((x) => x.invoiceId))
  ).toEqual([...fixture.invoices, fixture.invoices[1]])
})
