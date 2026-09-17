import { describe, expect, it } from "vitest"

import { fixtureSession } from "./fixtures"
import canonical from "./test/fixtures/hosted_checkout_session.json"
import { checkoutSessionSchema } from "./types"

// OpenRails' canonical hosted checkout fixture (testdata/wire/
// hosted_checkout_session.json, pinned there by wire_fixtures_test.go).
describe("checkoutSessionSchema", () => {
  it("decodes the canonical OpenRails fixture with int64 boundary money", () => {
    const parsed = checkoutSessionSchema.parse(canonical)
    expect(parsed.plan.unit_amount).toBe("9223372036854775807")
    expect(parsed.plan.currency).toBe("USD")
    expect(parsed.plan.unit_decimals).toBe(6)
    expect(parsed.line_items?.[1].amount).toBe("-9223372036854775808")
    expect(parsed.tax).toBe("0")
    expect(parsed.due_today).toBe("9223372036854775807")
    expect(parsed.rails.map((rail) => rail.driver)).toEqual([
      "collect_js",
      "solana_pay",
      "redirect",
    ])
    expect(parsed.saved_methods?.[0].exp_year).toBe(2030)
    expect(parsed.expires_at).toBe("2026-09-16T00:00:00.123456789Z")
  })

  it("refuses numeric money and a missing or invalid scale", () => {
    const session = fixtureSession()
    for (const plan of [
      { ...session.plan, unit_amount: 99_000_000 },
      { ...session.plan, unit_amount: "99000000.00" },
      { ...session.plan, unit_amount: "9223372036854775808" },
      { ...session.plan, unit_decimals: undefined },
      { ...session.plan, unit_decimals: "6" },
      { ...session.plan, unit_decimals: 19 },
      { ...session.plan, unit_decimals: 2.5 },
    ]) {
      expect(
        checkoutSessionSchema.safeParse({ ...session, plan }).success,
        JSON.stringify(plan)
      ).toBe(false)
    }
    for (const override of [
      { tax: 0 },
      { due_today: 5 },
      { line_items: [{ label: "Item", amount: 1 }] },
    ]) {
      expect(
        checkoutSessionSchema.safeParse({ ...session, ...override }).success,
        JSON.stringify(override)
      ).toBe(false)
    }
    expect(checkoutSessionSchema.safeParse(session).success).toBe(true)
  })
})
