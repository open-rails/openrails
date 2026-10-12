import { describe, expect, it, vi } from "vitest"

import paymentMethodFixture from "../../../../testdata/wire/payment_method.json"
import errorFixture from "../../../../testdata/wire/error_envelope.json"
import subscriptionFixture from "../../../../testdata/wire/subscription.json"
import usageFixture from "../../../../testdata/wire/usage.json"
import {
  apiError,
  fakeBilling,
  json,
  MERCHANT,
  price,
  product,
  subscription,
} from "../test/billing-server"
import {
  createBillingClient,
  RETRY_BUDGET_MS,
  signWalletAction,
  WalletRejectedError,
} from "./client"
import { BillingError, isServerError } from "./errors"
import { tokenSource } from "./token-source"
import {
  priceSchema,
  productSchema,
  subscriptionSchema,
  usageSchema,
} from "./types"

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

  it("decodes the canonical OpenRails usage report", () => {
    expect(usageSchema.parse(usageFixture)).toMatchObject({
      group_by: "event_type",
      rows: [
        {
          key: "gpu",
          event_count: 3,
          amount: "9223372036854775807",
          dimensions: { seconds: 90 },
        },
      ],
      next_cursor: "next-page",
    })
  })

  it("decodes the error envelope", async () => {
    const client = createBillingClient({
      fetch: async () => json(422, errorFixture),
    })
    const err = await client.listOrders().catch((e: unknown) => e)
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
          accrued_amount: "0",
          available_amount: "0",
          owed_amount: "9223372036854775807",
          accrual_rate_per_hour: "0",
          as_of: "2026-01-02T03:04:05Z",
          depletes_at: null,
        },
      ],
      default_payment_methods: [{ currency: "USD", payment_method_id: "pm_1" }],
    }
    const fetch = vi.fn(async () => json(200, account))
    const client = createBillingClient({
      baseUrl: "https://shop.test/billing/v1/",
      fetch,
    })
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

  it("pages the usage report by cursor", async () => {
    const page = (next_cursor: string | null) =>
      json(200, { ...usageFixture, next_cursor })
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(page("c2"))
      .mockResolvedValueOnce(page(null))
    const client = createBillingClient({ fetch })
    const first = await client.getUsage({
      currency: "USD",
      groupBy: "resource",
      limit: 2,
    })
    expect(first.next_cursor).toBe("c2")
    const last = await client.getUsage({
      currency: "USD",
      groupBy: "resource",
      limit: 2,
      cursor: first.next_cursor,
    })
    expect(last.next_cursor).toBeNull()
    expect(fetch.mock.calls.map(([url]) => url)).toEqual([
      "/billing/v1/me/usage?currency=USD&group_by=resource&limit=2",
      "/billing/v1/me/usage?currency=USD&group_by=resource&limit=2&cursor=c2",
    ])
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
      .listOrders()
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
      createBillingClient({ fetch: slow }).listOrders()
    ).rejects.toMatchObject({ status: 429 })
    expect(slow).toHaveBeenCalledTimes(1)
    const quick = calls(() => status(503, { "Retry-After": "1" }), ok)
    const started = Date.now()
    await createBillingClient({ fetch: quick }).listPaymentMethods()
    expect(quick).toHaveBeenCalledTimes(2)
    expect(Date.now() - started).toBeLessThan(RETRY_BUDGET_MS)
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
    const page = await client.listProducts({
      merchant: MERCHANT,
      keys: ["a", "c"],
    })
    expect(page.data.map((p) => p.key)).toEqual(["a"])
    const [url] = server.fetch.mock.calls[0]!
    expect(String(url)).toBe(
      `/billing/v1/products?merchant=${MERCHANT}&keys=a&keys=c`
    )
  })

  it("answers another merchant's catalog as the server does", async () => {
    const server = fakeBilling({
      products: [product({ id: "prod_a", key: "a" })],
    })
    const client = createBillingClient({ fetch: server.fetch })
    await expect(
      client.listProducts({
        merchant: "0190a000-0000-7000-8000-00000000000b",
        keys: ["a"],
      })
    ).rejects.toMatchObject({ status: 404, code: "resource_not_found" })
  })

  it("asks nothing for no keys", async () => {
    const server = fakeBilling()
    const client = createBillingClient({ fetch: server.fetch })
    expect(await client.listProducts({ merchant: MERCHANT, keys: [] })).toEqual(
      { data: [], next_cursor: null }
    )
    expect(server.fetch).not.toHaveBeenCalled()
  })
})

