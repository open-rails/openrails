import { expect, test, type Page, type Route } from "@playwright/test"

const api = () => process.env.E2E_API!

interface Created {
  order_id: string
  customer_id: string
  url: string
}

async function newOrder(body: { saved_payment_methods?: boolean; cancel_url?: string; customer_id?: string } = {}): Promise<Created> {
  const res = await fetch(`${api()}/__test/orders`, { method: "POST", body: JSON.stringify(body) })
  expect(res.status, await res.clone().text()).toBe(201)
  return (await res.json()) as Created
}

async function completed(orderId: string): Promise<boolean> {
  const res = await fetch(`${api()}/__test/host-events?type=order.completed`)
  const page = (await res.json()) as { data: { order?: { order_id: string } }[] }
  return page.data.some((event) => event.order?.order_id === orderId)
}

// Collect.js stands in for NMI's fields; its tokens name a visa ending 4242
// on the loopback gateway.
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

// Stripe.js stands in for Stripe's Payment Element: the customer types a
// card's last four; its calls to Stripe go to the fake Stripe through the
// harness (3155 needs the bank's challenge). Its Express Checkout Element
// offers Apple Pay, whose card ends 4242.
const STRIPE_JS = `
window.Stripe = function () {
  let typed = ""
  return {
    _registerWrapper() {},
    elements(options) {
      ;(window.__stripeElements = window.__stripeElements || []).push(options)
      return {
        create(type) {
          const handlers = {}
          let input
          if (type === "expressCheckout") {
            let button
            return {
              on(event, fn) { handlers[event] = fn },
              mount(host) {
                button = document.createElement("button")
                button.type = "button"
                button.textContent = "Apple Pay"
                button.addEventListener("click", () => {
                  typed = "4242"
                  handlers.confirm({ expressPaymentType: "apple_pay", paymentFailed(why) { window.__walletFailed = why } })
                })
                host.appendChild(button)
                setTimeout(() => handlers.ready({ elementType: "expressCheckout", availablePaymentMethods: { applePay: true, googlePay: false, link: false, paypal: false, klarna: false, amazonPay: false } }), 0)
              },
              destroy() { button && button.remove() },
            }
          }
          return {
            on(event, fn) { handlers[event] = fn },
            mount(host) {
              input = document.createElement("input")
              input.setAttribute("aria-label", "Card number")
              input.addEventListener("input", () => {
                typed = input.value
                handlers.change && handlers.change({ complete: typed.length >= 4 })
              })
              host.appendChild(input)
            },
            destroy() { input && input.remove() },
          }
        },
        submit: async () => ({}),
      }
    },
    async createPaymentMethod() {
      const res = await fetch("https://api.stripe.com/v1/payment_methods", { method: "POST", body: JSON.stringify({ last4: typed.slice(-4) }) })
      return { paymentMethod: { id: (await res.json()).id } }
    },
    async handleNextAction({ clientSecret }) {
      await fetch("https://api.stripe.com/v1/e2e/authenticate", { method: "POST", body: JSON.stringify({ client_secret: clientSecret }) })
      return { paymentIntent: { status: "succeeded" } }
    },
  }
}
window.Stripe.version = "dahlia"
`

// Turnstile's widget, passing at once.
const TURNSTILE_JS = `
window.turnstile = {
  render(el, options) {
    const button = document.createElement("button")
    button.textContent = "I am human"
    button.onclick = () => options.callback("e2e-captcha-token")
    el.appendChild(button)
    return "widget"
  },
}
`

const stripeCalls: string[] = []

test.beforeEach(async ({ page }) => {
  await page.route("https://secure.networkmerchants.com/token/Collect.js", (route) =>
    route.fulfill({ contentType: "application/javascript", body: COLLECT_JS })
  )
  await page.route("https://js.stripe.com/**", (route) => route.fulfill({ contentType: "application/javascript", body: STRIPE_JS }))
  await page.route("https://challenges.cloudflare.com/**", (route) =>
    route.fulfill({ contentType: "application/javascript", body: TURNSTILE_JS })
  )
  await page.route("https://api.stripe.com/**", async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    stripeCalls.push(path)
    const harness = path.endsWith("/authenticate") ? "/__test/stripe/authenticate" : "/__test/stripe/payment-methods"
    const res = await fetch(api() + harness, { method: "POST", body: route.request().postData() ?? "{}" })
    await route.fulfill({ status: res.status, contentType: "application/json", body: await res.text(), headers: { "Access-Control-Allow-Origin": "*" } })
  })
})

