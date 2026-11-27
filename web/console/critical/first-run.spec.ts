import { createHmac } from "node:crypto"
import { expect, test } from "@playwright/test"

// A fresh server's first run, from the setup link it logged
// (OPENRAILS_SETUP_LINK); the Go test asserts what landed.

const password = "Authtest-password-1"

// An authenticator app: RFC 6238 over the setup key enrollment issued.
function totp(secret: string, at = Date.now()) {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
  const bytes: number[] = []
  let word = 0
  let bits = 0
  for (const symbol of secret.replace(/=+$/, "").toUpperCase()) {
    word = (word << 5) | alphabet.indexOf(symbol)
    bits += 5
    if (bits >= 8) {
      bits -= 8
      bytes.push((word >>> bits) & 255)
      word &= (1 << bits) - 1
    }
  }
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(Math.floor(at / 30_000)))
  const digest = createHmac("sha1", Buffer.from(bytes)).update(counter).digest()
  return String(
    (digest.readUInt32BE(digest[19] & 15) & 0x7fffffff) % 1_000_000
  ).padStart(6, "0")
}

test("first-run-owner-then-merchant-then-payment-provider", async ({
  page,
}, info) => {
  const link = process.env.OPENRAILS_SETUP_LINK
  expect(link, "the Go test passes the setup link").toBeTruthy()

  // 1. The setup link creates the owner, who enrolls a second factor.
  await page.goto(link!)
  await expect(page.getByText("Set up the owner account")).toBeVisible()
  await page.getByLabel("Email").fill("owner@critical.invalid")
  await page.getByLabel("Username").fill("owner")
  await page.getByLabel("Password", { exact: true }).fill(password)
  await page.getByLabel("Confirm password").fill(password)
  await page.getByRole("button", { name: "Create owner account" }).click()
  const enrollment = page.waitForResponse(
    (r) =>
      r.url().endsWith("/auth/v1/me/2fa/setup") &&
      r.request().method() === "POST"
  )
  await page.getByRole("button", { name: "Set up authenticator" }).click()
  const { secret } = (await (await enrollment).json()) as { secret: string }
  const configuration = page.waitForResponse((r) =>
    r.url().endsWith("/v1/config")
  )
  await page.getByLabel("Verification code").fill(totp(secret))
  const merchantStep = page.getByRole("heading", {
    name: "Name your first merchant",
  })
  const backupCodes = page.getByRole("button", {
    name: /saved my backup codes/,
  })
  await expect(merchantStep.or(backupCodes)).toBeVisible()
  if (await backupCodes.isVisible()) await backupCodes.click()

  // 2. The same flow names the first merchant. A file holds this server's
  // merchant configuration, so it asks for no display name.
  await expect(merchantStep).toBeVisible()
  await expect(page).toHaveURL(/\/admin\/setup\/merchant$/)
  await configuration
  await expect(page.getByLabel(/Display name/)).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Skip for now" })).toBeVisible()
  await page.getByLabel("Name", { exact: true }).fill("first-shop")
  await page.screenshot({ path: info.outputPath("first-merchant.png") })
  const created = page.waitForResponse(
    (r) =>
      r.url().endsWith("/admin/merchants") && r.request().method() === "POST"
  )
  await page.getByRole("button", { name: "Create merchant" }).click()
  const answer = await created
  expect(answer.status()).toBe(201)
  expect(await answer.json()).toMatchObject({
    slug: "first-shop",
    role: "owner",
  })

  // 3. Its overview asks for a payment provider: declared in the file.
  await expect(page).toHaveURL(/\/admin\/billing/)
  const setup = page.locator('[data-setup="psp"]')
  await expect(setup).toContainText(
    "Connect a payment provider to take payments"
  )
  await expect(setup).toContainText("configuration is read from a file")
  await expect(setup.getByRole("button")).toHaveCount(0)
  await expect(page.locator(".console-header select")).toContainText(
    "first-shop"
  )
  await page.screenshot({
    path: info.outputPath("payment-provider.png"),
    fullPage: true,
  })
})
