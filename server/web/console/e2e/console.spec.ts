import {
  expect,
  test,
  type APIRequestContext,
  type Page,
} from "@playwright/test"

const password = "Console-test-password-42!"
async function staff(request: APIRequestContext, role = "owner") {
  const response = await request.post(`/__test/staff/${role}`)
  expect(response.status(), await response.text()).toBe(201)
  return (await response.json()) as { id: string; email: string }
}
async function login(page: Page, email: string) {
  await page.goto("/admin/login")
  await page.getByRole("textbox").first().fill(email)
  await page.locator("input[type=password]").fill(password)
  const signedIn = page.waitForResponse((response) =>
    response.url().endsWith("/auth/v1/password/login")
  )
  await page.getByRole("button", { name: "Sign in", exact: true }).click()
  const response = await signedIn
  expect(response.status()).toBe(200)
  const result = (await response.json()) as {
    token_set: { access_token: string }
  }
  await expect(
    page.getByRole("navigation", { name: "Console", exact: true })
  ).toBeVisible()
  return result.token_set.access_token
}

test("owner composes Billing, Access and Account with a live merchant selector", async ({
  page,
  request,
}) => {
  const operator = await staff(request)
  await login(page, operator.email)
  const navigation = page.getByRole("navigation", {
    name: "Console",
    exact: true,
  })
  await expect(
    navigation.getByRole("link", { name: "Billing", exact: true })
  ).toBeVisible()
  await expect(
    navigation.getByRole("link", { name: "Access", exact: true })
  ).toBeVisible()
  await expect(
    page
      .getByRole("combobox", { name: "Merchant", exact: true })
      .locator("option")
  ).toHaveCount(2)
  await page
    .getByRole("combobox", { name: "Merchant", exact: true })
    .selectOption({ label: "beta" })
  await navigation.getByRole("link", { name: "Access", exact: true }).click()
  await expect(
    page.getByRole("heading", { name: "Access", exact: true })
  ).toBeVisible()
  await page.getByRole("link", { name: "Roles", exact: true }).click()
  await expect(page).toHaveURL(/\/admin\/access\/roles/)
  await expect(page.getByRole("heading", { name: /Roles/ })).toBeVisible()
  await navigation.getByRole("link", { name: "Account", exact: true }).click()
  await expect(
    page.getByRole("heading", { name: "Account security", exact: true })
  ).toBeVisible()
  await expect(
    page.getByRole("list", { name: "Active sessions", exact: true })
  ).toBeVisible()
  await page.getByRole("button", { name: "Dark theme", exact: true }).click()
  await expect(page.locator(".console-shell")).toHaveAttribute(
    "data-theme",
    "dark"
  )
  await page.screenshot({
    path: test.info().outputPath("console-account-dark-desktop.png"),
    fullPage: true,
  })
  await page.setViewportSize({ width: 390, height: 844 })
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth
      )
    )
    .toBe(true)
  await page.screenshot({
    path: test.info().outputPath("console-account-dark-mobile.png"),
    fullPage: true,
  })
})

test("support sees its merchant and account, with no Access administration", async ({
  page,
  request,
}) => {
  const operator = await staff(request, "support")
  await login(page, operator.email)
  const navigation = page.getByRole("navigation", {
    name: "Console",
    exact: true,
  })
  await expect(
    navigation.getByRole("link", { name: "Access", exact: true })
  ).toHaveCount(0)
  await expect(
    page
      .getByRole("combobox", { name: "Merchant", exact: true })
      .locator("option")
  ).toHaveCount(1)
  await expect(
    page
      .getByRole("combobox", { name: "Merchant", exact: true })
      .locator("option")
  ).toHaveText("alpha")
  await navigation.getByRole("link", { name: "Account", exact: true }).click()
  await expect(
    page.getByRole("heading", { name: "Account security", exact: true })
  ).toBeVisible()
})

test("account security remains usable when billing permissions are unavailable", async ({
  page,
  request,
}) => {
  const operator = await staff(request, "support")
  await login(page, operator.email)
  let failedRequests = 0
  await page.route("**/v1/admin/access", (route) => {
    failedRequests += 1
    return route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ error: { message: "Billing is unavailable." } }),
    })
  })
  await page.goto("/admin/account")
  await expect.poll(() => failedRequests).toBeGreaterThan(0)
  await expect(
    page.getByRole("heading", { name: "Account security", exact: true })
  ).toBeVisible()
  await expect(
    page.getByRole("list", { name: "Active sessions", exact: true })
  ).toBeVisible()
})

