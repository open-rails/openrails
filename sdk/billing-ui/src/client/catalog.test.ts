// Catalog, plan-change and Solana calls. Bodies mirror the Go wire types
// (api.ProductObject, billing.PublicPrice, billing.TierChange*Response,
// handlers.SolanaRuntimeConfigResponse).
import { describe, expect, it, vi } from "vitest"

import currenciesFixture from "../test/fixtures/wire/currencies.json"
import { apiError, json, product } from "../test/billing-server"
import { createBillingClient } from "./client"
import { BillingError } from "./errors"

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
  return { client: createBillingClient({ fetch }), fetch, request }
}

const list = (data: unknown[]) => ({
  object: "list",
  data,
  total: data.length,
  limit: 100,
  offset: 0,
  has_more: false,
})

const price = {
  id: "price_1",
  key: "pro-monthly",
  object: "price",
  unit_amount: MAX,
  currency: "USD",
  type: "recurring",
  recurring: { interval: "720h" },
  product: "prod_1",
  active: true,
  providers: ["cards", "solana"],
  metadata: {},
  created_at: "2026-09-16T00:00:00.123456789Z",
}

describe("catalog", () => {
  it("lists products with their embedded prices", async () => {
    const { client, request } = served(
      json(
        200,
        list([
          product({ id: "prod_1", prices: [price] }),
          { id: "prod_2", object: "product", name: "Bare", tier_rank: 0 },
        ])
      ),
      json(200, list([]))
    )
    const page = await client.listProducts()
    expect(request()).toMatchObject({
      url: "/billing/v1/products?limit=100",
      method: "GET",
    })
    expect(page.total).toBe(2)
    expect(page.data[0]).toMatchObject({
      id: "prod_1",
      name: "Plus",
      tier_group: "membership",
      tier_rank: 2,
      prices: [
        {
          id: "price_1",
          unit_amount: MAX,
          recurring: { interval: "720h" },
          providers: ["cards", "solana"],
        },
      ],
    })
    expect(page.data[1].prices).toEqual([])

    await client.listProducts({ limit: 5, offset: 10 })
    expect(request(1).url).toBe("/billing/v1/products?limit=5&offset=10")
  })

  it("rejects a price whose amount is not an exact string", async () => {
    const { client } = served(
      json(200, list([product({ prices: [{ ...price, unit_amount: 999 }] })]))
    )
    await expect(client.listProducts()).rejects.toMatchObject({
      code: "invalid_response",
    })
  })

  it("lists prices by currency, product and type", async () => {
    const { client, request } = served(
      json(200, list([price, { ...price, id: "price_2", type: "one_time" }])),
      apiError(400, "invalid_param", "Invalid product ID format")
    )
    const page = await client.listPrices({
      currency: "USD",
      product: "prod_1",
      type: "recurring",
    })
    expect(request().url).toBe(
      "/billing/v1/prices?currency=USD&product=prod_1&type=recurring&limit=100"
    )
    expect(page.data.map((p) => p.id)).toEqual(["price_1", "price_2"])
    expect(page.data[0]).toMatchObject({ unit_amount: MAX, product: "prod_1" })

    const err = await client
      .listPrices({ product: "nope" })
      .catch((e: unknown) => e)
    expect(err).toBeInstanceOf(BillingError)
    expect(err).toMatchObject({ status: 400, code: "invalid_param" })
  })

  it("reads the currency registry", async () => {
    const { client, request } = served(
      json(200, currenciesFixture),
      json(200, { object: "currencies" })
    )
    expect(await client.listCurrencies()).toEqual([
      { code: "EUR", decimals: 6, minor_decimals: 2 },
      { code: "JPY", decimals: 4, minor_decimals: 0 },
      { code: "USD", decimals: 6, minor_decimals: 2 },
    ])
    expect(request().url).toBe("/billing/v1/currencies")
    await expect(client.listCurrencies()).rejects.toMatchObject({
      code: "invalid_response",
    })
  })
})

