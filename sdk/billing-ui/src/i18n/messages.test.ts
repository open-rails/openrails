import { describe, expect, it } from "vitest"

import {
  BillingError,
  readBillingError,
  toBillingError,
} from "../client/errors"
import { de } from "../locales/de"
import { en } from "../locales/en"
import { createTranslator, resolveMessages } from "./messages"

const m = createTranslator(resolveMessages())

describe("error messages", () => {
  it("names a server that couldn't be reached", () => {
    expect(m.error(new TypeError("Failed to fetch"))).toBe(
      "Couldn't reach the server. Check your connection and try again."
    )
    expect(m.error(toBillingError(new TypeError("Load failed")))).toBe(
      en.errors.network_error
    )
  })

  it("names a server that didn't answer", async () => {
    const gateway = await readBillingError(
      new Response("<html>Service Unavailable</html>", { status: 503 })
    )
    expect(m.error(gateway)).toBe(
      "The server isn't responding right now. Please try again in a moment."
    )
    expect(m.error(new DOMException("timed out", "TimeoutError"))).toBe(
      en.errors.server_unavailable
    )
    expect(createTranslator(resolveMessages([de])).error(gateway)).toBe(
      de.errors?.server_unavailable
    )
  })

  it("keeps coded API errors and the server's own message", () => {
    const coded = new BillingError(503, {
      type: "api_error",
      code: "service_unavailable",
      message: "billing paused",
    })
    expect(m.error(coded)).toBe(en.errors.service_unavailable)
    const unknown = new BillingError(400, {
      type: "invalid_request_error",
      code: "code_from_a_newer_openrails",
      message: "This coupon has expired.",
    })
    expect(m.error(unknown)).toBe("This coupon has expired.")
    expect(m.error(new TypeError("x is not a function"))).toBe(
      en.errors.generic
    )
    expect(
      m.error(
        new BillingError(500, {
          type: "api_error",
          code: "server_error",
          message: "HTTP 500",
        })
      )
    ).toBe(en.errors.generic)
  })
})
