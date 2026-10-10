import { describe, expect, it, vi } from "vitest"

import errorFixture from "../../../../testdata/wire/error_envelope.json"
import subscriptionFixture from "../../../../testdata/wire/subscription.json"
import { fakeBilling, json, price, product, subscription } from "../test/billing-server"
import {
  createBillingClient,
  RETRY_BUDGET_MS,
  signWalletAction,
  WalletRejectedError,
} from "./client"
import { fixtureSession } from "../fixtures"
import { checkoutOf } from "./checkout"
import { BillingError, isServerError } from "./errors"
import { priceSchema, productSchema, subscriptionSchema } from "./types"

describe("wire fixtures", () => {
  it("preserves catalog revisions and accepts prices from older servers", () => {
    expect(priceSchema.parse(price(720, 720, { revision: 2 })).revision).toBe(2)
    expect(priceSchema.parse(price(720, 720)).revision).toBeUndefined()
    expect(productSchema.parse(product({ revision: 3 })).revision).toBe(3)
    expect(productSchema.parse(product()).revision).toBeUndefined()
  })

  it("decodes the canonical OpenRails subscription", () => {
    const parsed = subscriptionSchema.parse(subscriptionFixture)
    expect(parsed).toMatchObject({
      id: "sub_cccccccc-cccc-4ccc-8ccc-cccccccccccc",
      status: "past_due",
      cancel_mode: "reversible",
      price: { unit_amount: "9223372036854775807", currency: "USD" },
      product: { display_name: "Pro" },
      card: { brand: "visa", last4: "4242" },
      dunning: {
        attempts: 2,
        retries_left: 3,
        next_retry_at: "2026-09-16T00:00:00.123456789Z",
        waiting_for_new_card: false,
        last_failure_reason: "insufficient_funds",
      },
    })
  })

  it("decodes the error envelope", async () => {
    const client = createBillingClient({
      fetch: async () => json(422, errorFixture),
    })
    const err = await client.listInvoices().catch((e: unknown) => e)
    expect(err).toBeInstanceOf(BillingError)
    expect(err).toMatchObject({
      status: 422,
      code: "idempotency_key_reused",
      type: "invalid_request_error",
      param: "amount",
      requestId: "req_fixture",
      message: "retry changed the committed amount",
    })
  })
})

