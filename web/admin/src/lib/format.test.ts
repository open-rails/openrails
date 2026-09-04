import { describe, expect, it } from "vitest"
import { formatUnits, microsFromInput, unitsFromInput } from "./format"

describe("server-scaled monetary amounts", () => {
  it("uses the supplied JPY/custom scale rather than assuming micros", () => {
    expect(unitsFromInput("1.2345", 4)).toBe(12345)
    expect(unitsFromInput("1.23456", 4)).toBeNull()
    expect(unitsFromInput("12", 0)).toBe(12)
    expect(unitsFromInput("12.1", 0)).toBeNull()
    expect(unitsFromInput("0.000000000000000001", 18)).toBe(1)
    expect(microsFromInput("1.234567")).toBe(1234567)
  })
  it("never rounds beyond the safe integer input boundary", () => {
    expect(unitsFromInput("9007199254.740991", 6)).toBe(Number.MAX_SAFE_INTEGER)
    expect(unitsFromInput("9007199254.740992", 6)).toBeNull()
    expect(unitsFromInput("1e2", 6)).toBeNull()
    expect(unitsFromInput("", 6)).toBeNull()
    expect(unitsFromInput("1", -1)).toBeNull()
  })
  it("formats exact signed native units", () => {
    expect(formatUnits(12345, "JPY", 4)).toBe("1.2345 JPY")
    expect(formatUnits(-1, "USD", 6)).toBe("-0.000001 USD")
    expect(formatUnits(12, "shop/points", 0)).toBe("12 shop/points")
    expect(formatUnits(Number.MAX_SAFE_INTEGER + 1, "USD", 6)).toContain(
      "exceeds the exact display range"
    )
  })
})
