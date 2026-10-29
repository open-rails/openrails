import { test, expect, type Page } from "@playwright/test"

// Each case drives the built console against the real router; the Go test
// that serves it asserts what landed. A case reads its fixture from
// /__fixture/config and asks the fixture for provider-side steps.

async function fixture<T>(page: Page): Promise<T> {
  return (await (await page.request.get("/__fixture/config")).json()) as T
}

async function act(page: Page, action: string) {
  const res = await page.request.post(`/__fixture/${action}`)
  expect(res.ok(), await res.text()).toBe(true)
}

// open loads a console page as one of the fixture's staff members.
async function open(page: Page, path: string, as = "staff") {
  await page.addInitScript(
    (who) => localStorage.setItem("critical-as", who),
    as
  )
  await page.goto(`/console${path}`)
}

async function confirmTyped(page: Page, word: string, action: string) {
  const dialog = page.getByRole("dialog")
  await dialog.getByRole("textbox").fill(word)
  await dialog.getByRole("button", { name: action, exact: true }).click()
  await expect(page.getByText("Apply queued").last()).toBeVisible()
}

const cell = (page: Page, kind: string, state: string) =>
  page.locator(`tr[data-kind="${kind}"] [data-state="${state}"]`)

test("console-pull-link-map-apply-lands-once", async ({ page }) => {
  const fx = await fixture<{ carol: string; vault: string }>(page)
  await open(page, "/ops/imports")
  await expect(page.getByText("No PSP has been pulled yet.")).toBeVisible()
  await page.getByRole("button", { name: "Pull", exact: true }).click()
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Pull", exact: true })
    .click()
  await expect(page.getByText("Pull started")).toBeVisible()
  await expect(page.getByRole("heading", { level: 1 })).toContainText(
    "Import of nmi"
  )
  const detail = page.url()
  await open(page, "/ops/imports")
  const row = page.locator("tr[data-import]")
  await expect(row).toHaveCount(1)
  await expect(row).toContainText("pulling")
  await expect(row).toContainText("Reading catalog and roster")
  await act(page, "start")
  // The list polls while the pull runs.
  await expect(row).toContainText("Pulled: ready to apply")
  await expect(row).toContainText("unmatched")
  await row.getByRole("link").click()
  await expect(page).toHaveURL(detail)
  await expect(cell(page, "subscription", "linked")).toHaveText("2")
  await expect(cell(page, "subscription", "unmatched")).toHaveText("1")
  await expect(page.locator('tr[data-ref="gold"]')).not.toContainText(
    "Unmapped"
  )
  await expect(page.locator('tr[data-ref="silver"]')).toContainText("Unmapped")

  await page.getByRole("link", { name: "Review records" }).click()
  const record = page.locator(`tr[data-ref="${fx.vault}"]`)
  await expect(record).toContainText("carol@legacy.invalid")
  await record.getByRole("button", { name: "Link to customer" }).click()
  const link = page.getByRole("dialog")
  await link.getByLabel("Customer id").fill(fx.carol)
  await link.getByRole("button", { name: "Link", exact: true }).click()
  await expect(page.getByText("Link saved: re-linking")).toBeVisible()
  await expect(page.getByText("No records in this state.")).toBeVisible()
  await page.getByRole("button", { name: "linked", exact: true }).click()
  await expect(page.locator(`tr[data-ref="${fx.vault}"]`)).toContainText(
    "via manual"
  )

  await open(page, "/ops/provider-prices")
  await expect(page.locator('tr[data-ref="gold"]')).toHaveCount(0)
  const silver = page.locator('tr[data-ref="silver"]')
  await expect(silver).toContainText("10.00 USD / 30 d")
  await silver.getByRole("button", { name: "Map to product" }).click()
  const map = page.getByRole("dialog")
  await expect(map.getByTestId("map-terms")).toHaveText("10.00 USD / 30 d")
  await map.getByLabel("Product").selectOption({ label: "Silver (silver)" })
  await expect(map.getByTestId("map-entitlements")).toContainText(
    "member:silver"
  )
  await expect(map.getByLabel("Price key")).toHaveValue("nmi-silver")
  await expect(
    map.getByLabel("Offer as a tier-change target")
  ).not.toBeChecked()
  await map.getByRole("button", { name: "Map", exact: true }).click()
  await expect(page.getByText("Price mapped")).toBeVisible()
  await expect(silver).toHaveCount(0)

  await page.goto(detail)
  await expect(page.locator('tr[data-ref="silver"]')).not.toContainText(
    "Unmapped"
  )
  await page.getByRole("button", { name: "Apply", exact: true }).click()
  await confirmTyped(page, "nmi", "Apply")
  await expect(cell(page, "subscription", "imported")).toHaveText("3")
  await expect(cell(page, "subscription", "unmatched")).toHaveText("0")
  await page.getByRole("button", { name: "Apply again" }).click()
  await confirmTyped(page, "nmi", "Apply again")
  await expect(cell(page, "subscription", "imported")).toHaveText("3")
})

