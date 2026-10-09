import { describe, expect, it, vi } from "vitest"

import { takeMerchantFromHash } from "@/lib/api/client"

const at = (hash: string) => {
  const location = {
    pathname: "/admin/settings",
    search: "?tab=psps",
    hash,
  } as Location
  const history = { state: null, replaceState: vi.fn() } as unknown as History
  return {
    location,
    history,
    replaced: () => vi.mocked(history.replaceState).mock.calls,
  }
}

describe("#merchant= deep link", () => {
  it("is consumed and dropped from the address", () => {
    const page = at("#merchant=acme")
    expect(takeMerchantFromHash(page.location, page.history)).toBe("acme")
    expect(page.replaced()).toEqual([[null, "", "/admin/settings?tab=psps"]])
  })

  it("keeps any other fragment", () => {
    const page = at("#merchant=acme&section=keys")
    expect(takeMerchantFromHash(page.location, page.history)).toBe("acme")
    expect(page.replaced()[0][2]).toBe("/admin/settings?tab=psps#section=keys")
  })

  it("leaves a page without one untouched", () => {
    const page = at("#section=keys")
    expect(takeMerchantFromHash(page.location, page.history)).toBeUndefined()
    expect(page.replaced()).toEqual([])
  })

  it("ignores an empty slug but still drops it", () => {
    const page = at("#merchant=")
    expect(takeMerchantFromHash(page.location, page.history)).toBeUndefined()
    expect(page.replaced()).toHaveLength(1)
  })
})
