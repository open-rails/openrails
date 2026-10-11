import { expect, test, type APIRequestContext } from "@playwright/test"
import { createUser, seedBilling } from "../openrails/api"

async function staff(request: APIRequestContext, role = "admin") {
  const response = await request.post(`/__test/staff/${role}`)
  expect(response.status(), await response.text()).toBe(201)
  return (await response.json()) as {
    access_token: string
    id: string
  }
}

test("host router deep links, customer search, detail slots, and history", async ({
  page,
  request,
}) => {
  const operator = await staff(request),
    customer = await createUser(request)
  await seedBilling(request, customer.id)
  await page.goto(`/admin/billing/customers#token=${operator.access_token}`)
  await expect(
    page.getByRole("heading", { name: "Customers", exact: true })
  ).toBeVisible()
  const search = page.getByPlaceholder("Search email, username, name or id…")
  await search.fill(customer.id)
  await search.press("Enter")
  await expect(page).toHaveURL(new RegExp(`search=${customer.id}`))
  await page
    .getByRole("link")
    .filter({ hasText: customer.id.slice(0, 13) })
    .click()
  await expect(page.getByTestId("host-customer")).toHaveText(
    `Host user ${customer.id}`
  )
  await expect(
    page.getByRole("link", { name: "View user account" })
  ).toHaveAttribute("href", `/host/users/${customer.id}`)
  await page.goBack()
  await expect(search).toHaveValue(customer.id)
  await page.goForward()
  await expect(page.getByTestId("host-customer")).toBeVisible()
  await page.getByRole("link", { name: "Catalog", exact: true }).click()
  await expect(
    page.getByRole("heading", { name: "Products", exact: true })
  ).toBeVisible()
})

test("read-only staff cannot see catalog controls or grant customer access", async ({
  page,
  request,
}) => {
  const operator = await staff(request, "reader"),
    customer = await createUser(request)
  await seedBilling(request, customer.id)
  await page.goto(
    `/admin/billing/customers/${customer.id}#token=${operator.access_token}`
  )
  await expect(page.getByTestId("host-customer")).toBeVisible()
  await expect(
    page.getByRole("link", { name: "Catalog", exact: true })
  ).toHaveCount(0)
  await expect(
    page.getByRole("button", { name: "Product access", exact: true })
  ).toHaveCount(0)
})

for (const theme of ["light", "dark", "auto"] as const) {
  test(`history fallback and scoped ${theme} styling`, async ({
    page,
    request,
  }) => {
    const operator = await staff(request)
    await page.emulateMedia({ colorScheme: "dark" })
    await page.goto(
      `/admin/history/catalog#token=${operator.access_token}&theme=${theme}`
    )
    await expect(
      page.getByRole("heading", { name: "Products", exact: true })
    ).toBeVisible()
    await expect(page.locator(".orba").first()).toHaveAttribute("data-orba-theme", theme === "auto" ? "dark" : theme)
    if (theme === "auto") {
      await page.emulateMedia({ colorScheme: "light" })
      await expect(page.locator(".orba").first()).toHaveAttribute("data-orba-theme", "light")
      await page.emulateMedia({ colorScheme: "dark" })
      await expect(page.locator(".orba").first()).toHaveAttribute("data-orba-theme", "dark")
    }
    await expect(page.getByTestId("host-header")).toHaveCSS(
      "background-color",
      "rgb(240, 230, 210)"
    )
    await page.getByRole("link", { name: "Subscriptions", exact: true }).click()
    await expect(page).toHaveURL(/\/admin\/history\/subscriptions/)
    await page.goBack()
    await expect(
      page.getByRole("heading", { name: "Products", exact: true })
    ).toBeVisible()
    await page.screenshot({ path: test.info().outputPath(`admin-${theme}-desktop.png`), fullPage: true })
    await page.setViewportSize({ width: 390, height: 844 })
    await expect(
      page.getByRole("button", { name: "Toggle Sidebar", exact: true }).first()
    ).toBeVisible()
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
    await page.screenshot({ path: test.info().outputPath(`admin-${theme}-mobile.png`), fullPage: true })
  })
}

