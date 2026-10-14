// Catalog, plan-change and Solana calls. Bodies mirror the Go wire types
// (billing.Product, billing.Price, billing.SubscriptionChange*,
// billing.PublicConfig).
import { describe, expect, it, vi } from "vitest"

import currenciesFixture from "../../../../testdata/wire/currencies.json"
import { apiError, json, order, product } from "../test/billing-server"
import { createBillingClient } from "./client"
import { productSchema } from "./types"

const MAX = "9223372036854775807"

function served(...responses: Response[]) {
  const fetch = vi.fn<(input: string, init: RequestInit) => Promise<Response>>(
    async () => {
      const next = responses.shift()
      if (!next) throw new Error("unexpected request")
      return next
    }
  )
  const request = (n = 0) => {
    const [url, init] = fetch.mock.calls[n]
    return {
      url,
      method: init.method,
      body: init.body ? JSON.parse(String(init.body)) : undefined,
      headers: new Headers(init.headers),
    }
  }
  const client = createBillingClient({
    fetch,
    getToken: () => "customer-token",
  })
  return { client, fetch, request }
}

const price = {
  id: "price_1",
  key: "pro-monthly",
  product_id: "prod_1",
  archived: false,
  unit_amount: MAX,
  currency: "USD",
  access_duration_hours: 720,
  billing_interval_hours: 720,
  trial_unit_amount: null,
  trial_duration_hours: null,
  sell_via: ["card"],
  provider: null,
  created_at: "2026-09-16T00:00:00.123456789Z",
  updated_at: "2026-09-16T00:00:00.123456789Z",
}

describe("catalog", () => {
  it("reads a host page's product with its embedded prices, amounts exact", () => {
    const parsed = productSchema.parse(
      product({ id: "prod_1", display_name: "Plus", prices: [price] })
    )
    expect(parsed).toMatchObject({
      id: "prod_1",
      display_name: "Plus",
      tier_group: "membership",
      tier_rank: 2,
      prices: [
        {
          id: "price_1",
          unit_amount: MAX,
          access_duration_hours: 720,
          billing_interval_hours: 720,
          sell_via: ["card"],
        },
      ],
    })
    expect(
      productSchema.safeParse(
        product({ prices: [{ ...price, unit_amount: 999 }] })
      ).success
    ).toBe(false)
  })

  it("creates the customer's order with its embedded pay URL in one call", async () => {
    const payUrl = {
      pay_url: "https://pay.test/pay/ord_1#pus_secret",
      pay_url_expires_at: "2026-10-12T20:15:00Z",
      embedded: true,
      success_url: "https://shop.test/",
      cancel_url: null,
      saved_payment_methods: true,
    }
    const { client, request } = served(
      json(201, order({ id: "ord_1", ...payUrl }))
    )
    const { order: made } = await client.createOrder({
      lines: [{ price_id: "price_1" }],
      payUrl: { embedded: true },
      idempotencyKey: "k",
    })
    expect(made).toMatchObject(payUrl)
    expect(request()).toMatchObject({
      url: "/billing/v1/me/orders",
      method: "POST",
      body: {
        lines: [{ price_id: "price_1" }],
        pay_url: { embedded: true },
      },
    })
    expect(request().headers.get("Idempotency-Key")).toBe("k")
  })

  it("reads the deployment's configuration with no credential", async () => {
    const capabilities = {
      route_groups: {
        admin: true,
        catalog_write: false,
        merchant_config: false,
      },
      features: { merchant_config_edits: false },
    }
    const { client, request } = served(
      json(200, {
        capabilities,
        currencies: currenciesFixture,
        captcha: null,
        network: null,
      }),
      json(200, { currencies: currenciesFixture })
    )
    const config = await client.getConfig()
    expect(config).toEqual({
      capabilities,
      currencies: currenciesFixture,
      captcha: null,
      network: null,
    })
    expect(config.currencies).toContainEqual({
      code: "JPY",
      decimals: 4,
      minor_decimals: 0,
    })
    expect(request().url).toBe("/billing/v1/config")
    expect(request().headers.get("Authorization")).toBeNull()
    await expect(client.getConfig()).rejects.toMatchObject({
      code: "invalid_response",
    })
  })
})