describe("createBillingClient", () => {
  it("reads the customer's summary", async () => {
    const account = {
      id: "7d5b4a0e-8c3f-4c1e-9b2a-1f0e2d3c4b5a",
      balances: [
        {
          currency: "USD",
          billing_mode: "arrears",
          balance_amount: "0",
          held_amount: "0",
          available_amount: "0",
          owed_amount: "9223372036854775807",
        },
      ],
      default_payment_methods: [{ currency: "USD", payment_method_id: "pm_1" }],
      unread_notifications: 2,
    }
    const fetch = vi.fn(async () => json(200, account))
    const client = createBillingClient({ baseUrl: "https://shop.test/billing/v1/", fetch })
    expect(await client.getAccount()).toEqual(account)
    const [url] = fetch.mock.calls[0] as unknown as [string]
    expect(url).toBe("https://shop.test/billing/v1/me")
  })

  it("targets the mount, encodes ids and attaches the bearer", async () => {
    const fetch = vi.fn(async () => json(200, subscriptionFixture))
    const client = createBillingClient({
      baseUrl: "https://shop.test/billing/v1/",
      fetch,
      getToken: async () => "tok",
      language: () => "de",
    })
    await client.cancelSubscription("sub_a/b", { reason: "  too pricey " })
    const [url, init] = fetch.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe(
      "https://shop.test/billing/v1/me/subscriptions/sub_a%2Fb/cancel"
    )
    expect(init.method).toBe("POST")
    expect(JSON.parse(String(init.body))).toEqual({ reason: "too pricey" })
    const headers = new Headers(init.headers)
    expect(headers.get("Authorization")).toBe("Bearer tok")
    expect(headers.get("Accept-Language")).toBe("de")
  })

  it("lists every status by default and pages payments by cursor", async () => {
    const server = fakeBilling()
    const client = createBillingClient({ fetch: server.fetch })
    const subs = await client.listSubscriptions()
    expect(subs.data).toHaveLength(1)
    expect(server.fetch.mock.calls[0][0]).toBe(
      "/billing/v1/me/subscriptions?status=all&limit=100"
    )
    await client.listPayments({ limit: 5, cursor: "c1", rail: "nmi" })
    expect(server.fetch.mock.calls[1][0]).toBe(
      "/billing/v1/me/payments?limit=5&cursor=c1&rail=nmi"
    )
  })

  it("rejects a drifting payload as invalid_response", async () => {
    const client = createBillingClient({
      fetch: async () => json(200, { data: [{ id: "sub_1" }] }),
    })
    await expect(client.listSubscriptions()).rejects.toMatchObject({
      code: "invalid_response",
    })
  })

  it("reports a converging card removal as pending", async () => {
    const client = createBillingClient({
      fetch: async () => new Response(null, { status: 202 }),
    })
    await expect(client.removePaymentMethod("pm_1")).resolves.toBe("pending")
  })

  it("carries the pinned currency registry, extendable by the host", () => {
    const client = createBillingClient({ currencies: { btc: 8 } })
    expect(client.currencies).toMatchObject({ USD: 6, JPY: 4, BTC: 8 })
  })

  it("completes a Solana cancel with the wallet's signature", async () => {
    const server = fakeBilling({
      subscriptions: [subscription({ id: "sub_sol", rail: "solana" })],
    })
    const client = createBillingClient({ fetch: server.fetch })
    const asked = await client.cancelSubscription("sub_sol", { reason: "done" })
    expect(asked.status).toBe("active")
    expect(asked.next_action?.type).toBe("solana_sign_transactions")
    const send = vi.fn(async (tx: string) => `sig-for-${tx}`)
    const signature = await signWalletAction(asked.next_action!, send)
    expect(send).toHaveBeenCalledWith("dHg=")
    const done = await client.cancelSubscription("sub_sol", {
      reason: "done",
      signature,
    })
    expect(done.status).toBe("canceled")
    expect(JSON.parse(String(server.fetch.mock.calls[1][1]?.body))).toEqual({
      reason: "done",
      signature: "sig-for-dHg=",
    })

    await expect(
      signWalletAction(asked.next_action!, async () => {
        throw new WalletRejectedError()
      })
    ).rejects.toMatchObject({ code: "wallet_rejected" })
    await expect(
      signWalletAction(asked.next_action!, async () => "")
    ).rejects.toMatchObject({ code: "wallet_no_signature" })
  })
})

