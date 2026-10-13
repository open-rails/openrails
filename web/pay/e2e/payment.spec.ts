import { createHash } from "node:crypto"
import { expect, test, type BrowserContext, type FrameLocator, type Page, type Route } from "@playwright/test"

const api = () => process.env.E2E_API!

interface Created {
  order_id: string
  customer_id: string
  url: string
}

async function newOrder(
  body: { saved_payment_methods?: boolean; cancel_url?: string; customer_id?: string; embedded?: boolean } = {}
): Promise<Created> {
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

async function payWithCollect(page: Page | FrameLocator) {
  await page.getByLabel("Name on card").fill("Card Holder")
  await page.getByLabel("Country").selectOption("US")
  await page.getByLabel(/ZIP/).fill("10001")
  await page.getByRole("button", { name: /^Pay \$12\.50/ }).click()
}

test("shows the merchant and the order, and keeps the secret out of the address bar", async ({ page }) => {
  const violations = await watchCSP(page)
  const order = await newOrder()
  await page.goto(order.url)
  await expect(page.getByRole("heading", { name: "Purchase Shop" })).toBeVisible()
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

test("the payment origin is registered at Stripe and serves Stripe's Apple Pay file", async ({ request }) => {
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
  await page.goto(`${base}#pus_${"A".repeat(43)}`)
  await expect(page.getByRole("status")).toHaveText("This payment link is not valid.")
  await expect(page.getByRole("button", { name: /^Pay/ })).toHaveCount(0)
})

test("the page refuses to be framed", async ({ page }) => {
  const order = await newOrder()
  await page.goto(`${api()}/__test/frame?src=${encodeURIComponent(order.url)}`)
  const frame = page.frameLocator("iframe")
  await page.waitForTimeout(500)
  await expect(frame.getByRole("heading", { name: "Purchase Shop" })).toHaveCount(0)
})

async function health(): Promise<{ api: string; payment: string; other: string }> {
  return (await (await fetch(`${api()}/__test/health`)).json()) as { api: string; payment: string; other: string }
}

type Message = { type?: string; order_id?: string; success_url?: string; height?: number }

test("an embedded payment page pays in the shop's frame and tells only the shop", async ({ page }) => {
  const order = await newOrder({ embedded: true })
  await page.goto(`${api()}/__test/embed?src=${encodeURIComponent(order.url)}`)
  const frame = page.frameLocator("iframe")
  await expect(frame.getByRole("heading", { name: "Purchase Shop" })).toBeVisible()
  await expect(frame.getByRole("link", { name: "← Back" })).toHaveCount(0)
  await payWithCollect(frame)
  await expect(frame.getByText("Payment complete.")).toBeVisible()
  const messages = await page.evaluate(() => (window as unknown as { __messages: Message[] }).__messages)
  expect(messages).toContainEqual({
    type: "openrails.payment.complete",
    order_id: order.order_id,
    success_url: `${api()}/__test/thanks?order=${order.order_id}`,
  })
  expect(messages.some((m) => m.type === "openrails.payment.resize" && (m.height ?? 0) > 0)).toBe(true)
  expect(new URL(page.url()).pathname).toBe("/__test/embed")
  await expect.poll(() => completed(order.order_id)).toBe(true)
})

test("an embedded payment page keeps back the fallback its frame names, as the shop's checkout does", async ({ page }) => {
  const order = await newOrder({ embedded: true })
  const fallbacks = encodeURIComponent(JSON.stringify({ stripe: { for: ["nmi"], afterFailures: 1 } }))
  await page.goto(`${api()}/__test/embed?src=${encodeURIComponent(`${order.url}&fallbacks=${fallbacks}`)}`)
  const frame = page.frameLocator("iframe")
  await expect(frame.getByRole("button", { name: /^Pay \$12\.50/ })).toBeVisible()
  await expect(frame.getByLabel("Pay with")).toHaveCount(0)

  // Without fallbacks the frame offers both card PSPs.
  const plain = await newOrder({ embedded: true })
  await page.goto(`${api()}/__test/embed?src=${encodeURIComponent(plain.url)}`)
  await expect(page.frameLocator("iframe").getByLabel("Pay with")).toBeVisible()
})

test("a site the shop did not register cannot frame its embedded payment page", async ({ page }) => {
  const order = await newOrder({ embedded: true })
  const { other } = await health()
  await page.goto(`${other}/__test/embed?src=${encodeURIComponent(order.url)}`)
  await page.waitForTimeout(500)
  await expect(page.frameLocator("iframe").getByRole("heading", { name: "Purchase Shop" })).toHaveCount(0)
})

// The network's issuer stands in at the payment origin's /__issuer, and the
// server's link routes answer as the network would (their server side is
// server/ci's): what is checked is the page's sign-in.
async function stubNetwork(context: BrowserContext, order: Created) {
  const payOrigin = new URL(order.url).origin
  const issuer = `${payOrigin}/__issuer`
  const seen = {
    authorize: [] as URL[],
    token: [] as { body: URLSearchParams; dpop: string | undefined }[],
    link: [] as { body: unknown; authorization: string | undefined }[],
  }
  await context.route(`${payOrigin}/v1/config`, async (route) => {
    const res = await route.fetch()
    const json = (await res.json()) as Record<string, unknown>
    await route.fulfill({ response: res, json: { ...json, network: { issuer, client_id: "openrails-payment-page" } } })
  })
  await context.route(`${payOrigin}/v1/me/network-link/challenges`, (route) =>
    route.fulfill({ status: 201, json: { id: "lnc_e2e", nonce: "nonce-e2e", expires_at: new Date(Date.now() + 300_000).toISOString() } })
  )
  await context.route(`${issuer}/.well-known/openid-configuration`, (route) =>
    route.fulfill({ json: { issuer, authorization_endpoint: `${issuer}/authorize`, token_endpoint: `${issuer}/token` } })
  )
  await context.route(`${issuer}/authorize?**`, (route) => {
    const url = new URL(route.request().url())
    seen.authorize.push(url)
    return route.fulfill({
      status: 302,
      headers: { location: `${payOrigin}/pay/sign-in?code=code-e2e&state=${encodeURIComponent(url.searchParams.get("state") ?? "")}` },
    })
  })
  await context.route(`${issuer}/token`, (route) => {
    seen.token.push({ body: new URLSearchParams(route.request().postData() ?? ""), dpop: route.request().headers()["dpop"] })
    return route.fulfill({ json: { access_token: "access-e2e", token_type: "DPoP", expires_in: 300, id_token: "id-token-e2e" } })
  })
  await context.route(`${payOrigin}/v1/me/network-link`, (route) => {
    seen.link.push({ body: route.request().postDataJSON(), authorization: route.request().headers()["authorization"] })
    return route.fulfill({
      status: 201,
      json: {
        id: "lnk_e2e",
        merchant_id: "mer_e2e",
        customer_id: order.customer_id,
        account_id: "acc_e2e",
        method: "payment_page",
        status: "active",
        linked_at: new Date().toISOString(),
        revoked_at: null,
        revoked_by: null,
      },
    })
  })
  return { payOrigin, seen }
}

test("Save with the network signs the shopper in and links the order's customer", async ({ page, context }) => {
  const order = await newOrder()
  const { payOrigin, seen } = await stubNetwork(context, order)
  await page.goto(order.url)
  await page.getByRole("button", { name: /^Save with / }).click()
  await expect(page.getByText(/^Saved to your .* account\.$/)).toBeVisible()
  expect(page.url()).toBe(order.url.split("#")[0])

  expect(seen.authorize).toHaveLength(1)
  const asked = seen.authorize[0].searchParams
  expect(Object.fromEntries(asked)).toMatchObject({
    response_type: "code",
    client_id: "openrails-payment-page",
    redirect_uri: `${payOrigin}/pay/sign-in`,
    scope: "openid",
    nonce: "nonce-e2e",
    prompt: "login",
    code_challenge_method: "S256",
  })
  expect(asked.get("state")).toMatch(new RegExp(`^${order.order_id}\\.`))
  expect(seen.token).toHaveLength(1)
  const { body, dpop } = seen.token[0]
  expect(body.get("grant_type")).toBe("authorization_code")
  expect(body.get("code")).toBe("code-e2e")
  expect(createHash("sha256").update(body.get("code_verifier") ?? "").digest("base64url")).toBe(asked.get("code_challenge"))
  expect(dpop).toBeTruthy()
  expect(seen.link).toEqual([{ body: { challenge_id: "lnc_e2e", id_token: "id-token-e2e" }, authorization: expect.stringMatching(/^Bearer pus_/) }])

  // A reload in the tab remembers it.
  await page.reload()
  await expect(page.getByText(/^Saved to your .* account\.$/)).toBeVisible()
  await context.unrouteAll({ behavior: "ignoreErrors" })
})

test("framed, the sign-in runs in a popup that hands its code back to the frame", async ({ page, context }) => {
  const order = await newOrder({ embedded: true })
  const { seen } = await stubNetwork(context, order)
  await page.goto(`${api()}/__test/embed?src=${encodeURIComponent(order.url)}`)
  const frame = page.frameLocator("iframe")
  const popup = page.waitForEvent("popup")
  await frame.getByRole("button", { name: /^Save with / }).click()
  await (await popup).waitForEvent("close")
  await expect(frame.getByText(/^Saved to your .* account\.$/)).toBeVisible()
  expect(seen.authorize[0].searchParams.get("state")).toMatch(/\.popup$/)
  expect(seen.link).toHaveLength(1)
  await context.unrouteAll({ behavior: "ignoreErrors" })
})

// Last: it moves the server's clock an hour on, past what the payments above
// rely on.
test("an expired pay URL says so", async ({ page }) => {
  const order = await newOrder()
  const res = await fetch(`${api()}/__test/clock`, { method: "POST", body: JSON.stringify({ seconds: 3601 }) })
  expect(res.status).toBe(200)
  await page.goto(order.url)
  await expect(page.getByRole("status")).toHaveText(/This payment link has expired/)
})
