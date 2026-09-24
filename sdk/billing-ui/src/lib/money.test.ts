import { describe, expect, it, vi } from "vitest"

import {
  addAmounts,
  amountToDecimal,
  amountUnits,
  formatAmount,
  intlFormatsDecimalStringsExactly,
  isAmount,
  isUnitDecimals,
} from "./money"

const maxInt64 = "9223372036854775807"
const minInt64 = "-9223372036854775808"

describe("amounts", () => {
  it("accept exactly the int64 decimal strings", () => {
    for (const value of ["0", "-1", "99000000", maxInt64, minInt64]) {
      expect(isAmount(value), value).toBe(true)
    }
    for (const value of [
      99_000_000,
      "9223372036854775808",
      "-9223372036854775809",
      "1.5",
      "1e6",
      "",
      " 1",
      "+1",
      null,
      undefined,
    ]) {
      expect(isAmount(value), String(value)).toBe(false)
    }
    expect(amountUnits(maxInt64)).toBe((1n << 63n) - 1n)
  })

  it("bound unit_decimals to the registry range", () => {
    expect(isUnitDecimals(0)).toBe(true)
    expect(isUnitDecimals(6)).toBe(true)
    expect(isUnitDecimals(18)).toBe(true)
    for (const value of [-1, 19, 1.5, "6", NaN, undefined]) {
      expect(isUnitDecimals(value), String(value)).toBe(false)
    }
  })

  it("sum without a JS number and refuse int64 overflow", () => {
    expect(addAmounts("9007199254740993", "1")).toBe("9007199254740994")
    expect(addAmounts(maxInt64, "-1", "1")).toBe(maxInt64)
    expect(addAmounts()).toBe("0")
    expect(addAmounts(maxInt64, "1")).toBeNull()
    expect(addAmounts("1", "x")).toBeNull()
  })

  it("render the exact major-unit decimal at the currency scale", () => {
    expect(amountToDecimal("99000000", 6)).toBe("99")
    expect(amountToDecimal("1234567", 6)).toBe("1.234567")
    expect(amountToDecimal("-500000", 6)).toBe("-0.5")
    expect(amountToDecimal("12345", 4)).toBe("1.2345")
    expect(amountToDecimal("7", 0)).toBe("7")
    expect(amountToDecimal(maxInt64, 6)).toBe("9223372036854.775807")
    expect(amountToDecimal("1", 19)).toBeNull()
    expect(amountToDecimal("1.0", 6)).toBeNull()
  })
})

describe("formatAmount", () => {
  it("formats at the registry scale, never through Number", () => {
    expect(formatAmount("99000000", "USD", 6)).toBe("$99.00")
    expect(formatAmount("1234567", "usd", 6)).toBe("$1.234567")
    expect(formatAmount("12340000", "JPY", 4)).toBe("¥1,234")
    expect(formatAmount("12345", "JPY", 4)).toBe("¥1.2345")
    expect(formatAmount(maxInt64, "USD", 6)).toBe("$9,223,372,036,854.775807")
    expect(formatAmount(minInt64, "EUR", 6)).toBe("-€9,223,372,036,854.775808")
  })

  it("refuses what it cannot show exactly", () => {
    expect(formatAmount("9223372036854775808", "USD", 6)).toBe(
      "USD amount exceeds the exact display range"
    )
    expect(formatAmount(null, "USD", 6)).toBe(
      "USD amount exceeds the exact display range"
    )
    expect(formatAmount("1", "USD", 19)).toBe(
      "USD amount exceeds the exact display range"
    )
  })

  it("falls back to a plain decimal for a currency Intl rejects", () => {
    expect(formatAmount("2500000", "USDC", 6)).toBe("2.5 USDC")
  })

  it("probes the engine's decimal-string support once", () => {
    expect(intlFormatsDecimalStringsExactly).toBe(true)
  })
})

describe("formatAmount on an engine that coerces decimal strings", () => {
  it("still renders every amount below 10^15 units and refuses above", async () => {
    vi.resetModules()
    const Original = Intl.NumberFormat
    class Coercing extends Original {
      format(value: unknown): string {
        return super.format(Number(value))
      }
    }
    vi.stubGlobal("Intl", { ...Intl, NumberFormat: Coercing })
    const legacy = await import("./money")
    expect(legacy.intlFormatsDecimalStringsExactly).toBe(false)
    expect(legacy.formatAmount("99000000", "USD", 6)).toBe("$99.00")
    expect(legacy.formatAmount("999999999999999", "USD", 6)).toBe(
      "$999,999,999.999999"
    )
    expect(legacy.formatAmount("1000000000000000", "USD", 6)).toBe(
      "USD amount exceeds this browser's exact display range"
    )
    expect(legacy.formatAmount(minInt64, "USD", 6)).toBe(
      "USD amount exceeds this browser's exact display range"
    )
  })
})
