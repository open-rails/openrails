// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest"

import { loadMerchants } from "@/lib/auth"
import { selectedMerchant, setSelectedMerchant } from "@/lib/api/client"
import { server } from "@/test/harness"
import { browserEnvironment } from "@/test/mount"

beforeEach(async () => {
  browserEnvironment()
  await server()
})

// OpenRails lists no user's merchants: the console asks a host's directory,
// else acts for the merchant opened by name. It never calls the admin API
// for them.
describe("the console's merchants", () => {
  it("come from a host's directory, sorted, the first selected", async () => {
    const merchants = await loadMerchants({
      id: "dir",
      merchants: () =>
        Promise.resolve([
          { id: "m2", slug: "zeta", role: "viewer" },
          { id: "m1", slug: "acme", role: "owner" },
        ]),
    })
    expect(merchants.map((m) => m.slug)).toEqual(["acme", "zeta"])
    expect(selectedMerchant()).toBe("acme")
  })

  it("are the one opened by name, or none", async () => {
    expect(await loadMerchants()).toEqual([])
    setSelectedMerchant("acme")
    expect(await loadMerchants()).toEqual([{ id: "", slug: "acme" }])
    expect(selectedMerchant()).toBe("acme")
  })
})
