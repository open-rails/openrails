import { afterEach, describe, expect, it, vi } from "vitest"

// A pre-2023 engine (Chrome < 106, Firefox < 116, Safari < 15.4) coerces a
// decimal-string argument through Number before formatting.
class LegacyNumberFormat {
  private readonly real: Intl.NumberFormat
  constructor(locales?: string | string[], options?: Intl.NumberFormatOptions) {
    this.real = new RealNumberFormat(locales, options)
  }
  format(value: number | bigint | string): string {
    return this.real.format(Number(value))
  }
}
const RealNumberFormat = Intl.NumberFormat

async function loadWithLegacyIntl() {
  vi.stubGlobal("Intl", { ...Intl, NumberFormat: LegacyNumberFormat })
  vi.resetModules()
  return import("./format")
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.resetModules()
})

describe("engines without exact decimal-string Intl formatting", () => {
  it("is detected on this engine as supported", async () => {
    const exact = await import("./format")
    expect(exact.intlFormatsDecimalStringsExactly).toBe(true)
    expect(exact.formatUnits("9223372036854775807", "USD", 6)).toBe(
      "$9,223,372,036,854.775807"
    )
  })

  it("still format every amount a Number holds exactly", async () => {
    const legacy = await loadWithLegacyIntl()
    expect(legacy.intlFormatsDecimalStringsExactly).toBe(false)
    expect(legacy.formatUnits("999999999999999", "USD", 6)).toBe(
      "$999,999,999.999999"
    )
    expect(legacy.formatUnits("-999999999999999", "JPY", 4)).toBe(
      "-¥99,999,999,999.9999"
    )
    expect(legacy.formatUnits(123456789012345, "shop/points", 0)).toBe(
      "123,456,789,012,345 shop/points"
    )
    expect(legacy.formatNativeAmount("1234567", "USD")).toBe("$1.234567")
  })

  it("refuse rather than misformat amounts beyond that range", async () => {
    const legacy = await loadWithLegacyIntl()
    // The hazard the guard exists for: coercion silently changes the digits.
    expect(
      new LegacyNumberFormat("en", { useGrouping: false }).format(
        "9007199254740993"
      )
    ).toBe("9007199254740992")
    expect(legacy.formatUnits("9007199254740993", "shop/points", 0)).toBe(
      "shop/points amount exceeds this browser's exact display range"
    )
    expect(legacy.formatUnits("9007199254740993", "USD", 6)).toBe(
      "USD amount exceeds this browser's exact display range"
    )
    expect(legacy.formatUnits("1000000000000000", "USD", 6)).toContain(
      "this browser's exact display range"
    )
    expect(legacy.formatUnits("-1000000000000000", "JPY", 4)).toContain(
      "this browser's exact display range"
    )
    expect(legacy.formatNativeAmount("9223372036854775807", "USD")).toContain(
      "this browser's exact display range"
    )
    // Not an int64 at all: the range refusal is unchanged.
    expect(legacy.formatUnits("9223372036854775808", "USD", 6)).toBe(
      "USD amount exceeds the exact display range"
    )
  })
})
