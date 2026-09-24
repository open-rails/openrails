import { describe, expect, it, vi } from "vitest"

import errorFixture from "../test/fixtures/wire/error_envelope.json"
import subscriptionFixture from "../test/fixtures/wire/subscription.json"
import { fakeBilling, json, subscription } from "../test/billing-server"
import { createBillingClient, WalletRejectedError } from "./client"
import { BillingError } from "./errors"
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
    const err = await client.getStatus().catch((e: unknown) => e)
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
