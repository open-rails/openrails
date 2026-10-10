import { act, renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "../client/client"
import { isBillingError } from "../client/errors"
import {
  apiError,
  fakeBilling,
  payment,
  paymentMethod,
  product,
  subscription,
  type FakeBilling,
} from "../test/billing-server"
import { useConfig, useCurrencyScales } from "./config"
import { useBillingRefresh } from "./context"
import {
  usePaymentMethods,
  usePayments,
  useProducts,
  useSubscriptions,
} from "./hooks"
import { BillingProvider, type BillingProviderProps } from "./provider"

function setup<T>(
  hook: () => T,
  server: FakeBilling,
  props: Partial<BillingProviderProps> = {}
) {
  const client = createBillingClient({ fetch: server.fetch })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <BillingProvider client={client} {...props}>
      {children}
    </BillingProvider>
  )
  return renderHook(hook, { wrapper })
}

describe("useConfig", () => {
  it("fetches GET /config once for every reader", async () => {
    const server = fakeBilling({
      currencies: [{ code: "XTS", decimals: 3, minor_decimals: 3 }],
    })
    const { result } = setup(
      () => ({ a: useConfig(), b: useConfig(), scales: useCurrencyScales() }),
      server
    )
    expect(result.current.a.loading).toBe(true)
    expect(result.current.scales.USD).toBe(6)
    await waitFor(() => expect(result.current.a.config).not.toBeNull())
    expect(result.current.b.config).toBe(result.current.a.config)
    expect(result.current.scales.XTS).toBe(3)
    expect(result.current.scales.USD).toBe(6)
    expect(server.calls.filter((call) => call === "GET /config")).toHaveLength(
      1
    )

    server.fail["GET /config"] = apiError(404, "resource_not_found")
    act(() => result.current.a.refetch())
    await waitFor(() => expect(result.current.b.error?.status).toBe(404))
    expect(result.current.b.config).not.toBeNull()
  })
})

describe("useSubscriptions", () => {
  it("cancels, patches the row and notifies the host", async () => {
    const server = fakeBilling()
    const onChange = vi.fn()
    const { result } = setup(() => useSubscriptions(), server, { onChange })
    await waitFor(() => expect(result.current.subscriptions).toHaveLength(1))
    const id = result.current.subscriptions![0].id

    let done!: Promise<unknown>
    act(() => {
      done = result.current.cancel(id, "too expensive")
    })
    expect(result.current.pending[id]).toBe("cancel")
    await act(async () => {
      expect(await done).toBeNull()
    })
    expect(result.current.pending[id]).toBeUndefined()
    expect(result.current.subscriptions![0].cancel_scheduled).toBe(true)
    expect(onChange).toHaveBeenCalledWith({
      type: "subscription.canceled",
      subscriptionId: id,
    })

    await act(async () => {
      expect(await result.current.resume(id)).toBeNull()
    })
    await waitFor(() =>
      expect(result.current.subscriptions![0].cancel_scheduled).toBe(false)
    )
  })

  it("refetches when the host refreshes", async () => {
    const server = fakeBilling()
    const { result } = setup(
      () => ({ subs: useSubscriptions(), refresh: useBillingRefresh() }),
      server
    )
    await waitFor(() =>
      expect(result.current.subs.subscriptions).toHaveLength(1)
    )
    server.subscriptions[0].status = "past_due"
    act(() => result.current.refresh())
    await waitFor(() =>
      expect(result.current.subs.subscriptions![0].status).toBe("past_due")
    )
  })

  it("returns the server refusal instead of throwing", async () => {
    const server = fakeBilling()
    const { result } = setup(() => useSubscriptions(), server)
    await waitFor(() => expect(result.current.subscriptions).not.toBeNull())
    const id = result.current.subscriptions![0].id
    server.fail[`POST /me/subscriptions/${id}/resume`] = apiError(
      400,
      "invalid_param",
      "subscription is not canceled"
    )
    let error: unknown
    await act(async () => {
      error = await result.current.resume(id)
    })
    expect(error).toMatchObject({ status: 400, code: "invalid_param" })
  })

  it("signs a Solana cancel's next action and tracks its stages", async () => {
    const server = fakeBilling({
      subscriptions: [subscription({ id: "sub_sol", rail: "solana" })],
    })
    const { result } = setup(() => useSubscriptions(), server)
    await waitFor(() => expect(result.current.subscriptions).not.toBeNull())
    let error: unknown
    await act(async () => {
      error = await result.current.cancel("sub_sol", "too expensive")
    })
    expect(error).toMatchObject({ code: "wallet_required" })
    expect(server.subscriptions[0].status).toBe("active")

    let sign!: (signature: string) => void
    let done!: Promise<unknown>
    act(() => {
      done = result.current.cancel(
        "sub_sol",
        "too expensive",
        () => new Promise((resolve) => (sign = resolve))
      )
    })
    await waitFor(() => expect(result.current.pending.sub_sol).toBe("signing"))
    await act(async () => {
      sign("sig")
      expect(await done).toBeNull()
    })
    expect(result.current.subscriptions![0].status).toBe("canceled")
    const posts = server.fetch.mock.calls.filter(
      ([, i]) => i?.method === "POST"
    )
    expect(JSON.parse(String(posts.at(-1)?.[1]?.body))).toEqual({
      reason: "too expensive",
      signature: "sig",
    })
  })
})

