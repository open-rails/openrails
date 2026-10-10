import { describe, expect, it } from "vitest"

import { fixtureSession } from "./fixtures"
import canonical from "../../../testdata/wire/checkout_session.json"
import { checkoutSessionSchema, payResultSchema } from "./types"

// OpenRails' canonical checkout session fixture
// (testdata/wire/checkout_session.json, pinned by wire_fixtures_test.go).
describe("checkoutSessionSchema", () => {
  it("decodes the canonical OpenRails fixture with int64 boundary money", () => {
    const parsed = checkoutSessionSchema.parse(canonical)
    expect(parsed.plan.unit_amount).toBe("9223372036854775807")
    expect(parsed.plan.currency).toBe("USD")
    expect(parsed.plan.unit_decimals).toBe(6)
    expect(parsed.line_items?.[1].amount).toBe("-9223372036854775808")
    expect(parsed.tax).toBe("0")
    expect(parsed.due_today).toBe("9223372036854775807")
    expect(parsed.options.map((option) => option.driver)).toEqual([
      "collect_js",
      "solana_pay",
      "stripe_elements",
    ])
    expect(parsed.options[2].psp_id).toBe(
      "psp_77777777-7777-4777-8777-777777777777"
    )
    expect(parsed.next_action?.type).toBe("solana_pay")
    expect(parsed.operation?.status).toBe("pending")
    expect(parsed.line_items?.[1].sublabel).toBeUndefined()
    expect(parsed.saved_methods?.[0].card?.exp_year).toBe(2030)
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

  it("takes a redirect only to an https page, a wallet link only as solana:", () => {
    for (const next_action of [
      { type: "redirect_to_url", url: "javascript:alert(1)" },
      { type: "redirect_to_url", url: "http://pay.example/checkout" },
      { type: "solana_pay", url: "https://pay.example/solana-pay" },
    ]) {
      expect(
        payResultSchema.safeParse({ status: "requires_action", next_action })
          .success,
        JSON.stringify(next_action)
      ).toBe(false)
    }
    expect(
      payResultSchema.safeParse({
        status: "requires_action",
        next_action: { type: "redirect_to_url", url: "https://pay.example/x" },
        operation: null,
      }).success
    ).toBe(true)
  })
})