// Server errors surface at once; only a GET retries, once, briefly.
describe("server errors", () => {
  const ok = () => new Response("{}", { status: 200 })
  const status = (code: number, headers?: Record<string, string>) =>
    new Response(null, { status: code, headers })
  const calls = (...responses: (() => Response)[]) => {
    const fetch = vi.fn(async () => {
      const next = responses.shift()
      if (!next) throw new Error("unexpected request")
      return next()
    })
    return fetch
  }

  it("surfaces a 500 without retrying", async () => {
    const fetch = calls(() => status(500))
    const err = await createBillingClient({ fetch })
      .listInvoices()
      .catch((e: unknown) => e)
    expect(err).toMatchObject({ status: 500, code: "server_error" })
    expect(isServerError(err)).toBe(true)
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  it("retries a GET once on 502/503/504 and network errors", async () => {
    for (const first of [
      () => status(502),
      () => status(503),
      () => status(504),
      () => {
        throw new TypeError("network down")
      },
    ]) {
      const fetch = calls(first, ok)
      await createBillingClient({ fetch }).listPaymentMethods()
      expect(fetch).toHaveBeenCalledTimes(2)
    }
    const fetch = calls(
      () => status(503),
      () => status(503)
    )
    await expect(
      createBillingClient({ fetch }).listPaymentMethods()
    ).rejects.toMatchObject({ status: 503 })
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it("never retries a write", async () => {
    const fetch = calls(() => status(503))
    await expect(
      createBillingClient({ fetch }).cancelSubscription("sub_1", {
        reason: "too pricey",
      })
    ).rejects.toMatchObject({ status: 503 })
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  it("honours Retry-After only within the budget", async () => {
    const slow = calls(() => status(429, { "Retry-After": "5" }))
    await expect(
      createBillingClient({ fetch: slow }).listInvoices()
    ).rejects.toMatchObject({ status: 429 })
    expect(slow).toHaveBeenCalledTimes(1)
    const quick = calls(() => status(503, { "Retry-After": "1" }), ok)
    const started = Date.now()
    await createBillingClient({ fetch: quick }).listPaymentMethods()
    expect(quick).toHaveBeenCalledTimes(2)
    expect(Date.now() - started).toBeLessThan(RETRY_BUDGET_MS)
  })
})

describe("checkout sessions", () => {
  it.each([
    {},
    { priceKey: "monthly" },
    { productKey: "premium" },
    { priceId: "price_1", productKey: "premium" },
    { priceId: "price_1", priceKey: "monthly" },
    { priceId: "price_1", productKey: "premium", priceKey: "monthly" },
  ])("rejects incomplete or ambiguous price selectors before making a request: %j", async (input) => {
    const fetch = vi.fn()
    const client = createBillingClient({ fetch })
    await expect(checkoutOf(client).createCheckoutSession(input)).rejects.toMatchObject({
      code: "invalid_request",
    })
    expect(fetch).not.toHaveBeenCalled()
  })

  it("accepts a price id without a product key", async () => {
    const fetch = vi.fn(async () => Response.json({
      id: `ocs_${"a".repeat(64)}`,
      url: "https://pay.example/checkout#ocs_x",
      expires_at: "2026-10-03T00:00:00Z",
    }, { status: 201 }))
    await checkoutOf(createBillingClient({ fetch })).createCheckoutSession({ priceId: "price_1" })
    const [, init] = fetch.mock.calls[0] as unknown as [string, RequestInit]
    expect(JSON.parse(String(init.body))).toEqual({ price_id: "price_1" })
  })

  it.each([undefined, true, false])("preserves the order renewal preference %s", async (autoRenew) => {
    const fetch = vi.fn().mockResolvedValue(Response.json({
      id: `ocs_${"a".repeat(64)}`, url: null, expires_at: "2026-10-03T00:00:00Z",
    }, { status: 201 }))
    await checkoutOf(createBillingClient({ fetch })).createCheckoutSession({ priceId: "price_1", autoRenew })
    const [, init] = fetch.mock.calls[0] as unknown as [string, RequestInit]
    const body = JSON.parse(String(init.body))
    expect(body).toEqual(autoRenew === undefined ? { price_id: "price_1" } : { price_id: "price_1", auto_renew: autoRenew })
  })

  it("binds a customer-selected deposit to the minted checkout", async () => {
    const fetch = vi.fn().mockResolvedValue(
      Response.json(
        {
          id: `ocs_${"a".repeat(64)}`,
          url: null,
          expires_at: "2026-10-03T00:00:00Z",
        },
        { status: 201 }
      )
    )
    await checkoutOf(createBillingClient({ fetch })).createCheckoutSession({
      productKey: "api-credits",
      priceKey: "deposit",
      amount: "100000000",
    })
    const [, init] = fetch.mock.calls[0] as unknown as [string, RequestInit]
    expect(JSON.parse(String(init.body))).toEqual({
      product_key: "api-credits",
      price_key: "deposit",
      amount: "100000000",
    })
  })

  it("mints with the customer's bearer and pays with the id alone", async () => {
    const calls: { url: string; init: RequestInit }[] = []
    const replies = [
      Response.json(
        {
          id: `ocs_${"a".repeat(64)}`,
          url: "https://pay.example/checkout#ocs_x",
          expires_at: "2026-10-03T00:00:00Z",
        },
        { status: 201 }
      ),
      Response.json({ status: "succeeded", subscription_id: "sub_1" }),
    ]
    const client = createBillingClient({
      getToken: () => "user-token",
      fetch: async (url, init) => {
        calls.push({ url, init })
        return replies.shift()!
      },
    })
    const link = await checkoutOf(client).createCheckoutSession({
      productKey: "premium",
      priceKey: "monthly",
      successUrl: "https://host-one.example/welcome",
    })
    expect(link.url).toBe("https://pay.example/checkout#ocs_x")
    const paid = await checkoutOf(client)
      .checkoutSource(link.id)
      .pay({ option_id: "option_1" })
    expect(paid.status).toBe("succeeded")

    expect(calls[0].url).toBe("/billing/v1/me/checkout-sessions")
    expect(new Headers(calls[0].init.headers).get("Authorization")).toBe(
      "Bearer user-token"
    )
    expect(JSON.parse(String(calls[0].init.body))).toEqual({
      product_key: "premium",
      price_key: "monthly",
      success_url: "https://host-one.example/welcome",
    })
    expect(calls[1].url).toBe(`/billing/v1/checkout-sessions/${link.id}/pay`)
    expect(new Headers(calls[1].init.headers).get("Authorization")).toBeNull()
  })

  it("reads and pays as the customer on a customer surface", async () => {
    const calls: { url: string; init: RequestInit }[] = []
    const replies = [
      Response.json(fixtureSession({ id: "ocs_1" })),
      Response.json({ status: "succeeded", subscription_id: "sub_1" }),
    ]
    const client = createBillingClient({
      getToken: () => "owner-token",
      fetch: async (url, init) => {
        calls.push({ url, init })
        return replies.shift()!
      },
    })
    const source = checkoutOf(client).checkoutSource("ocs_1", {
      customerBase: "/api/v1/merchants/acme/billing/me/",
    })
    expect((await source.getSession()).id).toBe("ocs_1")
    await expect(
      source.pay({ option_id: "option_1", payment_method_id: "pm_1" })
    ).resolves.toMatchObject({ status: "succeeded" })
    expect(calls.map((c) => c.url)).toEqual([
      "/api/v1/merchants/acme/billing/me/checkout-sessions/ocs_1",
      "/api/v1/merchants/acme/billing/me/checkout-sessions/ocs_1/pay",
    ])
    for (const call of calls)
      expect(new Headers(call.init.headers).get("Authorization")).toBe(
        "Bearer owner-token"
      )
    expect(calls[1].init.method).toBe("POST")
  })

  it("answers refusals the buyer can act on as results", async () => {
    const refusal = (status: number, code: string) =>
      Response.json(
        {
          error: {
            type: "invalid_request_error",
            code,
            message: `refused ${code}`,
          },
        },
        { status }
      )
    for (const [reply, want] of [
      [refusal(410, "checkout_session_expired"), { status: "expired" }],
      [refusal(404, "checkout_session_not_found"), { status: "expired" }],
      [
        refusal(403, "checkout_session_unavailable"),
        {
          status: "blocked",
          failure_message: "refused checkout_session_unavailable",
        },
      ],
      [
        refusal(422, "checkout_request_invalid"),
        {
          status: "failed",
          failure_message: "refused checkout_request_invalid",
        },
      ],
    ] as const) {
      const client = createBillingClient({ fetch: async () => reply })
      await expect(
        checkoutOf(client).checkoutSource("ocs_1").pay({ option_id: "o" })
      ).resolves.toEqual(want)
    }
    const busy = createBillingClient({
      fetch: async () => refusal(409, "checkout_payment_in_progress"),
    })
    await expect(
      checkoutOf(busy).checkoutSource("ocs_1").pay({ option_id: "o" })
    ).rejects.toMatchObject({ status: 409 })
    await expect(checkoutOf(busy).createCheckoutSession({})).rejects.toMatchObject({
      code: "invalid_request",
    })
  })
})

describe("listProducts", () => {
  it("looks products up by key, a parameter per key", async () => {
    const server = fakeBilling({
      products: [
        product({ id: "prod_a", key: "a", entitlements: ["course:101"] }),
        product({ id: "prod_b", key: "b", entitlements: ["course:102"] }),
      ],
    })
    const client = createBillingClient({ fetch: server.fetch })
    const page = await client.listProducts({ keys: ["a", "c"] })
    expect(page.data.map((p) => p.key)).toEqual(["a"])
    const [url] = server.fetch.mock.calls[0]!
    expect(String(url)).toBe("/billing/v1/catalog/products?keys=a&keys=c")
  })

  it("asks nothing for no keys", async () => {
    const server = fakeBilling()
    const client = createBillingClient({ fetch: server.fetch })
    expect(await client.listProducts({ keys: [] })).toEqual({ data: [], next_cursor: null })
    expect(server.fetch).not.toHaveBeenCalled()
  })
})
