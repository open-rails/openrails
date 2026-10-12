import { afterEach, describe, expect, it, vi } from "vitest"

// A fresh page: the module's memory is gone, the tab's storage is not.
const reload = async () => {
  vi.resetModules()
  return import("./purchase-key")
}

afterEach(() => {
  vi.restoreAllMocks()
  sessionStorage.clear()
})

describe("purchase keys", () => {
  it("survive a reload per customer, price and amount", async () => {
    const page = await reload()
    const slot = { customer: "cus_a", price: "course:once:v1" }
    const first = page.purchaseKey(slot)
    expect(first.resumed).toBe(false)
    expect(page.purchaseKey(slot)).toEqual({ key: first.key, resumed: true })

    const reloaded = await reload()
    expect(reloaded.purchaseKey(slot)).toEqual({
      key: first.key,
      resumed: true,
    })
    // Another customer in the same tab never shares it, nor another price
    // or deposit amount.
    for (const other of [
      { ...slot, customer: "cus_b" },
      { ...slot, price: "course:rent:v1" },
      { ...slot, amount: "100000000" },
    ])
      expect(reloaded.purchaseKey(other).key).not.toBe(first.key)
  })

  it("are released once the purchase is paid or closed", async () => {
    const page = await reload()
    const slot = { customer: "cus_a", price: "course:once:v1" }
    const { key } = page.purchaseKey(slot)
    page.releasePurchaseKey(slot)
    const next = page.purchaseKey(slot)
    expect(next.key).not.toBe(key)
    expect(next.resumed).toBe(false)
    const reloaded = await reload()
    expect(reloaded.purchaseKey(slot).key).toBe(next.key)
  })

  it("last as long as the page without storage", async () => {
    const off = () => {
      throw new DOMException("storage disabled", "SecurityError")
    }
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(off)
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(off)
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(off)
    const page = await reload()
    const slot = { customer: "cus_a", price: "course:once:v1" }
    const { key } = page.purchaseKey(slot)
    expect(page.purchaseKey(slot)).toEqual({ key, resumed: true })
    page.releasePurchaseKey(slot)
    expect(page.purchaseKey(slot).key).not.toBe(key)
    const reloaded = await reload()
    expect(reloaded.purchaseKey(slot).resumed).toBe(false)
  })
})
