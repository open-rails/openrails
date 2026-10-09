import { describe, expect, it } from "vitest"

import { settingsTab, useMerchantConfig } from "@/lib/capabilities"
import { adminQueries } from "@/lib/queries"
import { client, render } from "@/test/harness"

function Probe() {
  return <span>{useMerchantConfig() ? "config" : "no-config"}</span>
}

const mounted = (groups: Record<string, boolean> | null) => {
  const queries = client()
  if (groups)
    queries.setQueryData(adminQueries.config().queryKey, {
      capabilities: { route_groups: groups, features: {} },
      currencies: [],
      payment: null,
    })
  return render(<Probe />, queries)
}

describe("the merchant's configuration pages", () => {
  it("follow the capability route group", () => {
    expect(mounted({ merchant: true, merchant_config: true })).toContain(
      ">config<"
    )
    expect(mounted({ merchant: true, merchant_config: false })).toContain(
      "no-config"
    )
    expect(mounted({ merchant: true })).toContain("no-config")
    expect(mounted(null)).toContain("no-config")
  })

  it("leave the settings tabs they own when not mounted", () => {
    expect(settingsTab(null, true)).toBe("merchant")
    expect(settingsTab("psps", true)).toBe("psps")
    expect(settingsTab(null, false)).toBe("team")
    for (const tab of ["merchant", "notifications", "psps"])
      expect(settingsTab(tab, false)).toBe("team")
    expect(settingsTab("customer-controls", false)).toBe("customer-controls")
  })
})
