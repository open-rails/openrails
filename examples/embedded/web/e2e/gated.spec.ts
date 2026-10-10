// A visitor without access lands on the buy page, signs in with auth-ui, pays
// with billing-ui's CheckoutModal (Collect.js is a stand-in; nmimock is the
// gateway) and is sent back to the content, which the server now serves.
import { expect, test, type Locator, type Page } from "@playwright/test"

const [alice, bobby, carol, dana] = (process.env.E2E_USERS ?? "").split(",")
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
async function signIn(page: Page, buy: Locator, user: string) {
  await buy.click()
  const form = page.getByRole("tabpanel", { name: "Sign in" })
  await form.getByRole("textbox", { name: "Email or phone number" }).fill(`${user}@example.com`)
  await form.getByRole("textbox", { name: "Password" }).fill(password)
  await form.getByRole("button", { name: "Sign in", exact: true }).click()
  await expect(page.getByRole("dialog")).toBeHidden()
}

// Pays in CheckoutModal with a new card.
async function pay(page: Page, buy: Locator, confirm: RegExp) {
  await buy.click()
  const checkout = page.getByRole("dialog")
  await checkout.getByLabel("Name on card").fill("Card Holder")
  await checkout.getByLabel("Country").selectOption("US")
  await checkout.getByLabel(/ZIP/).fill("10001")
  await checkout.getByRole("button", { name: confirm }).click()
}

const button = (page: Page, name: string) => page.getByRole("button", { name, exact: true })

// The course page plays its signed video URL.
async function expectVideo(page: Page) {
  const video = page.locator("video")
  await expect(video).toHaveAttribute("src", /^\/media\/courses\/.+\.mp4\?expires=\d+&sig=[0-9a-f]+$/)
  await expect.poll(() => video.evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(1)
  expect(await video.evaluate((v: HTMLVideoElement) => v.error)).toBeNull()
}

test("the store lists courses with their prices and what the user owns", async ({ page }) => {
  await page.goto("/")
  const css = page.locator("section").filter({ has: page.getByRole("heading", { name: "Intro to CSS" }) })
  await expect(page.getByRole("heading", { name: "Intro to Tailwind" })).toBeVisible()
  await expect(css.getByRole("button", { name: "$4.99 to keep", exact: true })).toBeVisible()
  const rent = css.getByRole("button", { name: "$1.99 for 3 days", exact: true })

  await signIn(page, rent, dana)
  await pay(page, rent, /^Pay \$1\.99/)
  await expect(page).toHaveURL("/courses/css-101")
  await expectVideo(page)

  await page.goto("/")
  await expect(css.getByRole("link", { name: "Watch" })).toBeVisible()
  await expect(css.getByRole("button")).toHaveCount(0)
  const tailwind = page.locator("section").filter({ has: page.getByRole("heading", { name: "Intro to Tailwind" }) })
  await expect(tailwind.getByRole("button", { name: "$4.99 to keep", exact: true })).toBeVisible()
  // A members-only video is sold with the membership.
  const qa = page.locator("section").filter({ has: page.getByRole("heading", { name: "Live Q&A" }) })
  await expect(qa.getByText("Members only")).toBeVisible()
  await expect(qa.getByRole("button", { name: "$10.00 every 30 days", exact: true })).toBeVisible()
})

test("a course page sends a visitor without access to buy it", async ({ page }) => {
  await page.goto("/courses/css-101")
  await expect(page).toHaveURL("/courses/css-101/buy")
  // Everything on sale that unlocks it, named and priced by the catalog: the
  // course, the bundle and the membership.
  await expect(page.getByRole("heading", { name: "Course 101 — Intro to CSS" })).toBeVisible()
  await expect(page.getByRole("heading", { name: "CSS courses 101 and 102" })).toBeVisible()
  await expect(page.getByRole("heading", { name: "Channel membership" })).toBeVisible()
  await expect(button(page, "Rent for 3 days, $1.99")).toBeVisible()

  await signIn(page, button(page, "$4.99"), alice)
  await pay(page, button(page, "$4.99"), /^Pay \$4\.99/)
  await expect(page).toHaveURL("/courses/css-101")
  await expectVideo(page)

  // Signed in, without the other course: its buy page.
  await page.goto("/courses/tailwind-102")
  await expect(page).toHaveURL("/courses/tailwind-102/buy")
})

test("the bundle unlocks both courses", async ({ page }) => {
  await page.goto("/courses/tailwind-102")
  await expect(page).toHaveURL("/courses/tailwind-102/buy")
  await signIn(page, button(page, "$8.99"), bobby)
  await pay(page, button(page, "$8.99"), /^Pay \$8\.99/)
  await expect(page).toHaveURL("/courses/tailwind-102")
  await expectVideo(page)

  await page.goto("/courses/css-101")
  await expectVideo(page)
  await expect(page).toHaveURL("/courses/css-101")
})

test("the membership unlocks members-only videos and every course", async ({ page }) => {
  await page.goto("/courses/live-qa")
  await expect(page).toHaveURL("/courses/live-qa/buy")
  await expect(page.getByRole("heading", { name: "Channel membership" })).toBeVisible()
  await expect(page.getByTestId("offer")).toHaveCount(1)
  await expect(button(page, "$99.00 every 365 days")).toBeVisible()
  await signIn(page, button(page, "$10.00 every 30 days"), carol)
  await pay(page, button(page, "$10.00 every 30 days"), /^Subscribe for/)
  await expect(page).toHaveURL("/courses/live-qa")
  await expectVideo(page)

  await page.goto("/courses/css-101")
  await expectVideo(page)
  await expect(page).toHaveURL("/courses/css-101")
})

test("an unknown course says it couldn't load", async ({ page }) => {
  await page.goto("/courses/no-such-course")
  await expect(page.getByText("Couldn't load this course.")).toBeVisible()
})
