import { describe, expect, it } from "vitest"
import { amountFromInput, formatUnits } from "@/lib/format"

// Canonical Go wire fixtures (testdata/wire, pinned by wire_fixtures_test.go).
const fixtures = import.meta.glob<string>(
  "../../../../../testdata/wire/*.json",
  {
    query: "?raw",
    import: "default",
    eager: true,
  }
)

function fixture(name: string): string {
  const entry = Object.entries(fixtures).find(([path]) =>
    path.endsWith(`/${name}`)
  )
  if (!entry) throw new Error(`missing wire fixture ${name}`)
  return entry[1]
}

const maxInt64 = (1n << 63n) - 1n
const minInt64 = -(1n << 63n)

describe("canonical wire fixtures in the browser", () => {
  it("contain only JSON numbers JavaScript represents exactly", () => {
    expect(Object.keys(fixtures).length).toBeGreaterThanOrEqual(6)
    for (const [path, raw] of Object.entries(fixtures)) {
      const numbers =
        raw.replace(/"(?:[^"\\]|\\.)*"/g, '""').match(/-?\d[\d.eE+-]*/g) ?? []
      for (const token of numbers) {
        expect(
          Number.isSafeInteger(Number(token)) &&
            String(Number(token)) === token,
          `${path}: ${token}`
        ).toBe(true)
      }
      expect(() => JSON.parse(raw)).not.toThrow()
    }
  })

  it("round-trip int64 boundary money without Number conversion", () => {
    const page = JSON.parse(fixture("page_credit_transactions.json"))
    expect(page.object).toBe("list")
    expect(BigInt(page.data[0].amount)).toBe(maxInt64)
    expect(BigInt(page.data[1].balance_after)).toBe(minInt64)
    expect(page.data[0].authorized).toBeNull()
    expect(page.data[0].created_at).toBe("2026-09-16T00:00:00.123456789Z")
    expect(amountFromInput("9223372036854.775807", 6)).toBe(page.data[0].amount)
    expect(formatUnits(page.data[1].amount, "JPY", 0).replace(/\D/g, "")).toBe(
      minInt64.toString().slice(1)
    )

    const settings = JSON.parse(fixture("merchant_settings.json"))
    expect(BigInt(settings.billing_policies[1].spend_windows[0].limit)).toBe(
      maxInt64
    )
    expect(settings.billing_policies[1].spend_windows[0].window_seconds).toBe(
      2592000
    )
  })

  it("keep error details and empty lists structurally stable", () => {
    const envelope = JSON.parse(fixture("error_envelope.json"))
    expect(envelope.error.code).toBe("idempotency_key_reused")
    expect(envelope.error.param).toBe("amount")
    expect(BigInt(envelope.error.metadata.committed_amount)).toBe(maxInt64)
    expect(envelope.error.metadata.detail).toBeNull()

    const empty = JSON.parse(fixture("page_empty.json"))
    expect(empty.data).toEqual([])
    expect(empty.has_more).toBe(false)
  })
})
