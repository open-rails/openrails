import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { NoMerchants } from "@/components/no-merchants"
import { AppLayout } from "@/layouts/app-layout"
import { getBootstrap } from "@/lib/api/client"
import { render, server } from "@/test/harness"

const auth = vi.hoisted(() => ({
  value: {
    ready: true,
    signedIn: true,
    merchantsFailed: false,
    merchants: [] as unknown[],
    me: { email: "ada@example.com" },
    logout: () => Promise.resolve(),
  },
}))
vi.mock("@/lib/auth", () => ({ useAuth: () => auth.value }))

beforeEach(async () => {
  await server()
  auth.value = { ...auth.value, merchants: [], merchantsFailed: false }
})
afterEach(() => {
  getBootstrap().new_merchant_url = ""
})

describe("a user with no merchants", () => {
  it("is pointed at an operator on a standalone deployment", () => {
    const html = render(<NoMerchants />)
    expect(html).toContain("You don&#x27;t have access to any merchants yet")
    expect(html).toContain("Ask an operator of this OpenRails deployment")
    expect(html).not.toContain("New merchant")
  })

  it("gets the host's New merchant action when one is configured", () => {
    getBootstrap().new_merchant_url = "/merchants/new"
    const html = render(<NoMerchants />)
    expect(html).toContain("New merchant")
    expect(html).toContain("Create a merchant to start accepting payments")
    expect(html).not.toContain("Ask an operator")
  })

  it("sees the empty state instead of a merchant's dashboard", () => {
    expect(render(<AppLayout />)).toContain(
      "You don&#x27;t have access to any merchants yet"
    )
  })

  it("is told a failed load is a failure, not an empty list", () => {
    auth.value = { ...auth.value, merchantsFailed: true }
    const html = render(<AppLayout />)
    expect(html).toContain("Could not load your merchants.")
    expect(html).not.toContain("any merchants yet")
  })
})
