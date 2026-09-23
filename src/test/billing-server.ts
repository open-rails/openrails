// In-memory `/billing/v1/me` for jsdom tests, shaped by the OpenRails wire
// fixtures. Real-server coverage lives in the Playwright e2e suite.
import { vi, type Mock } from "vitest"

import subscriptionFixture from "./fixtures/wire/subscription.json"

export const json = (status: number, body?: unknown) =>
  new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  })

export const apiError = (status: number, code: string, message = code) =>
  json(status, { error: { type: "invalid_request_error", code, message } })

type Row = Record<string, unknown> & { id: string }

export function subscription(overrides: Partial<Row> = {}): Row {
  return {
    ...subscriptionFixture,
    payments: undefined,
    cancel_portal_url: undefined,
    current_period_ends_at: "2036-09-16T12:00:00Z",
    price: {
      ...subscriptionFixture.price,
      unit_amount: "9990000",
      access_duration_hours: 720,
    },
    ...overrides,
  } as Row
}

export function paymentMethod(overrides: Partial<Row> = {}): Row {
  return {
    id: "pm_1",
    object: "payment_method",
    type: "card",
    rail: "nmi",
    psp_id: "55555555-5555-5555-5555-555555555555",
    card: { brand: "visa", last4: "4242", exp_month: 12, exp_year: 2030 },
    health: { expiry_status: "valid", active: true },
    created_at: "2026-09-01T00:00:00Z",
    ...overrides,
  } as Row
}

export function payment(overrides: Partial<Row> = {}): Row {
  return {
    id: "pay_1",
    object: "charge",
    status: "succeeded",
    amount: "9990000",
    amount_refunded: "0",
    currency: "USD",
    customer_id: "22222222-2222-2222-2222-222222222222",
    rail: "nmi",
    refunded: false,
    created_at: "2026-09-16T00:00:00Z",
    card: { brand: "visa", last4: "4242" },
    ...overrides,
  } as Row
}

const page = (data: Row[], limit: number, offset: number, total: number) => ({
  object: "list",
  data,
  total,
  limit,
  offset,
  has_more: offset + data.length < total,
})

export interface FakeBilling {
  subscriptions: Row[]
  methods: Row[]
  payments: Row[]
  /** Applies queued cancel/resume on the Nth later read (server lag). */
  lag: number
  /** Next response override per "METHOD /path". */
  fail: Record<string, Response>
  calls: string[]
  fetch: Mock<typeof fetch>
}

export function fakeBilling(
  seed: Partial<
    Pick<FakeBilling, "subscriptions" | "methods" | "payments">
  > = {}
): FakeBilling {
  const state: FakeBilling = {
    subscriptions: seed.subscriptions ?? [subscription()],
    methods: seed.methods ?? [paymentMethod()],
    payments: seed.payments ?? [payment()],
    lag: 0,
    fail: {},
    calls: [],
    fetch: vi.fn<typeof fetch>(),
  }
  const queued: { reads: number; apply: () => void }[] = []

  const findSub = (id: string) => state.subscriptions.find((s) => s.id === id)

  state.fetch.mockImplementation(
    async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = new URL(String(input), "http://host.test")
      const method = init.method ?? "GET"
      const path = url.pathname.replace(/^\/billing\/v1/, "")
      const key = `${method} ${path}`
      state.calls.push(key)
      const failure = state.fail[key]
      if (failure) {
        delete state.fail[key]
        return failure
      }
      const body = init.body ? JSON.parse(String(init.body)) : undefined
      const limit = Number(url.searchParams.get("limit") ?? 20)
      const offset = Number(url.searchParams.get("offset") ?? 0)
      let m: RegExpMatchArray | null

      if (key === "GET /me/subscriptions")
        return json(
          200,
          page(state.subscriptions, limit, offset, state.subscriptions.length)
        )
      if ((m = key.match(/^GET \/me\/subscriptions\/([^/]+)$/))) {
        for (const q of [...queued])
          if (q.reads-- <= 0) {
            q.apply()
            queued.splice(queued.indexOf(q), 1)
          }
        const sub = findSub(decodeURIComponent(m[1]))
        return sub ? json(200, sub) : apiError(404, "resource_not_found")
      }
      if ((m = key.match(/^POST \/me\/subscriptions\/([^/]+)\/cancel$/))) {
        const sub = findSub(decodeURIComponent(m[1]))
        if (!sub) return apiError(404, "resource_not_found")
        if (String(body?.feedback ?? "").trim().length < 4)
          return apiError(400, "invalid_param")
        queued.push({
          reads: state.lag,
          apply: () =>
            Object.assign(sub, {
              status: "cancelled",
              cancel_scheduled: true,
              resumable: true,
            }),
        })
        return json(202, { status: "queued" })
      }
      if ((m = key.match(/^POST \/me\/subscriptions\/([^/]+)\/resume$/))) {
        const sub = findSub(decodeURIComponent(m[1]))
        if (!sub) return apiError(404, "resource_not_found")
        queued.push({
          reads: state.lag,
          apply: () =>
            Object.assign(sub, {
              status: "active",
              cancel_scheduled: false,
              resumable: false,
            }),
        })
        return json(202, { status: "queued" })
      }
      if (/^POST \/me\/subscriptions\/[^/]+\/solana-cancel-tx$/.test(key))
        return json(200, { transaction: "dHg=", subscription_pda: "pda" })
      if (
        (m = key.match(/^POST \/me\/subscriptions\/([^/]+)\/solana-cancel$/))
      ) {
        const sub = findSub(decodeURIComponent(m[1]))
        if (!sub) return apiError(404, "resource_not_found")
        Object.assign(sub, {
          status: "cancelled",
          ended_at: sub.current_period_ends_at,
        })
        return json(200, { subscription_id: sub.id, status: "cancelled" })
      }
      if (key === "GET /me/payment-methods")
        return json(
          200,
          page(state.methods, limit, offset, state.methods.length)
        )
      if (key === "POST /me/payment-methods") {
        const created = paymentMethod({
          id: `pm_${state.methods.length + 1}`,
          card: {
            brand: "mastercard",
            last4: "5454",
            exp_month: 1,
            exp_year: 2031,
          },
        })
        state.methods.push(created)
        return json(200, created)
      }
      if ((m = key.match(/^DELETE \/me\/payment-methods\/([^/]+)$/))) {
        const id = decodeURIComponent(m[1])
        state.methods = state.methods.filter((pm) => pm.id !== id)
        return new Response(null, { status: 204 })
      }
      if (key === "PUT /me/collection-payment-method") {
        const code = String(body.currency)
        for (const pm of state.methods) {
          const others = (
            (pm.collection_default_currencies as string[]) ?? []
          ).filter((c) => c !== code)
          pm.collection_default_currencies =
            pm.id === body.payment_method_id ? [...others, code] : others
        }
        return json(200, { success: true })
      }
      if (key === "GET /me/payments")
        return json(
          200,
          page(
            state.payments.slice(offset, offset + limit),
            limit,
            offset,
            state.payments.length
          )
        )
      return apiError(404, "resource_not_found", `no route ${key}`)
    }
  )
  return state
}
