import { describe, expect, it, vi } from "vitest"

import { fixtureSession } from "./fixtures"
import { createHttpSource } from "./source"

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  })
}

describe("createHttpSource", () => {
  it("reads an exact-money session and refuses a numeric one", async () => {
    const exact = fixtureSession({
      plan: {
        display_name: "Annual",
        unit_amount: "9007199254740993",
        currency: "JPY",
        unit_decimals: 4,
        automatically_renews: false,
      },
    })
    const source = createHttpSource({
      baseUrl: "https://checkout.example.test/",
      sessionId: "ocs_test",
      fetch: vi.fn(async (input: RequestInfo | URL) => {
        expect(String(input)).toBe(
          "https://checkout.example.test/api/v1/checkout/sessions/ocs_test"
        )
        return jsonResponse(exact)
      }),
    })
    const session = await source.getSession()
    expect(session.plan.unit_amount).toBe("9007199254740993")
    expect(session.plan.unit_decimals).toBe(4)

    const numeric = createHttpSource({
      baseUrl: "https://checkout.example.test",
      sessionId: "ocs_test",
      fetch: vi.fn(async () =>
        jsonResponse({
          ...exact,
          plan: { ...exact.plan, unit_amount: 99_000_000 },
        })
      ),
    })
    await expect(numeric.getSession()).rejects.toThrow("session unavailable")
  })

  it("rejects a non-HTTPS payment redirect", async () => {
    const source = createHttpSource({
      baseUrl: "https://checkout.example.test",
      sessionId: "ocs_test",
      fetch: vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              status: "requires_action",
              redirect_url: "javascript:alert(1)",
            }),
            { status: 200, headers: { "Content-Type": "application/json" } }
          )
      ),
    })

    await expect(source.pay({ option_id: "option_stripe" })).rejects.toThrow(
      "Payment failed. Try again."
    )
  })
})
