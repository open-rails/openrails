import { act, renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import { createBillingClient } from "../client/client"
import {
  apiError,
  fakeBilling,
  payment,
  paymentMethod,
  subscription,
  type FakeBilling,
} from "../test/billing-server"
import { useBillingRefresh } from "./context"
import { usePaymentMethods, usePayments, useSubscriptions } from "./hooks"
import { BillingProvider, type BillingProviderProps } from "./provider"

function setup<T>(
  hook: () => T,
  server: FakeBilling,
  props: Partial<BillingProviderProps> = {}
) {
  const client = createBillingClient({ fetch: server.fetch })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <BillingProvider
      client={client}
      settle={{ intervalMs: 1, attempts: 5 }}
      {...props}
    >
      {children}
    </BillingProvider>
  )
  return renderHook(hook, { wrapper })
}

describe("useSubscriptions", () => {
  it("cancels, waits for the queued change and notifies the host", async () => {
    const server = fakeBilling()
    server.lag = 2
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
      type: "subscription.cancelled",
      subscriptionId: id,
      settled: true,
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
      "subscription is not cancelled"
    )
    let error: unknown
    await act(async () => {
      error = await result.current.resume(id)
    })
    expect(error).toMatchObject({ status: 400, code: "invalid_param" })
  })

  it("tracks Solana stages per row", async () => {
    const server = fakeBilling({
      subscriptions: [subscription({ id: "sub_sol", rail: "solana" })],
    })
    const { result } = setup(() => useSubscriptions(), server)
    await waitFor(() => expect(result.current.subscriptions).not.toBeNull())
    let sign!: (signature: string) => void
    let done!: Promise<unknown>
    act(() => {
      done = result.current.cancelOnChain(
        "sub_sol",
        () => new Promise((resolve) => (sign = resolve))
      )
    })
    await waitFor(() => expect(result.current.pending.sub_sol).toBe("signing"))
    await act(async () => {
      sign("sig")
      expect(await done).toBeNull()
    })
    expect(result.current.subscriptions![0].status).toBe("cancelled")
  })
})

describe("usePaymentMethods", () => {
  it("adds, sets a currency default and removes", async () => {
    const server = fakeBilling({ methods: [paymentMethod()] })
    const onChange = vi.fn()
    const { result } = setup(() => usePaymentMethods(), server, { onChange })
    await waitFor(() => expect(result.current.methods).toHaveLength(1))

    await act(async () => {
      await result.current.add({ payment_token: "tok", name_on_card: "A" })
    })
    await waitFor(() => expect(result.current.methods).toHaveLength(2))

    await act(async () => {
      await result.current.setDefault("pm_2", "usd")
    })
    expect(
      result.current.methods!.find((m) => m.id === "pm_2")
        ?.collection_default_currencies
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
  it("pages by offset using has_more", async () => {
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
