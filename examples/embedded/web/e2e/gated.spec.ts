// A visitor without access lands on the buy page, signs in with auth-ui, pays
// with billing-ui's CheckoutModal (Collect.js is a stand-in; nmimock is the
// gateway) and is sent back to the content, which the server now serves.
import { expect, test, type Page } from "@playwright/test"

const [alice, bobby, carol] = (process.env.E2E_USERS ?? "").split(",")
const password = process.env.E2E_PASSWORD ?? ""

const COLLECT_JS = `
window.CollectJS = {
  configure(config) {
    this.config = config
    for (const field of Object.values(config.fields)) {
      const host = document.querySelector(field.selector)
      if (host && !host.querySelector("input")) {
        const input = document.createElement("input")
        input.setAttribute("aria-label", field.title)
        host.appendChild(input)
      }
    }
    setTimeout(() => {
      config.fieldsAvailableCallback()
      for (const name of ["ccnumber", "ccexp", "cvv"]) config.validationCallback(name, true, "")
    }, 0)
  },
  startPaymentRequest() {
    setTimeout(() => this.config.callback({
      token: "e2e-" + crypto.randomUUID() + "-4242",
      card: { number: "4xxxxxxxxxxx4242", type: "visa", exp: "1235" },
    }), 0)
  },
}
`

test.beforeEach(async ({ page }) => {
  await page.route("https://secure.networkmerchants.com/token/Collect.js", (route) =>
    route.fulfill({ contentType: "application/javascript", body: COLLECT_JS })
  )
})

// Clicking a buy button signed out opens auth-ui's sign-in.
async function signIn(page: Page, buy: string, user: string) {
  await page.getByRole("button", { name: buy }).click()
  const form = page.getByRole("tabpanel", { name: "Sign in" })
  await form.getByRole("textbox", { name: "Email or phone number" }).fill(`${user}@example.com`)
  await form.getByRole("textbox", { name: "Password" }).fill(password)
  await form.getByRole("button", { name: "Sign in", exact: true }).click()
  await expect(page.getByRole("dialog")).toBeHidden()
}

// Pays in CheckoutModal with a new card.
async function pay(page: Page, buy: string, confirm: RegExp) {
  await page.getByRole("button", { name: buy }).click()
  const checkout = page.getByRole("dialog")
  await checkout.getByLabel("Name on card").fill("Card Holder")
  await checkout.getByLabel("Country").selectOption("US")
  await checkout.getByLabel(/ZIP/).fill("10001")
  await checkout.getByRole("button", { name: confirm }).click()
}

test("a visitor buys a course and watches it", async ({ page }) => {
  await page.goto("/courses/css-101")
  await expect(page).toHaveURL("/courses/css-101/buy")
  await expect(page.getByRole("heading", { name: "Intro to CSS" })).toBeVisible()

  await signIn(page, "Buy for $4.99", alice)
  await pay(page, "Buy for $4.99", /^Pay \$4\.99/)
  await expect(page).toHaveURL("/courses/css-101")
  await expect(page.getByRole("listitem").filter({ hasText: "The box model" })).toBeVisible()

  // Signed in, without the other course: its buy page.
  await page.goto("/courses/tailwind-102")
  await expect(page).toHaveURL("/courses/tailwind-102/buy")
})

test("the bundle unlocks both courses", async ({ page }) => {
  await page.goto("/courses/tailwind-102")
  await expect(page).toHaveURL("/courses/tailwind-102/buy")
  await signIn(page, "Both courses for $8.99", bobby)
  await pay(page, "Both courses for $8.99", /^Pay \$8\.99/)
  await expect(page).toHaveURL("/courses/tailwind-102")
  await expect(page.getByRole("listitem").filter({ hasText: "Utility classes" })).toBeVisible()

  await page.goto("/courses/css-101")
  await expect(page.getByRole("listitem").filter({ hasText: "Flexbox" })).toBeVisible()
  await expect(page).toHaveURL("/courses/css-101")
})

test("a member reads the members-only Q&A", async ({ page }) => {
  await page.goto("/members/qa")
  await expect(page).toHaveURL("/join")
  await signIn(page, "$10 every 30 days", carol)
  await pay(page, "$10 every 30 days", /^Subscribe for/)
  await expect(page).toHaveURL("/members/qa")
  await expect(page.getByRole("listitem").filter({ hasText: "How do I center a div?" })).toBeVisible()
})

test("an unknown course is not found", async ({ page }) => {
  await page.goto("/courses/no-such-course/buy")
  await expect(page.getByText("Not found")).toBeVisible()
})