test("console-mapping-upload-and-ccbill-price", async ({ page }) => {
  const fx = await fixture<{ csv: string; blocked: string; picked: string }>(
    page
  )
  await open(page, "/ops/imports")
  await page.getByRole("button", { name: "Upload mappings" }).click()
  const upload = page.getByRole("dialog")
  await upload.getByLabel("Mappings file").setInputFiles({
    name: "mappings.csv",
    mimeType: "text/csv",
    buffer: Buffer.from(fx.csv),
  })
  await expect(upload).toContainText("149 to set, 1 to delete, in 2 requests.")
  await upload.getByRole("button", { name: "Upload", exact: true }).click()
  await expect(upload).toContainText("Mappings set: 149, deleted: 1")
  await upload.getByRole("button", { name: "Re-link" }).click()
  await expect(page.getByText("Re-link requested")).toBeVisible()
  await page.keyboard.press("Escape")

  await page.locator("tr[data-import]").getByRole("link").click()
  const detail = page.url()
  await expect(cell(page, "subscription", "linked")).toHaveText("2")
  await expect(cell(page, "subscription", "unmatched")).toHaveText("1")
  await cell(page, "subscription", "linked").getByRole("link").click()
  const linked = page.locator("tbody tr")
  await expect(linked).toHaveCount(2)
  for (const row of await linked.all())
    await expect(row).toContainText("via rule:1")

  await page.goto(detail)
  await page.getByRole("button", { name: "Apply", exact: true }).click()
  await confirmTyped(page, "ccbill", "Apply")
  await expect(cell(page, "subscription", "blocked")).toHaveText("1")
  await expect(cell(page, "subscription", "imported")).toHaveText("1")
  await cell(page, "subscription", "blocked").getByRole("link").click()
  const blocked = page.locator(`tr[data-ref="${fx.blocked}"]`)
  await expect(blocked).toContainText("2 match")
  await blocked.getByRole("button", { name: "Set price" }).click()
  const pick = page.getByRole("dialog")
  await pick.getByLabel("Provider price").selectOption(fx.picked)
  await pick.getByRole("button", { name: "Set price", exact: true }).click()
  await expect(page.getByText("Price saved: re-linking")).toBeVisible()
  await expect(blocked).toHaveCount(0)

  // A re-link returns the import to pulled: it applies again as Apply.
  await page.goto(detail)
  await page.getByRole("button", { name: "Apply", exact: true }).click()
  await confirmTyped(page, "ccbill", "Apply")
  await expect(cell(page, "subscription", "imported")).toHaveText("2")
  await expect(cell(page, "subscription", "unmatched")).toHaveText("1")
})

