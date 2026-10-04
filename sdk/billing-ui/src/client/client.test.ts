import { describe, expect, it, vi } from "vitest"

import errorFixture from "../test/fixtures/wire/error_envelope.json"
import subscriptionFixture from "../test/fixtures/wire/subscription.json"
import { fakeBilling, json, subscription } from "../test/billing-server"
import {
  createBillingClient,
  RETRY_BUDGET_MS,
  WalletRejectedError,
} from "./client"
import { BillingError, isServerError } from "./errors"
import { subscriptionSchema } from "./types"

describe("wire fixtures", () => {
  it("decodes the canonical OpenRails subscription", () => {
    const parsed = subscriptionSchema.parse(subscriptionFixture)
    expect(parsed).toMatchObject({
      id: "sub_cccccccc-cccc-4ccc-8ccc-cccccccccccc",
      status: "active",
      cancel_mode: "reversible",
      price: { unit_amount: "9223372036854775807", currency: "USD" },
      product: { display_name: "Pro" },
      card: { brand: "visa", last4: "4242" },
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
  it("targets the mount, encodes ids and attaches the bearer", async () => {
    const fetch = vi.fn(async () => new Response(null, { status: 202 }))
    const client = createBillingClient({
      baseUrl: "https://shop.test/billing/v1/",
      fetch,
      getToken: async () => "tok",
      language: () => "de",
    })
    await client.cancelSubscription("sub_a/b", { feedback: "  too pricey " })
    const [url, init] = fetch.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe(
      "https://shop.test/billing/v1/me/subscriptions/sub_a%2Fb/cancel"
    )
    expect(init.method).toBe("POST")
    expect(JSON.parse(String(init.body))).toEqual({ feedback: "too pricey" })
    const headers = new Headers(init.headers)
    expect(headers.get("Authorization")).toBe("Bearer tok")
    expect(headers.get("Accept-Language")).toBe("de")
  })

  it("lists every status by default and pages payments by offset", async () => {
    const server = fakeBilling()
    const client = createBillingClient({ fetch: server.fetch })
    const subs = await client.listSubscriptions()
    expect(subs.data).toHaveLength(1)
    expect(server.fetch.mock.calls[0][0]).toBe(
      "/billing/v1/me/subscriptions?status=all&limit=100"
    )
    await client.listPayments({ limit: 5, offset: 10 })
    expect(server.fetch.mock.calls[1][0]).toBe(
      "/billing/v1/me/payments?limit=5&offset=10"
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

  it("runs the Solana cancel loop and maps a declined wallet", async () => {
    const server = fakeBilling({
      subscriptions: [subscription({ id: "sub_sol", rail: "solana" })],
    })
    const client = createBillingClient({ fetch: server.fetch })
    const stages: string[] = []
    const send = vi.fn(async (tx: string) => `sig-for-${tx}`)
    await client.cancelSubscriptionOnChain("sub_sol", send, (s) =>
      stages.push(s)
    )
    expect(stages).toEqual(["preparing", "signing", "confirming"])
    expect(send).toHaveBeenCalledWith("dHg=")
    expect(server.subscriptions[0].status).toBe("cancelled")

    await expect(
      client.cancelSubscriptionOnChain("sub_sol", async () => {
        throw new WalletRejectedError()
      })
    ).rejects.toMatchObject({ code: "wallet_rejected" })
  })
})

// #1088: server errors surface at once; only a GET retries, once, briefly.
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
        feedback: "too pricey",
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

describe("hosted checkout", () => {
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
    const link = await client.createCheckoutSession({
      priceKey: "monthly",
      successUrl: "https://host-one.example/welcome",
    })
    expect(link.url).toBe("https://pay.example/checkout#ocs_x")
    const paid = await client
      .checkoutSource(link.id)
      .pay({ option_id: "option_1" })
    expect(paid.status).toBe("succeeded")

    expect(calls[0].url).toBe("/billing/v1/me/checkout/sessions")
    expect(new Headers(calls[0].init.headers).get("Authorization")).toBe(
      "Bearer user-token"
    )
    expect(JSON.parse(String(calls[0].init.body))).toEqual({
      price_key: "monthly",
      success_url: "https://host-one.example/welcome",
    })
    expect(calls[1].url).toBe(`/billing/v1/checkout-sessions/${link.id}/pay`)
    expect(new Headers(calls[1].init.headers).get("Authorization")).toBeNull()
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
        client.checkoutSource("ocs_1").pay({ option_id: "o" })
      ).resolves.toEqual(want)
    }
    const busy = createBillingClient({
      fetch: async () => refusal(409, "checkout_payment_in_progress"),
    })
    await expect(
      busy.checkoutSource("ocs_1").pay({ option_id: "o" })
    ).rejects.toMatchObject({ status: 409 })
    await expect(busy.createCheckoutSession({})).rejects.toMatchObject({
      code: "invalid_request",
    })
  })
})