// Every Content-Security-Policy violation the page reports.
async function watchCSP(page: Page): Promise<() => Promise<string[]>> {
  await page.addInitScript(() => {
    const seen: string[] = []
    ;(window as unknown as { __csp: string[] }).__csp = seen
    document.addEventListener("securitypolicyviolation", (e) => seen.push(`${e.violatedDirective} ${e.blockedURI}`))
  })
  return () => page.evaluate(() => (window as unknown as { __csp: string[] }).__csp)
}

async function payWithCollect(page: Page) {
  await page.getByLabel("Name on card").fill("Card Holder")
  await page.getByLabel("Country").selectOption("US")
  await page.getByLabel(/ZIP/).fill("10001")
  await page.getByRole("button", { name: /^Pay \$12\.50/ }).click()
}

test("shows the merchant and the order, and keeps the secret out of the address bar", async ({ page }) => {
  const violations = await watchCSP(page)
  const order = await newOrder()
  await page.goto(order.url)
  await expect(page.getByRole("heading", { name: "Checkout Shop" })).toBeVisible()
  await expect(page.getByText(/^Course /)).toBeVisible()
  await expect(page.getByText("$12.50").first()).toBeVisible()
  expect(new URL(page.url()).hash).toBe("")
  expect(await violations()).toEqual([])

  // A reload in the same tab still opens the order.
  await page.reload()
  await expect(page.getByText(/^Course /)).toBeVisible()
})

test("pays with a new card and returns to success_url; the backend learns from order.completed", async ({ page }) => {
  const violations = await watchCSP(page)
  const order = await newOrder()
  await page.goto(order.url)
  await payWithCollect(page)
  await expect(page).toHaveURL(`${api()}/__test/thanks?order=${order.order_id}`)
  await expect(page.getByRole("heading", { name: `Thanks for order ${order.order_id}` })).toBeVisible()
  await expect.poll(() => completed(order.order_id)).toBe(true)
  expect(await violations()).toEqual([])
})

test("a card the bank challenges pays after 3-D Secure", async ({ page }) => {
  const order = await newOrder()
  await page.goto(order.url)
  await page.getByLabel("Pay with").selectOption({ label: "Stripe" })
  await page.getByLabel("Card number").fill("4000002500003155")
  stripeCalls.length = 0
  await page.getByRole("button", { name: /^Pay \$12\.50/ }).click()
  await expect(page).toHaveURL(`${api()}/__test/thanks?order=${order.order_id}`)
  expect(stripeCalls).toEqual(["/v1/payment_methods", "/v1/e2e/authenticate"])
  await expect.poll(() => completed(order.order_id)).toBe(true)
})

test("saved cards appear only where the merchant offers them", async ({ page }) => {
  const first = await newOrder()
  await page.goto(first.url)
  await payWithCollect(page)
  await expect(page).toHaveURL(/__test\/thanks/)

  const offered = await newOrder({ customer_id: first.customer_id, saved_payment_methods: true })
  await page.goto(offered.url)
  const saved = page.getByRole("radiogroup", { name: "Saved cards" })
  await expect(saved.getByText("Visa card ending 4242")).toBeAttached()
  await saved.getByText(/•••• 4242/).click()
  await page.getByRole("button", { name: /^Pay \$12\.50/ }).click()
  await expect(page).toHaveURL(`${api()}/__test/thanks?order=${offered.order_id}`)

  const hidden = await newOrder({ customer_id: first.customer_id })
  await page.goto(hidden.url)
  await expect(page.getByRole("button", { name: /^Pay \$12\.50/ })).toBeVisible()
  await expect(page.getByRole("radiogroup", { name: "Saved cards" })).toHaveCount(0)
})

