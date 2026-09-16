import { describe, expect, it } from "vitest"
import currencyUnits from "./currency-units.json"
import {
  amountFromInput,
  currencyScale,
  formatNativeAmount,
  formatUnits,
  nativeAmountFromInput,
  nativeAmountToInput,
  unitsFromInput,
  unitsToDecimal,
} from "./format"

const INT64_MAX = "9223372036854775807"
const INT64_MIN = "-9223372036854775808"
const digits = (value: string) => value.replace(/\D/g, "")

describe("server-scaled monetary amounts", () => {
  it("uses the supplied JPY/custom scale rather than assuming micros", () => {
    expect(unitsFromInput("1.2345", 4)).toBe(12345)
    expect(unitsFromInput("1.23456", 4)).toBeNull()
    expect(unitsFromInput("12", 0)).toBe(12)
    expect(unitsFromInput("12.1", 0)).toBeNull()
    expect(unitsFromInput("0.000000000000000001", 18)).toBe(1)
    expect(nativeAmountFromInput("1.234567", "USD")).toBe(1234567)
  })
  it("never rounds beyond the safe integer input boundary", () => {
    expect(unitsFromInput("9007199254.740991", 6)).toBe(Number.MAX_SAFE_INTEGER)
    expect(unitsFromInput("9007199254.740992", 6)).toBeNull()
    expect(unitsFromInput("1e2", 6)).toBeNull()
    expect(unitsFromInput("", 6)).toBeNull()
    expect(unitsFromInput("1", -1)).toBeNull()
    expect(amountFromInput("9223372036854.775807", 6)).toBe(INT64_MAX)
    expect(amountFromInput("9223372036854.775808", 6)).toBeNull()
  })
  it("formats exact signed native units at nonuniform scales", () => {
    expect(formatUnits(12345, "JPY", 4)).toBe("¥1.2345")
    expect(formatUnits(-1, "USD", 6)).toBe("-$0.000001")
    expect(formatUnits(12, "shop/points", 0)).toBe("12 shop/points")
    expect(formatUnits(12345, "KWD", 3)).toBe("KWD\u00a012.345")
    expect(digits(formatUnits("1", "shop/points", 18))).toBe(
      "0000000000000000001"
    )
    expect(formatUnits(Number.MAX_SAFE_INTEGER + 1, "USD", 6)).toContain(
      "exceeds the exact display range"
    )
    expect(formatUnits("12.5", "USD", 6)).toContain("exceeds")
    expect(formatUnits("9223372036854775808", "USD", 6)).toContain("exceeds")
  })
  it("keeps exact int64 strings out of JS number rounding", () => {
    expect(formatUnits(INT64_MAX, "USD", 6)).toBe("$9,223,372,036,854.775807")
    expect(formatUnits(INT64_MIN, "JPY", 4)).toBe("-¥922,337,203,685,477.5808")
    expect(unitsToDecimal(INT64_MIN, 0)).toBe(INT64_MIN)
    expect(unitsToDecimal("-5", 4)).toBe("-0.0005")
  })
})

describe("engine currency registry", () => {
  it("scales each registered currency from the generated table", () => {
    expect(currencyScale(" jpy ")).toBe(4)
    expect(currencyScale("constructor")).toBeUndefined()
    expect(nativeAmountFromInput("500", "JPY")).toBe(5_000_000)
    expect(nativeAmountFromInput("500", "USD")).toBe(500_000_000)
    expect(nativeAmountFromInput("1", "UNKNOWN")).toBeNull()
    expect(formatNativeAmount(5_000_000, "JPY")).toBe("¥500")
    expect(formatNativeAmount(20_000_000, "usd")).toBe("$20.00")
    expect(formatNativeAmount(1, "XXX")).toContain("unregistered currency")
  })
  it.each(Object.entries(currencyUnits))(
    "%s round-trips every int64 edge exactly at scale %i",
    (currency, scale) => {
      for (const units of [
        "0",
        "1",
        "-1",
        String(10n ** BigInt(scale)),
        String(Number.MAX_SAFE_INTEGER),
        INT64_MAX,
        INT64_MIN,
      ]) {
        const input = nativeAmountToInput(units, currency)
        expect(amountFromInput(input, scale)).toBe(units)
        // Display keeps every input digit; Intl may only pad minor-unit zeros.
        const shown = digits(formatNativeAmount(units, currency))
        expect(shown.slice(0, digits(input).length)).toBe(digits(input))
        expect(shown.slice(digits(input).length)).toMatch(/^0*$/)
      }
      expect(
        nativeAmountFromInput(
          nativeAmountToInput(Number.MAX_SAFE_INTEGER, currency),
          currency
        )
      ).toBe(Number.MAX_SAFE_INTEGER)
      // An already-rounded JSON number stays visible but cannot be saved.
      const rounded = nativeAmountToInput(2 ** 60, currency)
      expect(rounded).not.toBe("")
      expect(nativeAmountFromInput(rounded, currency)).toBeNull()
    }
  )
})