describe("plan change", () => {
  it("lists the catalog", async () => {
    const server = fakeBilling({
      products: [product(), product({ id: "prod_pro", display_name: "Pro" })],
    })
    const { result } = setup(() => useProducts(), server)
    await waitFor(() => expect(result.current.products).toHaveLength(2))
    expect(result.current.nextCursor).toBeNull()
    expect(result.current.products![0].prices[0]).toMatchObject({
      id: "price_plus",
      unit_amount: "19990000",
    })
    expect(server.calls).toEqual(["GET /catalog/products"])
  })

  it("changes the subscription, refetches the list and notifies the host", async () => {
    const server = fakeBilling()
    const onChange = vi.fn()
    const { result } = setup(() => useSubscriptions(), server, { onChange })
    await waitFor(() => expect(result.current.subscriptions).toHaveLength(1))
    const id = result.current.subscriptions![0].id

    let done!: ReturnType<typeof result.current.changeSubscription>
    act(() => {
      done = result.current.changeSubscription(id, {
        priceId: "price_plus",
        idempotencyKey: "key-1",
      })
    })
    expect(result.current.pending[id]).toBe("change")
    const change = await act(() => done)
    expect(change).toMatchObject({
      status: "succeeded",
      price_id: "price_plus",
    })
    expect(result.current.pending[id]).toBeUndefined()
    await waitFor(() =>
      expect(result.current.subscriptions![0].price?.id).toBe("price_plus")
    )
    expect(onChange).toHaveBeenCalledWith({
      type: "subscription.changed",
      subscriptionId: id,
      change,
    })
  })

  it("returns a change refusal instead of throwing", async () => {
    const server = fakeBilling()
    const onChange = vi.fn()
    const { result } = setup(() => useSubscriptions(), server, { onChange })
    await waitFor(() => expect(result.current.subscriptions).not.toBeNull())
    const id = result.current.subscriptions![0].id
    server.fail[`POST /me/subscriptions/${id}/change`] = apiError(
      409,
      "subscription_change_renewal_due"
    )
    const refused = await act(() =>
      result.current.changeSubscription(id, {
        priceId: "price_plus",
        idempotencyKey: "key-1",
      })
    )
    expect(isBillingError(refused)).toBe(true)
    expect(refused).toMatchObject({
      status: 409,
      code: "subscription_change_renewal_due",
    })
    expect(onChange).not.toHaveBeenCalled()
  })
})

describe("usePaymentMethods", () => {
  it("adds, sets a currency's default card and removes", async () => {
    const server = fakeBilling({ methods: [paymentMethod()] })
    const onChange = vi.fn()
    const { result } = setup(() => usePaymentMethods(), server, { onChange })
    await waitFor(() => expect(result.current.methods).toHaveLength(1))

    await act(async () => {
      await result.current.add({
        psp_id: "psp_nmi",
        payment_token: "tok",
        billing_details: { name: "A" },
      })
    })
    await waitFor(() => expect(result.current.methods).toHaveLength(2))

    await act(async () => {
      await result.current.setDefault("pm_2", "usd")
    })
    expect(
      result.current.methods!.find((m) => m.id === "pm_2")?.default_currencies
    ).toEqual(["USD"])

    server.fail["DELETE /me/payment-methods/pm_1"] = apiError(
      409,
      "resource_conflict"
    )
    let error: unknown
    await act(async () => {
      error = await result.current.remove("pm_1")
    })
    expect(error).toMatchObject({ status: 409 })

    await act(async () => {
      await result.current.remove("pm_1")
    })
    await waitFor(() =>
      expect(result.current.methods!.map((m) => m.id)).toEqual(["pm_2"])
    )
    expect(onChange.mock.calls.map(([c]) => c.type)).toEqual([
      "payment_method.added",
      "payment_method.default_changed",
      "payment_method.removed",
    ])
  })
})

describe("usePayments", () => {
  it("pages by cursor", async () => {
    const server = fakeBilling({
      payments: Array.from({ length: 3 }, (_, i) =>
        payment({ id: `pay_${i}` })
      ),
    })
    const { result } = setup(() => usePayments({ pageSize: 2 }), server)
    await waitFor(() => expect(result.current.payments).toHaveLength(2))
    expect(result.current.hasMore).toBe(true)
    act(() => result.current.next())
    await waitFor(() =>
      expect(result.current.payments!.map((p) => p.id)).toEqual(["pay_2"])
    )
    expect(result.current.page).toBe(1)
    expect(result.current.hasMore).toBe(false)
  })
})