describe("tier change", () => {
  const preview = {
    object: "tier_change_preview",
    action: "upgrade",
    price_id: "price_2",
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
    expect(await client.previewTierChange("sub_a/b", "price_2")).toEqual({
      action: "upgrade",
      price_id: "price_2",
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
      url: "/billing/v1/me/subscriptions/sub_a%2Fb/change-tier/preview",
      method: "POST",
      body: { price_id: "price_2" },
    })
    expect(sent.headers.has("Idempotency-Key")).toBe(false)

    await expect(
      client.previewTierChange("sub_1", "price_3")
    ).rejects.toMatchObject({
      status: 400,
      code: "invalid_param",
      message: "cannot change to a different tier group",
    })
    await expect(
      client.previewTierChange("sub_1", "price_2")
    ).rejects.toMatchObject({ code: "invalid_response" })
  })

  it("changes tier under the caller's idempotency key", async () => {
    const { client, request } = served(
      json(200, {
        object: "tier_change",
        status: "succeeded",
        mode: "tier_change",
        action: "upgrade",
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
    const change = await client.changeTier("sub_1", {
      priceId: "price_2",
      idempotencyKey: "key-1",
    })
    expect(change).toMatchObject({
      status: "succeeded",
      action: "upgrade",
      effective: "now",
      subscription_id: "sub_2",
      amount_due_now: "5000000",
      next_charge_amount: MAX,
    })
    const sent = request()
    expect(sent).toMatchObject({
      url: "/billing/v1/me/subscriptions/sub_1/change-tier",
      method: "POST",
      body: { price_id: "price_2" },
    })
    expect(sent.headers.get("Idempotency-Key")).toBe("key-1")
  })

  it("returns an unresolved change and names the one in flight", async () => {
    const { client, fetch } = served(
      json(202, {
        object: "tier_change",
        status: "processing",
        mode: "tier_change",
        price_id: "price_2",
        payment: { rail: "stripe" },
        amount_due_now: "0",
        next_charge_amount: "0",
        operation_id: "op_1",
      }),
      json(200, {
        object: "tier_change",
        status: "requires_action",
        mode: "tier_change",
        price_id: "price_2",
        payment: { rail: "stripe" },
        next_action: { type: "payment_authentication" },
        amount_due_now: "5000000",
        next_charge_amount: "19990000",
        operation_id: "op_1",
      }),
      json(409, {
        error: {
          type: "invalid_request_error",
          code: "tier_change_in_flight",
          message: "tier change op_1 is unresolved",
          metadata: { operation_id: "op_1" },
        },
      }),
      new Response(null, { status: 503 })
    )
    const input = { priceId: "price_2", idempotencyKey: "key-1" }
    expect(await client.changeTier("sub_1", input)).toMatchObject({
      status: "processing",
      operation_id: "op_1",
    })
    expect(await client.changeTier("sub_1", input)).toMatchObject({
      status: "requires_action",
      next_action: { type: "payment_authentication" },
      operation_id: "op_1",
    })
    await expect(client.changeTier("sub_1", input)).rejects.toMatchObject({
      status: 409,
      code: "tier_change_in_flight",
      metadata: { operation_id: "op_1" },
    })
    // A write is never retried: the caller replays its key.
    await expect(client.changeTier("sub_1", input)).rejects.toMatchObject({
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

  it("reads the runtime config", async () => {
    const { client, request } = served(
      json(200, {
        network: "devnet",
        chain: "solana:devnet",
        explorerCluster: "devnet",
        preferredToken: "USDC",
        tokens: [token],
        features: {
          solanaPay: true,
          recurringSubscriptions: true,
          solanaPayRecurringSubscriptions: true,
        },
      }),
      json(500, {
        error: {
          type: "api_error",
          code: "internal_error",
          message: "Solana configuration missing",
        },
      })
    )
    expect(await client.getSolanaConfig()).toMatchObject({
      network: "devnet",
      chain: "solana:devnet",
      explorerCluster: "devnet",
      preferredToken: "USDC",
      tokens: [{ symbol: "USDC", decimals: 6, recurring_eligible: true }],
      features: { recurringSubscriptions: true },
    })
    expect(request().url).toBe("/billing/v1/solana/config")
    await expect(client.getSolanaConfig()).rejects.toMatchObject({
      status: 500,
      code: "internal_error",
    })
  })

  it("quotes tokens for a price and wallet", async () => {
    const quoted = {
      ...token,
      price: "0.9998",
      quote: {
        amount: "18446744073709.551615",
        units: "18446744073709551615",
        token_price_usd: "0.9998",
        fx_rate: "1",
        fx_currency: "USD",
        quoted_at: "2026-09-16T00:00:00Z",
        expires_at: "2026-09-16T00:15:00Z",
      },
      balance: { amount: "1.5", units: "1500000", sufficient: false },
    }
    const { client, request } = served(
      json(200, { tokens: [quoted, { ...token, symbol: "SOL" }] }),
      json(200, {
        tokens: [{ ...quoted, quote: { ...quoted.quote, units: 1.8e19 } }],
      })
    )
    const tokens = await client.listSolanaTokens({
      priceId: "price_1",
      wallet: "wallet1",
    })
    expect(request().url).toBe(
      "/billing/v1/solana/tokens?price_id=price_1&wallet=wallet1"
    )
    expect(tokens[0]).toMatchObject({
      symbol: "USDC",
      price: "0.9998",
      quote: { units: "18446744073709551615", fx_currency: "USD" },
      balance: { units: "1500000", sufficient: false },
    })
    expect(tokens[1].quote).toBeUndefined()
    await expect(client.listSolanaTokens()).rejects.toMatchObject({
      code: "invalid_response",
    })
  })

  it("prepares the tier-change transaction", async () => {
    const { client, request } = served(
      json(200, {
        transaction: "dHg=",
        kind: "upgrade",
        new_subscription_pda: "pda",
      }),
      json(200, { transaction: "", kind: "upgrade" }),
      apiError(
        503,
        "service_unavailable",
        "Solana recurring billing is not configured"
      )
    )
    expect(await client.prepareSolanaTierChange("sub_a/b", "price_2")).toEqual({
      transaction: "dHg=",
      kind: "upgrade",
      new_subscription_pda: "pda",
    })
    expect(request()).toMatchObject({
      url: "/billing/v1/me/subscriptions/sub_a%2Fb/solana-tier-change",
      method: "POST",
      body: { new_price_id: "price_2" },
    })
    await expect(
      client.prepareSolanaTierChange("sub_1", "price_2")
    ).rejects.toMatchObject({ code: "invalid_response" })
    await expect(
      client.prepareSolanaTierChange("sub_1", "price_2")
    ).rejects.toMatchObject({ status: 503, code: "service_unavailable" })
  })

  it("confirms the signed tier change", async () => {
    const { client, request, fetch } = served(
      json(200, {
        subscription_id: "sub_2",
        new_subscription_id: "sub_2",
        kind: "downgrade",
        status: "active",
        already_confirmed: true,
      }),
      apiError(400, "invalid_param", "transaction not confirmed")
    )
    const input = { signature: "sig", newPriceId: "price_2" }
    expect(await client.confirmSolanaTierChange("sub_1", input)).toEqual({
      subscription_id: "sub_2",
      new_subscription_id: "sub_2",
      kind: "downgrade",
      status: "active",
      already_confirmed: true,
    })
    expect(request()).toMatchObject({
      url: "/billing/v1/me/subscriptions/sub_1/solana-tier-change/confirm",
      method: "POST",
      body: { signature: "sig", new_price_id: "price_2" },
    })
    await expect(
      client.confirmSolanaTierChange("sub_1", input)
    ).rejects.toMatchObject({
      status: 400,
      message: "transaction not confirmed",
    })
    expect(fetch).toHaveBeenCalledTimes(2)
  })
})