test("grants product access through the scoped dialog and real billing API", async ({
  page,
  request,
}) => {
  const operator = await staff(request),
    customer = await createUser(request)
  await seedBilling(request, customer.id)
  const catalog = (await (await request.get("/__test/health")).json()) as {
    one_time_product_id: string
  }
  await page.goto(
    `/admin/billing/customers/${customer.id}#token=${operator.access_token}`
  )
  await page
    .getByRole("button", { name: "Product access", exact: true })
    .click()
  const dialog = page.getByRole("dialog", { name: "Grant product access" })
  await dialog.getByRole("combobox", { name: "Product", exact: true }).click()
  await page.getByRole("option", { name: /Lifetime pass/ }).click()
  await dialog
    .getByRole("button", { name: "Grant access", exact: true })
    .click()
  await expect(dialog).toBeHidden()
  const response = await request.get(
    `/billing/v1/admin/product-access?customer_id=${customer.id}`,
    { headers: { Authorization: `Bearer ${operator.access_token}` } }
  )
  expect(response.status(), await response.text()).toBe(200)
  const grants = (await response.json()) as { data: { product_id: string }[] }
  expect(
    grants.data.some(
      (grant) => grant.product_id === catalog.one_time_product_id
    )
  ).toBe(true)
})

test("refunds an actual loopback charge and cancels its subscription", async ({
  page,
  request,
}) => {
  const operator = await staff(request),
    customer = await createUser(request)
  const authorization = { Authorization: `Bearer ${operator.access_token}` }
  const catalog = (await (await request.get("/__test/health")).json()) as {
    card_monthly_price_id: string
  }
  const providers = (await (
    await request.get("/billing/v1/merchant/psps", { headers: authorization })
  ).json()) as { data: { id: string; key: string }[] }
  const psp = providers.data.find((provider) => provider.key === "cards")!
  const bought = await request.post("/billing/v1/me/orders", {
    headers: {
      Authorization: `Bearer ${customer.access_token}`,
      "Idempotency-Key": `admin-proof-${customer.id}`,
    },
    data: {
      lines: [{ price_id: catalog.card_monthly_price_id }],
      expected_total: "4990000",
      payment: {
        token: `e2e-${customer.id}-4242`,
        psp_id: psp.id,
        billing_details: { name: "Admin proof", address: { country: "US" } },
      },
    },
  })
  expect(bought.status(), await bought.text()).toBe(201)
  const order = (await bought.json()) as { payment: { payment_id: string } }
  const payment = order.payment.payment_id
  await page.goto(
    `/admin/billing/payments/${payment}#token=${operator.access_token}`
  )
  await page.getByRole("button", { name: "Refund", exact: true }).click()
  const refund = page.getByRole("dialog", { name: "Refund payment" })
  await refund.getByLabel("Amount (USD)").fill("1.00")
  await refund.getByLabel("Reason (optional)").fill("Component proof")
  await refund.getByRole("button", { name: "Refund", exact: true }).click()
  await expect(refund).toBeHidden()
  const refunded = (await (
    await request.get(`/billing/v1/admin/payments/${payment}`, {
      headers: authorization,
    })
  ).json()) as { amount_refunded: string }
  expect(refunded.amount_refunded).toBe("1000000")
  const held = (await (
    await request.get(`/__test/customers/${customer.id}/billing`)
  ).json()) as { subscriptions: { id: string }[] }
  const subscription = held.subscriptions[0].id
  await page.goto(
    `/admin/billing/subscriptions/${subscription}#token=${operator.access_token}`
  )
  await page
    .getByRole("button", { name: "Cancel subscription", exact: true })
    .click()
  const cancel = page.getByRole("dialog", {
    name: "Cancel subscription",
    exact: true,
  })
  await cancel.getByLabel("Type CANCEL to confirm").fill("CANCEL")
  await cancel
    .getByRole("button", { name: "Cancel subscription", exact: true })
    .click()
  await expect(cancel).toBeHidden()
  const canceled = (await (
    await request.get(`/billing/v1/admin/subscriptions/${subscription}`, {
      headers: authorization,
    })
  ).json()) as { status: string }
  expect(canceled.status).toBe("canceled")
})

test("loads dashboard widgets and keeps a custom range in the host URL", async ({
  page,
  request,
}) => {
  const operator = await staff(request)
  await page.goto(`/admin/billing/#token=${operator.access_token}`)
  await expect(
    page.getByRole("heading", { name: "Dashboard", exact: true })
  ).toBeVisible()
  await page.getByRole("combobox", { name: "Date range", exact: true }).click()
  await page.getByRole("option", { name: /Custom/ }).click()
  await page.getByLabel("Start", { exact: true }).fill("2026-10-01")
  await page.getByLabel("End", { exact: true }).fill("2026-10-11")
  await expect(page).toHaveURL(/from=2026-10-01/)
  await expect(page).toHaveURL(/to=2026-10-11/)
  await page.reload()
  await expect(page.getByLabel("Start", { exact: true })).toHaveValue(
    "2026-10-01"
  )
})