test("console-parked-signup-lands-after-mapping", async ({ page }) => {
  const fx = await fixture<{ subscription: string; finding: string }>(page)
  await open(page, `/ops?finding=${fx.finding}`)
  const finding = page.locator("tr", { hasText: "catalog.price.unmapped" })
  await expect(
    finding.getByRole("link", { name: "Provider prices" })
  ).toBeVisible()
  await expect(finding.getByRole("button", { name: "Resolve" })).toHaveCount(0)

  await open(page, "/ops/parked")
  const park = page.locator(`tr[data-ref="${fx.subscription}"]`)
  await expect(park).toContainText("price_unmapped")
  await expect(park).toContainText("price_gold")
  await park.getByRole("button", { name: "Map to product" }).click()
  const map = page.getByRole("dialog")
  await expect(map.getByTestId("map-terms")).toHaveText(/^15\.00 usd \/ 1 mo$/i)
  await map.getByLabel("Product").selectOption({ label: "gold (gold)" })
  await map.getByRole("button", { name: "Map", exact: true }).click()
  await expect(page.getByText("Price mapped")).toBeVisible()
  // Mapping wakes the parked subscription's converge; run it now.
  await act(page, "settle")
  await page.getByLabel("Show released").check()
  await expect(park).toContainText("landed")
  await expect(park.getByRole("link")).toBeVisible()
})

test("console-failed-pull-and-read-only-access", async ({ page }) => {
  const fx = await fixture<{ nmiImport: string; stripeImport: string }>(page)
  await open(page, "/ops/imports", "reader")
  await expect(
    page.locator(`tr[data-import="${fx.stripeImport}"]`)
  ).toContainText("failed")
  await expect(page.getByRole("button", { name: "Pull" })).toHaveCount(0)
  await expect(
    page.getByRole("button", { name: "Upload mappings" })
  ).toHaveCount(0)

  await open(page, `/ops/imports/${fx.stripeImport}`, "reader")
  const failure = page.getByRole("alert")
  await expect(failure).toContainText("psp_credentials_rejected")
  await expect(
    failure.getByRole("link", { name: "Settings › PSPs" })
  ).toHaveAttribute("href", "/console/settings?tab=psps")
  await expect(page.locator("tr[data-kind] a")).toHaveCount(0)
  await expect(page.getByRole("button", { name: /Apply|Re-link/ })).toHaveCount(
    0
  )
  await open(page, `/ops/imports/${fx.stripeImport}/records`, "reader")
  await expect(page.getByText("No records in this state.")).toBeVisible()

  await open(page, `/ops/imports/${fx.nmiImport}`, "reader")
  await expect(page.locator('tr[data-ref="gold"]')).toContainText("Unmapped")
  for (const name of ["Map to product", "Apply", "Re-link"])
    await expect(page.getByRole("button", { name, exact: true })).toHaveCount(0)
  await open(
    page,
    `/ops/imports/${fx.nmiImport}/records?state=linked`,
    "reader"
  )
  await expect(page.locator("tbody tr").first()).toContainText("via ")
  await expect(
    page.getByRole("button", { name: "Link to customer" })
  ).toHaveCount(0)

  await open(page, "/ops/provider-prices", "reader")
  await page.getByLabel("PSP").selectOption({ label: "nmi (nmi)" })
  await expect(page.locator('tr[data-ref="gold"]')).toBeVisible()
  await expect(
    page.getByRole("button", { name: "Map to product" })
  ).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Dismiss" })).toHaveCount(0)
  await open(page, "/ops/parked", "reader")
  await expect(page.getByText("Nothing is parked.")).toBeVisible()

  // Configuration without catalog manage: Map is there, disabled, naming
  // the permission it needs.
  await open(page, "/ops/provider-prices", "config")
  await page.getByLabel("PSP").selectOption({ label: "nmi (nmi)" })
  await expect(page.getByRole("note")).toContainText("merchant:catalog:manage")
  const map = page
    .locator('tr[data-ref="gold"]')
    .getByRole("button", { name: "Map to product" })
  await expect(map).toBeDisabled()
  await expect(map).toHaveAttribute("title", "Needs merchant:catalog:manage")
  await open(page, "/ops/imports", "config")
  await expect(
    page.getByRole("button", { name: "Pull", exact: true })
  ).toBeVisible()
})

