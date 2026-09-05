import { describe, expect, it } from "vitest"
import { renderToStaticMarkup } from "react-dom/server"
import { AutoTopupSafetySummary } from "./auto-topup-safety"

describe("auto-topup safety", () => {
  it("shows disabled, decline and rolling cap state without claiming unknown funds", () => {
    const html = renderToStaticMarkup(
      <AutoTopupSafetySummary
        status={{
          enabled: false,
          consecutive_declines: 3,
          daily: 3,
          weekly: 4,
          monthly: 8,
          pending: true,
          policy: {
            max_daily: 3,
            max_weekly: 10,
            max_monthly: 30,
            declines_before_disable: 3,
          },
        }}
      />
    )
    expect(html).toContain("Disabled")
    expect(html).toContain("3/3 in 24h")
    expect(html).toContain("4/10 in 7d")
    expect(html).toContain("8/30 in 30d")
    expect(html).toContain("pending verification")
    expect(html).toContain("Consecutive declines: 3/3")
  })
  it("omits unconfigured custom-currency safety", () => {
    expect(renderToStaticMarkup(<AutoTopupSafetySummary />)).toBe("")
  })
})
