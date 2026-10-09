// Catalog, plan-change and Solana calls. Bodies mirror the Go wire types
// (billing.Product, billing.Price, billing.SubscriptionChange*Response,
// handlers.SolanaRuntimeConfigResponse).
import { describe, expect, it, vi } from "vitest"

import currenciesFixture from "../test/fixtures/wire/currencies.json"
import { apiError, json, product } from "../test/billing-server"
import { createBillingClient } from "./client"

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

const list = (data: unknown[], next: string | null = null) => ({
  data,
  next_cursor: next,
})

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
  psps: { cards: { status: "linked", ids: null, sync_status: "unknown" } },
  created_at: "2026-09-16T00:00:00.123456789Z",
  updated_at: "2026-09-16T00:00:00.123456789Z",
}

describe("catalog", () => {
  it("lists products with their embedded prices", async () => {
    const { client, request } = served(
      json(
        200,
        list(
          [
            product({ id: "prod_1", display_name: "Plus", prices: [price] }),
            product({ id: "prod_2", display_name: "Bare", prices: [] }),
          ],
          "next"
        )
      ),
      json(200, list([]))
    )
    const page = await client.listProducts()
    expect(request()).toMatchObject({
      url: "/billing/v1/catalog/products?limit=100",
      method: "GET",
    })
    expect(page.next_cursor).toBe("next")
    expect(page.data[0]).toMatchObject({
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
          psps: { cards: { status: "linked" } },
        },
      ],
    })
    expect(page.data[1].prices).toEqual([])

    await client.listProducts({ limit: 5, cursor: "next" })
    expect(request(1).url).toBe("/billing/v1/catalog/products?limit=5&cursor=next")
  })

  it("rejects a price whose amount is not an exact string", async () => {
    const { client } = served(
      json(200, list([product({ prices: [{ ...price, unit_amount: 999 }] })]))
    )
    await expect(client.listProducts()).rejects.toMatchObject({
      code: "invalid_response",
    })
  })

  it("reads the public configuration", async () => {
    const capabilities = {
      route_groups: { admin: true, catalog_write: false, merchant_config: false },
      features: { team_invites: false },
    }
    const { client, request } = served(
      json(200, { capabilities, currencies: currenciesFixture, payment: null }),
      json(200, { currencies: currenciesFixture })
    )
    expect(await client.getConfig()).toEqual({
      capabilities,
      currencies: [
        { code: "EUR", decimals: 6, minor_decimals: 2 },
        { code: "JPY", decimals: 4, minor_decimals: 0 },
        { code: "USD", decimals: 6, minor_decimals: 2 },
      ],
      payment: null,
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

  it("returns an unresolved change and names the one in flight", async () => {
    const { client, fetch } = served(
      json(202, {
        object: "subscription_change",
        status: "processing",
        price_id: "price_2",
        payment: { rail: "stripe" },
        amount_due_now: "0",
        next_charge_amount: "0",
        operation_id: "op_1",
      }),
      json(200, {
        object: "subscription_change",
        status: "requires_action",
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
          code: "subscription_change_in_flight",
          message: "tier change op_1 is unresolved",
          metadata: { operation_id: "op_1" },
        },
      }),
      new Response(null, { status: 503 })
    )
    const input = { priceId: "price_2", idempotencyKey: "key-1" }
    expect(await client.changeSubscription("sub_1", input)).toMatchObject({
      status: "processing",
      operation_id: "op_1",
    })
    expect(await client.changeSubscription("sub_1", input)).toMatchObject({
      status: "requires_action",
      next_action: { type: "payment_authentication" },
      operation_id: "op_1",
    })
    await expect(client.changeSubscription("sub_1", input)).rejects.toMatchObject({
      status: 409,
      code: "subscription_change_in_flight",
      metadata: { operation_id: "op_1" },
    })
    // A write is never retried: the caller replays its key.
    await expect(client.changeSubscription("sub_1", input)).rejects.toMatchObject({
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

  it("reads the network and tokens from the payment setup", async () => {
    const { client, request } = served(
      json(200, {
        capabilities: { route_groups: {}, features: {} },
        currencies: currenciesFixture,
        payment: {
          psps: [],
          solana: {
            network: "devnet",
            chain: "solana:devnet",
            preferred_token: "USDC",
            tokens: [token],
          },
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
    expect((await client.getConfig()).payment).toMatchObject({
      psps: [],
      solana: {
        network: "devnet",
        chain: "solana:devnet",
        preferred_token: "USDC",
        tokens: [{ symbol: "USDC", decimals: 6, recurring_eligible: true }],
      },
    })
    expect(request().url).toBe("/billing/v1/config")
    await expect(client.getConfig()).rejects.toMatchObject({
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
