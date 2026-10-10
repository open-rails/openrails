import { beforeEach, describe, expect, it, vi } from "vitest"

import type { AdminAccess } from "@/lib/api/generated/wire"
import {
  hasAnyArea,
  settingsTab,
  useAdminArea,
  useAdminUpdates,
  useCatalogArea,
  useCatalogWrites,
  useDashboardLayout,
  useMerchantConfig,
  useMerchantConfigEdits,
  useMetrics,
} from "@/lib/capabilities"
import { adminQueries } from "@/lib/queries"
import { client, render } from "@/test/harness"

// The access query key names the selected merchant, kept in sessionStorage.
beforeEach(() => {
  const values = new Map<string, string>()
  vi.stubGlobal("sessionStorage", {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => void values.set(key, value),
    removeItem: (key: string) => void values.delete(key),
  })
})

function Probe() {
  const flags: [string, boolean][] = [
    ["admin", useAdminArea()],
    ["admin-updates", useAdminUpdates()],
    ["catalog", useCatalogArea()],
    ["catalog-edits", useCatalogWrites()],
    ["config", useMerchantConfig()],
    ["metrics", useMetrics()],
    ["layout", useDashboardLayout()],
  ]
  return (
    <span>
      {flags
        .filter(([, on]) => on)
        .map(([name]) => name)
        .join(",")}
    </span>
  )
}

const holding = (access: AdminAccess | null) => {
  const queries = client()
  if (access) queries.setQueryData(adminQueries.access().queryKey, access)
  return render(<Probe />, queries)
}

function EditsProbe() {
  return <span>{useMerchantConfigEdits() ? "edits" : "read-only"}</span>
}

// configured mounts the public configuration with features.
const configured = (features: Record<string, boolean> | null) => {
  const queries = client()
  if (features)
    queries.setQueryData(adminQueries.config().queryKey, {
      capabilities: { route_groups: { merchant_config: true }, features },
      currencies: [],
      rails: [],
      payment: null,
      captcha: null,
    })
  return render(<EditsProbe />, queries)
}

const none: AdminAccess = {
  admin: "none",
  catalog: false,
  merchant_config: false,
  metrics: false,
}

describe("the console's areas", () => {
  it("follow what the caller holds", () => {
    expect(holding(null)).toContain("<span></span>")
    expect(holding(none)).toContain("<span></span>")
    expect(holding({ ...none, admin: "read" })).toContain(">admin<")
    expect(holding({ ...none, admin: "update" })).toContain(
      ">admin,admin-updates<"
    )
    expect(holding({ ...none, catalog: true })).toContain(
      ">catalog,catalog-edits<"
    )
    expect(holding({ ...none, metrics: true })).toContain(">metrics<")
    expect(
      holding({ ...none, merchant_config: true, metrics: true })
    ).toContain(">config,metrics,layout<")
  })

  it("show no access to a caller holding nothing", () => {
    expect(hasAnyArea(none)).toBe(false)
    expect(hasAnyArea({ ...none, metrics: true })).toBe(true)
  })

  it("offer configuration edits only where the configuration is editable", () => {
    expect(configured({ merchant_config_edits: true })).toContain(">edits<")
    expect(configured({ merchant_config_edits: false })).toContain("read-only")
    expect(configured({})).toContain("read-only")
    expect(configured(null)).toContain("read-only")
  })

  it("leave the settings tabs they own when not held", () => {
    expect(settingsTab(null, true, true)).toBe("merchant")
    expect(settingsTab("psps", true, false)).toBe("psps")
    expect(settingsTab(null, false, true)).toBe("customer-controls")
    for (const tab of ["merchant", "notifications", "psps"])
      expect(settingsTab(tab, false, true)).toBe("customer-controls")
    expect(settingsTab("customer-controls", true, false)).toBe("merchant")
    expect(settingsTab("team", true, true, ["team", "api-keys"])).toBe("team")
    expect(settingsTab("api-keys", false, false, ["team", "api-keys"])).toBe(
      "api-keys"
    )
    expect(settingsTab(null, false, false, ["team"])).toBe("team")
    expect(settingsTab("team", true, true)).toBe("merchant")
  })
})