describe("an expired token", () => {
  const expired = () => apiError(401, "credential_expired")
  // tokens hands out t1, t2, ... and counts what the host was asked for.
  const tokens = () => {
    let n = 0
    const fetchToken = vi.fn(async () => ({
      access_token: `t${++n}`,
      expires_in: 900,
    }))
    return { fetchToken, getToken: tokenSource({ fetchToken }) }
  }
  // answering serves each request the next answer and records what it carried.
  const answering = (...answers: (() => Response)[]) => {
    const sent: { method?: string; bearer: string | null }[] = []
    const fetch = async (_input: string, init: RequestInit) => {
      sent.push({
        method: init.method,
        bearer: new Headers(init.headers).get("Authorization"),
      })
      return (answers.shift() ?? expired)()
    }
    return { fetch, sent }
  }

  it("is replaced once and the call sent again, a write too", async () => {
    const { fetchToken, getToken } = tokens()
    const { fetch, sent } = answering(expired, () =>
      json(200, { ...subscription(), id: "sub_1" })
    )
    const client = createBillingClient({ fetch, getToken })
    await client.resumeSubscription("sub_1")
    expect(sent).toEqual([
      { method: "POST", bearer: "Bearer t1" },
      { method: "POST", bearer: "Bearer t2" },
    ])
    expect(fetchToken).toHaveBeenCalledTimes(2)
  })

  it("is not replaced a second time", async () => {
    const { fetchToken, getToken } = tokens()
    const { fetch, sent } = answering(expired, expired, expired)
    const client = createBillingClient({ fetch, getToken })
    await expect(client.getAccount()).rejects.toMatchObject({
      status: 401,
      code: "credential_expired",
    })
    expect(sent.map((request) => request.bearer)).toEqual([
      "Bearer t1",
      "Bearer t2",
    ])
    expect(fetchToken).toHaveBeenCalledTimes(2)
  })

  it("is left to the host without a token source, and so is any other 401", async () => {
    const plain = vi.fn(async () => expired())
    await expect(
      createBillingClient({
        fetch: plain,
        getToken: () => "host-token",
      }).getAccount()
    ).rejects.toMatchObject({ code: "credential_expired" })
    expect(plain).toHaveBeenCalledTimes(1)

    const { fetchToken, getToken } = tokens()
    const refused = vi.fn(async () => apiError(401, "authentication_required"))
    await expect(
      createBillingClient({ fetch: refused, getToken }).getAccount()
    ).rejects.toMatchObject({
      code: "authentication_required",
    })
    expect(refused).toHaveBeenCalledTimes(1)
    expect(fetchToken).toHaveBeenCalledTimes(1)
  })

  it("is never asked for by the public reads", async () => {
    const { fetchToken, getToken } = tokens()
    const server = fakeBilling({
      products: [product({ id: "prod_a", key: "a" })],
    })
    const client = createBillingClient({ fetch: server.fetch, getToken })
    await client.getConfig()
    await client.listProducts({ merchant: MERCHANT, keys: ["a"] })
    expect(fetchToken).not.toHaveBeenCalled()
    expect(
      server.fetch.mock.calls.map(([, init]) =>
        new Headers(init?.headers).get("Authorization")
      )
    ).toEqual([null, null])
  })
})

it("keeps the custodian capture key and confirms its pending method", async () => {
  const calls: { path: string; init: RequestInit }[] = []
  const capture = {
    custodian_id: "custodian-one",
    session_id: "session-one",
    token: "session-token-one",
  }
  const client = createBillingClient({
    fetch: async (path, init) => {
      calls.push({ path, init })
      return json(
        200,
        calls.length === 1
          ? {
              ...paymentMethodFixture,
              status: "requires_action",
              psp_id: null,
              next_action: {
                type: "capture",
                payload: { session_id: capture.session_id },
              },
            }
          : paymentMethodFixture
      )
    },
  })
  const pending = await client.addPaymentMethod(
    { psp_id: "psp-one" },
    "capture-key"
  )
  expect(pending.next_action?.type).toBe("capture")
  expect(new Headers(calls[0].init.headers).get("Idempotency-Key")).toBe(
    "capture-key"
  )
  expect(JSON.parse(String(calls[0].init.body))).toEqual({ psp_id: "psp-one" })
  await client.confirmPaymentMethod(pending.id, capture)
  expect(calls[1].path).toContain(`/me/payment-methods/${pending.id}/confirm`)
  expect(JSON.parse(String(calls[1].init.body))).toEqual({ capture })
})
