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
    page.getByLabel("Merchant", { exact: true }).locator("option")
  ).toHaveCount(2)
  await page
    .getByLabel("Merchant", { exact: true })
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
    page.getByRole("heading", { name: "Sessions", exact: true })
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
    page.getByLabel("Merchant", { exact: true }).locator("option")
  ).toHaveCount(1)
  await expect(
    page.getByLabel("Merchant", { exact: true }).locator("option")
  ).toHaveText("alpha")
  await navigation.getByRole("link", { name: "Account", exact: true }).click()
  await expect(
    page.getByRole("heading", { name: "Account security", exact: true })
  ).toBeVisible()
})

test("a native AuthKit credential write asks for fresh sign-in and retries once", async ({
  page,
  request,
}) => {
  const operator = await staff(request)
  const stale = await request.post(`/__test/stale-token/${operator.id}`)
  expect(stale.status()).toBe(200)
  const { access_token: token } = (await stale.json()) as {
    access_token: string
  }
  await page.route(
    "**/auth/v1/password/login",
    async (route) => {
      const response = await route.fetch()
      const body = await response.json()
      body.token_set.access_token = token
      await route.fulfill({ response, json: body })
    },
    { times: 1 }
  )
  await login(page, operator.email)
  await page
    .getByRole("navigation", { name: "Console", exact: true })
    .getByRole("link", { name: "Access", exact: true })
    .click()
  await page.getByRole("link", { name: "API keys", exact: true }).click()
  await page.getByRole("button", { name: /Create API key|New API key/ }).click()
  await page.getByLabel("Name", { exact: true }).fill("Browser proof")
  await page.getByRole("combobox", { name: "Role", exact: true }).click()
  await page
    .getByRole("option", { name: /merchant.?directory.?viewer/i })
    .click()
  await page
    .getByRole("button", { name: /Create key|Create API key/, exact: false })
    .last()
    .click()
  const challenge = page.getByRole("dialog")
  await expect(challenge).toBeVisible()
  await challenge.locator("input[type=password]").fill(password)
  await challenge
    .getByRole("button", { name: /Continue|Confirm|Verify/ })
    .click()
  await expect(page.getByText("Browser proof", { exact: true })).toBeVisible()
  await expect(page.getByRole("button", { name: /Copy.*key/i })).toBeVisible()
})

test("a group invitation reaches enrollment through the shipped console", async ({
  page,
  request,
  browser,
}) => {
  const operator = await staff(request)
  const token = await login(page, operator.email)
  const health = (await (await request.get("/__test/health")).json()) as {
    merchants: string[]
  }
  const email = `invited-${Date.now()}@console.test`
  const invitation = await request.post(
    `/auth/v1/groups/${health.merchants[0]}/invitations`,
    {
      headers: { Authorization: `Bearer ${token}` },
      data: { email, role: "merchant:viewer" },
    }
  )
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
    await newcomer
      .getByLabel(/Email|Identifier/)
      .first()
      .fill(email)
    await newcomer
      .getByLabel("Username", { exact: true })
      .fill(`invited${Date.now()}`)
    await newcomer.locator("input[type=password]").first().fill(password)
    if ((await newcomer.locator("input[type=password]").count()) > 1)
      await newcomer.locator("input[type=password]").nth(1).fill(password)
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
      newcomer.getByLabel("Merchant", { exact: true }).locator("option")
    ).toHaveCount(1)
    await expect(
      newcomer.getByLabel("Merchant", { exact: true }).locator("option")
    ).toHaveText("alpha")
  } finally {
    await guest.close()
  }
})