test("a native root role change completes after the server requires fresh sign-in", async ({
  page,
  request,
}) => {
  const operator = await staff(request)
  const member = await staff(request)
  await page.route(
    "**/auth/v1/password/login",
    async (route) => {
      const response = await route.fetch()
      const body = await response.json()
      const stale = await request.post("/__test/stale-session", {
        data: { access_token: body.token_set.access_token },
      })
      expect(stale.status()).toBe(200)
      body.token_set.access_token = (await stale.json()).access_token
      await route.fulfill({ response, json: body })
    },
    { times: 1 }
  )
  await login(page, operator.email)
  await page
    .getByRole("navigation", { name: "Console", exact: true })
    .getByRole("link", { name: "Access", exact: true })
    .click()
  const memberRow = page.getByRole("row").filter({ hasText: member.id })
  const writes: number[] = []
  page.on("response", (response) => {
    if (
      response.request().method() === "PUT" &&
      response.url().endsWith(`/groups/root/members/users/${member.id}`)
    )
      writes.push(response.status())
  })
  await memberRow
    .getByRole("combobox")
    .selectOption("root:merchant-directory-viewer")
  const challenge = page
    .getByRole("dialog")
    .filter({ has: page.locator("input[type=password]") })
  await expect(challenge).toBeVisible()
  expect(writes[0]).toBe(401)
  await challenge.locator("input[type=password]").fill(password)
  await challenge.getByRole("button", { name: "Confirm", exact: true }).click()
  await expect(memberRow.getByRole("combobox")).toHaveValue(
    "root:merchant-directory-viewer"
  )
  await expect
    .poll(() => writes.filter((status) => status === 200))
    .toEqual([200])
})

test("an emailed passwordless link uses its native confirmation endpoint once", async ({
  page,
  request,
}) => {
  const operator = await staff(request, "support")
  const started = await request.post("/auth/v1/passwordless/start", {
    data: { identifier: operator.email, mode: "link" },
  })
  expect(started.status(), await started.text()).toBe(202)
  const letter = await request.get(
    `/__test/mail?kind=verification&to=${encodeURIComponent(operator.email)}`
  )
  expect(letter.status()).toBe(200)
  const { link } = (await letter.json()) as { link: string }
  expect(new URL(link).pathname).toBe("/admin/passwordless")
  let confirmations = 0
  page.on("request", (request) => {
    if (request.url().endsWith("/auth/v1/passwordless/confirm"))
      confirmations += 1
  })
  await page.goto(link)
  await expect(
    page.getByRole("navigation", { name: "Console", exact: true })
  ).toBeVisible()
  expect(confirmations).toBe(1)
  expect(new URL(page.url()).hash).toBe("")
})

test("a group invitation reaches enrollment through the shipped console", async ({
  page,
  request,
  browser,
}) => {
  const operator = await staff(request)
  await login(page, operator.email)
  const health = (await (await request.get("/__test/health")).json()) as {
    merchants: string[]
  }
  const email = `invited-${Date.now()}@console.test`
  await page
    .getByRole("combobox", { name: "Merchant", exact: true })
    .selectOption(health.merchants[0])
  await page
    .getByRole("navigation", { name: "Console", exact: true })
    .getByRole("link", { name: "Access", exact: true })
    .click()
  await page
    .getByRole("combobox", { name: "Manage access for", exact: true })
    .selectOption(health.merchants[0])
  await page.getByRole("link", { name: "Invitations", exact: true }).click()
  await page.getByRole("textbox", { name: /Email/i }).fill(email)
  await page
    .getByRole("combobox", { name: "Role", exact: true })
    .selectOption("merchant:viewer")
  const invited = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      response.url().endsWith(`/groups/${health.merchants[0]}/invitations`)
  )
  await page
    .getByRole("button", { name: /Send invitation|Invite/, exact: true })
    .click()
  const invitation = await invited
  expect(invitation.status(), await invitation.text()).toBe(202)
  const letter = await request.get(
    `/__test/mail?kind=invite&to=${encodeURIComponent(email)}`
  )
  expect(letter.status()).toBe(200)
  const { link } = (await letter.json()) as { link: string }
  const guest = await browser.newContext()
  try {
    const newcomer = await guest.newPage()
    await newcomer.goto(link)
    await expect(
      newcomer.getByRole("tab", { name: "Create account", exact: true })
    ).toHaveAttribute("aria-selected", "true")
    await newcomer
      .getByRole("textbox", { name: /Email|Identifier/ })
      .first()
      .fill(email)
    await newcomer
      .getByLabel("Username", { exact: true })
      .fill(`invited${Date.now()}`)
    await newcomer
      .locator("input[type=password]:visible")
      .first()
      .fill(password)
    if ((await newcomer.locator("input[type=password]:visible").count()) > 1)
      await newcomer
        .locator("input[type=password]:visible")
        .nth(1)
        .fill(password)
    await newcomer
      .getByRole("button", { name: /Create account|Register/, exact: true })
      .click()
    await expect
      .poll(async () =>
        (
          await request.get(
            `/__test/mail?kind=verification&to=${encodeURIComponent(email)}`
          )
        ).status()
      )
      .toBe(200)
    const verification = (await (
      await request.get(
        `/__test/mail?kind=verification&to=${encodeURIComponent(email)}`
      )
    ).json()) as { code: string }
    await newcomer
      .getByRole("textbox", { name: /code/i })
      .fill(verification.code)
    await newcomer
      .getByRole("button", { name: /Verify|Continue/, exact: false })
      .click()
    await expect(
      newcomer.getByRole("navigation", { name: "Console", exact: true })
    ).toBeVisible()
    await expect(
      newcomer
        .getByRole("combobox", { name: "Merchant", exact: true })
        .locator("option")
    ).toHaveCount(1)
    await expect(
      newcomer
        .getByRole("combobox", { name: "Merchant", exact: true })
        .locator("option")
    ).toHaveText("alpha")
  } finally {
    await guest.close().catch(() => {})
  }
})