test("pays with Apple Pay through Stripe's Express Checkout Element", async ({ page }) => {
  const violations = await watchCSP(page)
  const order = await newOrder()
  await page.goto(order.url)
  const wallet = page.getByRole("button", { name: "Apple Pay" })
  await expect(wallet).toBeVisible()
  await expect(page.getByText("or pay another way")).toBeVisible()
  // The total Stripe.js is given: $12.50 in cents.
  const options = await page.evaluate(() => (window as unknown as { __stripeElements: Record<string, unknown>[] }).__stripeElements)
  expect(options.filter((o) => o.mode === "payment")).toEqual([
    expect.objectContaining({ mode: "payment", amount: 1250, currency: "usd", paymentMethodCreation: "manual", paymentMethodTypes: ["card"] }),
  ])
  expect(await violations()).toEqual([])
  stripeCalls.length = 0
  await wallet.click()
  await expect(page).toHaveURL(`${api()}/__test/thanks?order=${order.order_id}`)
  expect(stripeCalls).toEqual(["/v1/payment_methods"])
  await expect.poll(() => completed(order.order_id)).toBe(true)
})

test("the checkout host is registered at Stripe and serves Stripe's Apple Pay file", async ({ request }) => {
  const order = await newOrder()
  const domains = async () => {
    const res = await fetch(`${api()}/__test/stripe/domains`)
    expect(res.status, await res.clone().text()).toBe(200)
    return ((await res.json()) as { domains: string[] }).domains
  }
  await expect.poll(domains).toEqual([new URL(order.url).hostname])
  const file = await request.get(new URL("/.well-known/apple-developer-merchantid-domain-association", order.url).href)
  expect(file.status()).toBe(200)
  // Hex of {"pspId":", Stripe's merchant id with Apple.
  expect((await file.text()).startsWith("7B2270737049")).toBe(true)
})

test("a challenged pay is resent with the captcha token", async ({ page }) => {
  const order = await newOrder()
  await page.goto(order.url)
  let challenged = false
  await page.route(`**/v1/me/orders/${order.order_id}/pay`, async (route) => {
    if (challenged) return route.continue()
    challenged = true
    await route.fulfill({
      status: 403,
      contentType: "application/json",
      body: JSON.stringify({ error: { type: "invalid_request_error", code: "captcha_required", message: "solve the captcha" } }),
    })
  })
  const resent = page.waitForRequest((r) => r.url().endsWith(`/v1/me/orders/${order.order_id}/pay`) && challenged && !!r.headers()["x-captcha-token"])
  await payWithCollect(page)
  await page.getByRole("button", { name: "I am human" }).click()
  expect((await resent).headers()["x-captcha-token"]).toBe("e2e-captcha-token")
  await expect(page).toHaveURL(`${api()}/__test/thanks?order=${order.order_id}`)
})

test("the back link returns to cancel_url", async ({ page }) => {
  const order = await newOrder({ cancel_url: `${api()}/__test/thanks?canceled=1` })
  await page.goto(order.url)
  await page.getByRole("link", { name: "← Back" }).click()
  await expect(page).toHaveURL(/canceled=1/)
})

test("a wrong secret opens nothing", async ({ page }) => {
  const order = await newOrder()
  const [base] = order.url.split("#")
  await page.goto(`${base}#cks_${"A".repeat(43)}`)
  await expect(page.getByRole("status")).toHaveText("This checkout link is not valid.")
  await expect(page.getByRole("button", { name: /^Pay/ })).toHaveCount(0)
})

test("an expired checkout says so", async ({ page }) => {
  const order = await newOrder()
  const res = await fetch(`${api()}/__test/clock`, { method: "POST", body: JSON.stringify({ seconds: 3601 }) })
  expect(res.status).toBe(200)
  await page.goto(order.url)
  await expect(page.getByRole("status")).toHaveText(/This checkout has expired/)
})

test("the page refuses to be framed", async ({ page }) => {
  const order = await newOrder()
  await page.goto(`${api()}/__test/frame?src=${encodeURIComponent(order.url)}`)
  const frame = page.frameLocator("iframe")
  await page.waitForTimeout(500)
  await expect(frame.getByRole("heading", { name: "Checkout Shop" })).toHaveCount(0)
})
