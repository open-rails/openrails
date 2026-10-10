import { beforeEach, describe, expect, it, vi } from "vitest"

import {
  settingsTab,
  useCatalogWrites,
  useMerchantConfig,
} from "@/lib/capabilities"
import { adminQueries } from "@/lib/queries"
import { client, render } from "@/test/harness"

// The catalog's query key names the selected merchant, kept in sessionStorage.
beforeEach(() => {
  const values = new Map<string, string>()
  vi.stubGlobal("sessionStorage", {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => void values.set(key, value),
    removeItem: (key: string) => void values.delete(key),
  })
})

function Probe() {
  const config = useMerchantConfig() ? "config" : "no-config"
  const edits = useCatalogWrites() ? "edits" : "no-edits"
  return (
    <>
      <span>{config}</span>
      <span>{edits}</span>
    </>
  )
}

const mounted = (groups: Record<string, boolean> | null) => {
  const queries = client()
  if (groups)
    queries.setQueryData(adminQueries.config().queryKey, {
      capabilities: { route_groups: groups, features: {} },
      currencies: [],
      rails: [],
      payment: null,
      captcha: null,
    })
  return render(<Probe />, queries)
}

describe("the merchant's configuration pages", () => {
  it("follow the capability route group", () => {
    expect(mounted({ admin: true, merchant_config: true })).toContain(
      ">config<"
    )
    expect(mounted({ admin: true, merchant_config: false })).toContain(
      "no-config"
    )
    expect(mounted({ admin: true })).toContain("no-config")
    expect(mounted(null)).toContain("no-config")
  })

  it("offer catalog edits exactly with the catalog_write bundle", () => {
    expect(mounted({ admin: true, catalog_write: true })).toContain(">edits<")
    expect(mounted({ admin: true, merchant_config: true })).toContain(
      "no-edits"
    )
    expect(mounted(null)).toContain("no-edits")
  })

  it("leave the settings tabs they own when not mounted", () => {
    expect(settingsTab(null, true)).toBe("merchant")
    expect(settingsTab("psps", true)).toBe("psps")
    expect(settingsTab(null, false)).toBe("customer-controls")
    for (const tab of ["merchant", "notifications", "psps"])
      expect(settingsTab(tab, false)).toBe("customer-controls")
    expect(settingsTab("customer-controls", false)).toBe("customer-controls")
  })

  it("show a host's tabs, and no tab nobody declares", () => {
    expect(settingsTab("team", true)).toBe("merchant")
    expect(settingsTab("team", true, ["team", "api-keys"])).toBe("team")
    expect(settingsTab("api-keys", false, ["team", "api-keys"])).toBe(
      "api-keys"
    )
  })
})