describe("subscription change", () => {
  const preview = {
    object: "subscription_change_preview",
    price_id: "price_2",
    quantity: 3,
    rail: "nmi",
    currency: "USD",
    amount_due_now: "5000000",
    next_charge_amount: MAX,
    next_charge_date: "2026-10-16T00:00:00Z",
    effective: "now",
    is_estimate: false,
    message: "You'll be charged $5.00 now.",
  }

  it("previews without an idempotency key", async () => {
    const { client, request } = served(
      json(200, preview),
      apiError(400, "invalid_param", "cannot change to a different tier group"),
      json(200, { ...preview, amount_due_now: 5 })
    )
    expect(
      await client.previewSubscriptionChange("sub_a/b", {
        priceId: "price_2",
        quantity: 3,
      })
    ).toEqual({
      price_id: "price_2",
      quantity: 3,
      rail: "nmi",
      currency: "USD",
      amount_due_now: "5000000",
      next_charge_amount: MAX,
      next_charge_date: "2026-10-16T00:00:00Z",
      effective: "now",
      is_estimate: false,
      message: "You'll be charged $5.00 now.",
    })
    const sent = request()
    expect(sent).toMatchObject({
      url: "/billing/v1/me/subscriptions/sub_a%2Fb/change/preview",
      method: "POST",
      body: { price_id: "price_2", quantity: 3 },
    })
    expect(sent.headers.has("Idempotency-Key")).toBe(false)

    await expect(
      client.previewSubscriptionChange("sub_1", { priceId: "price_3" })
    ).rejects.toMatchObject({
      status: 400,
      code: "invalid_param",
      message: "cannot change to a different tier group",
    })
    await expect(
      client.previewSubscriptionChange("sub_1", { priceId: "price_2" })
    ).rejects.toMatchObject({ code: "invalid_response" })
  })

  it("changes under the caller's idempotency key", async () => {
    const { client, request } = served(
      json(200, {
        object: "subscription_change",
        status: "succeeded",
        effective: "now",
        price_id: "price_2",
        payment: { rail: "nmi" },
        subscription_id: "sub_2",
        currency: "USD",
        amount_due_now: "5000000",
        next_charge_amount: MAX,
        next_charge_date: "2026-10-16T00:00:00Z",
      })
    )
    const change = await client.changeSubscription("sub_1", {
      priceId: "price_2",
      idempotencyKey: "key-1",
    })
    expect(change).toMatchObject({
      status: "succeeded",
      effective: "now",
      subscription_id: "sub_2",
      amount_due_now: "5000000",
      next_charge_amount: MAX,
    })
    const sent = request()
    expect(sent).toMatchObject({
      url: "/billing/v1/me/subscriptions/sub_1/change",
      method: "POST",
      body: { price_id: "price_2" },
    })
    expect(sent.headers.get("Idempotency-Key")).toBe("key-1")
  })

  it("returns an unresolved change, its challenge and the one in flight", async () => {
    const { client, fetch } = served(
      json(202, {
        object: "subscription_change",
        status: "processing",
        price_id: "price_2",
        payment: { rail: "stripe" },
        amount_due_now: "0",
        next_charge_amount: "0",
      }),
      json(200, {
        object: "subscription_change",
        status: "requires_action",
        price_id: "price_2",
        payment: { rail: "stripe" },
        next_action: {
          type: "authenticate",
          url: null,
          psp_id: "psp_1",
          transactions: [],
          payload: { client_secret: "pi_1_secret", payment_intent_id: "pi_1" },
        },
        amount_due_now: "5000000",
        next_charge_amount: "19990000",
      }),
      json(409, {
        error: {
          type: "invalid_request_error",
          code: "subscription_change_in_flight",
          message: "another change of this subscription is unresolved",
        },
      }),
      new Response(null, { status: 503 })
    )
    const input = { priceId: "price_2", idempotencyKey: "key-1" }
    expect(await client.changeSubscription("sub_1", input)).toMatchObject({
      status: "processing",
    })
    expect(await client.changeSubscription("sub_1", input)).toMatchObject({
      status: "requires_action",
      next_action: {
        type: "authenticate",
        payload: { client_secret: "pi_1_secret" },
      },
    })
    await expect(
      client.changeSubscription("sub_1", input)
    ).rejects.toMatchObject({
      status: 409,
      code: "subscription_change_in_flight",
    })
    // A write is never retried: the caller replays its key.
    await expect(
      client.changeSubscription("sub_1", input)
    ).rejects.toMatchObject({
      status: 503,
    })
    expect(fetch).toHaveBeenCalledTimes(4)
  })
})