test("console-proposals-group-approve-held-dismiss", async ({
  page,
  context,
}) => {
  await open(page, "/ops/imports")
  await page.getByRole("button", { name: "Pull", exact: true }).click()
  const pull = page.getByRole("dialog")
  await pull.getByLabel("History since").selectOption({ label: "Full history" })
  await pull.getByRole("button", { name: "Pull", exact: true }).click()
  await expect(page.getByTestId("import-progress")).toContainText(
    "Pulled: ready to apply"
  )
  const counts = page.getByTestId("import-proposals")
  await expect(counts).toContainText(
    "5 open proposals (4 corrections Apply approves)"
  )
  const detail = page.url()

  // A pull's proposals are reviewed in the inbox, never resolved as findings.
  await open(page, "/ops")
  const pulls = page.locator("tr", { hasText: "pull." })
  await expect(pulls).toHaveCount(5)
  await expect(pulls.getByRole("link", { name: "Review" })).toHaveCount(5)
  await expect(pulls.getByRole("button", { name: "Resolve" })).toHaveCount(0)

  // The pull supersedes the first import: it says so and offers nothing.
  const fx = await fixture<{ first: string }>(page)
  await open(page, `/ops/imports/${fx.first}`)
  await expect(page.getByTestId("superseded")).toContainText(
    "A newer import of this PSP supersedes this one"
  )
  await expect(
    page.getByRole("button", { name: "Apply again", exact: true })
  ).toBeDisabled()
  await expect(
    page.getByRole("button", { name: "Re-link", exact: true })
  ).toBeDisabled()
  await page.getByTestId("superseded").getByRole("link").click()
  await expect(page).toHaveURL(detail)

  await page.getByRole("button", { name: "Apply", exact: true }).click()
  await confirmTyped(page, "nmi", "Apply")
  await expect(counts).toContainText("1 open proposals")
  await expect(counts).toContainText("· 2 approved")
  await counts.getByRole("link").click()
  const group = page.locator("section[data-group]")
  await expect(group).toHaveCount(1)
  await expect(group).toContainText(
    "nmi · 1 subscriptions ended at the provider"
  )
  await expect(group).toContainText("ends access")
  const inbox = page.url()
  // Another person's view, loaded before this one decides.
  const stale = await context.newPage()
  await stale.goto(inbox)
  await expect(stale.locator("tr[data-proposal]")).toHaveCount(1)

  const row = page.locator("tr[data-proposal]")
  await row.getByRole("checkbox").check()
  await page.getByRole("button", { name: "Approve 1" }).click()
  const approve = page.getByRole("dialog")
  const confirm = approve.getByRole("button", { name: "Approve", exact: true })
  await expect(confirm).toBeDisabled()
  await approve.getByLabel("Type 1 to confirm").fill("1")
  await confirm.click()
  await expect(row.getByTestId("outcome")).toContainText("held")
  await expect(row.getByTestId("outcome")).toContainText(
    "destructive_actions_held"
  )

  await page.getByRole("button", { name: "Reload" }).click()
  await row.getByRole("checkbox").check()
  await page.getByRole("button", { name: "Dismiss 1" }).click()
  const dismiss = page.getByRole("dialog")
  const confirmDismiss = dismiss.getByRole("button", {
    name: "Dismiss",
    exact: true,
  })
  await expect(confirmDismiss).toBeDisabled()
  await dismiss.getByLabel("Notes (required)").fill("kept: the member paid us")
  await confirmDismiss.click()
  await expect(row.getByTestId("outcome")).toContainText("dismissed")

  const late = stale.locator("tr[data-proposal]")
  await late.getByRole("checkbox").check()
  await stale.getByRole("button", { name: "Approve 1" }).click()
  await stale.getByRole("dialog").getByLabel("Type 1 to confirm").fill("1")
  await stale
    .getByRole("dialog")
    .getByRole("button", { name: "Approve", exact: true })
    .click()
  await expect(late.getByTestId("outcome")).toContainText("already_resolved")
  await stale.close()
})
