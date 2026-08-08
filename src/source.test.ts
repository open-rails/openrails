import { describe, expect, it, vi } from "vitest"

import { createHttpSource } from "./source"

describe("createHttpSource", () => {
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