describe("Solana", () => {
  const token = {
    symbol: "USDC",
    name: "USD Coin",
    mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
    decimals: 6,
    preferred: true,
    recurring_eligible: true,
  }

  it("reads the network and tokens from the customer's merchant's payment setup", async () => {
    const { client, request } = served(
      json(200, {
        payment: {
          psps: [],
          solana: {
            network: "devnet",
            chain: "solana:devnet",
            preferred_token: "USDC",
            tokens: [token],
          },
        },
        merchant: {
          display_name: "Shop",
          logo_url: null,
          support_url: null,
          embed_origins: null,
        },
      }),
      json(500, {
        error: {
          type: "api_error",
          code: "internal_error",
          message: "failed to load payment configuration",
        },
      })
    )
    const config = await client.getCustomerConfig()
    expect(config.payment).toMatchObject({
      psps: [],
      solana: {
        network: "devnet",
        chain: "solana:devnet",
        preferred_token: "USDC",
        tokens: [{ symbol: "USDC", decimals: 6, recurring_eligible: true }],
      },
    })
    expect(config.merchant).toEqual({
      display_name: "Shop",
      logo_url: null,
      support_url: null,
      embed_origins: [],
    })
    expect(request().url).toBe("/billing/v1/me/config")
    expect(request().headers.get("Authorization")).toBe("Bearer customer-token")
    await expect(client.getCustomerConfig()).rejects.toMatchObject({
      status: 500,
      code: "internal_error",
    })
  })

  it("answers a tier change's wallet step and repeats it signed", async () => {
    const change = {
      object: "subscription_change",
      effective: "now",
      price_id: "price_2",
      payment: { rail: "solana" },
      currency: "USD",
      amount_due_now: "5000000",
      next_charge_amount: "19990000",
    }
    const { client, request } = served(
      json(200, {
        ...change,
        status: "requires_action",
        subscription_id: "sub_1",
        next_action: {
          type: "solana_sign_transactions",
          redirect_to_url: null,
          transactions: ["dHg="],
        },
      }),
      json(200, { ...change, status: "succeeded", subscription_id: "sub_2" })
    )
    const input = { priceId: "price_2", idempotencyKey: "key-1" }
    expect(await client.changeSubscription("sub_1", input)).toMatchObject({
      status: "requires_action",
      next_action: { type: "solana_sign_transactions", transactions: ["dHg="] },
    })
    expect(request().body).toEqual({ price_id: "price_2" })
    expect(
      await client.changeSubscription("sub_1", { ...input, signature: "sig" })
    ).toMatchObject({ status: "succeeded", subscription_id: "sub_2" })
    const sent = request(1)
    expect(sent.body).toEqual({ price_id: "price_2", signature: "sig" })
    expect(sent.headers.get("Idempotency-Key")).toBe("key-1")
  })
})
