import { describe, expect, it, vi } from "vitest"
import { billingConfirmWarnings, loadBillingConfirmWarnings } from "./summary"

const page = (statuses: string[], next_cursor: string | null = null) =>
  Response.json({ data: statuses.map((status) => ({ status })), next_cursor })

describe("loadBillingConfirmWarnings", () => {
  it("uses authenticated customer paging and stops at the first ongoing subscription", async () => {
    const authFetch = vi
      .fn()
      .mockResolvedValueOnce(page(["canceled"], "older"))
      .mockResolvedValueOnce(page(["past_due"], "oldest"))
    const warnings = await loadBillingConfirmWarnings({
      baseUrl: "https://billing.example/v1",
      auth: { authFetch },
      customer: "customer/one",
    })
    expect(warnings).toEqual(
      billingConfirmWarnings({ subscriptions: [{ status: "past_due" }] })
    )
    expect(authFetch).toHaveBeenCalledTimes(2)
    for (const [index, [input, init]] of authFetch.mock.calls.entries()) {
      const url = new URL(input)
      expect(url.pathname).toBe("/v1/admin/subscriptions")
      expect(url.searchParams.get("customer_id")).toBe("customer/one")
      expect(url.searchParams.get("limit")).toBe("100")
      expect(url.searchParams.get("cursor")).toBe(index === 0 ? null : "older")
      expect(init.method).toBe("GET")
    }
  })

  it("returns no warning only after a complete scan finds no ongoing subscription", async () => {
    const authFetch = vi.fn().mockResolvedValue(page(["canceled"]))
    await expect(
      loadBillingConfirmWarnings({
        baseUrl: "/billing/v1",
        auth: { authFetch },
        customer: "one",
      })
    ).resolves.toEqual([])
  })

  it.each([403, 500])("keeps an HTTP %i failure advisory", async (status) => {
    const authFetch = vi
      .fn()
      .mockResolvedValue(
        Response.json({ error: { message: "Unavailable" } }, { status })
      )
    const warnings = await loadBillingConfirmWarnings({
      baseUrl: "/billing/v1",
      auth: { authFetch },
      customer: "one",
    })
    expect(warnings).toEqual([
      expect.stringContaining("could not be fully checked"),
    ])
    expect(authFetch).toHaveBeenCalledTimes(1)
  })

  it("bounds an incomplete history and refuses a repeated cursor", async () => {
    const authFetch = vi
      .fn()
      .mockImplementation(async () =>
        page(["canceled"], String(authFetch.mock.calls.length))
      )
    const options = {
      baseUrl: "/billing/v1",
      auth: { authFetch },
      customer: "one",
    }
    expect(await loadBillingConfirmWarnings(options)).toEqual([
      expect.stringContaining("could not be fully checked"),
    ])
    expect(authFetch).toHaveBeenCalledTimes(10)
    authFetch
      .mockReset()
      .mockImplementation(async () => page(["canceled"], "same"))
    expect(await loadBillingConfirmWarnings(options)).toEqual([
      expect.stringContaining("could not be fully checked"),
    ])
    expect(authFetch).toHaveBeenCalledTimes(2)
  })

  it("propagates cancellation without calling billing", async () => {
    const authFetch = vi.fn()
    const controller = new AbortController()
    controller.abort()
    await expect(
      loadBillingConfirmWarnings({
        baseUrl: "/billing/v1",
        auth: { authFetch },
        customer: "one",
        signal: controller.signal,
      })
    ).rejects.toMatchObject({ name: "AbortError" })
    expect(authFetch).not.toHaveBeenCalled()
  })
})
