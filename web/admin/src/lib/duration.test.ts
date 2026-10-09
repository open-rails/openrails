// Durations read and type as words; the wire keeps exact hours and seconds.
import { describe, expect, it } from "vitest"

import { formatCap, formatHours, formatSeconds, hoursToCap, parseHours } from "./duration"
import { priceIntervalLabel } from "@/pages/catalog/price-format"

describe("readable durations", () => {
  it.each([
    [720, "30 days"],
    [168, "1 week"],
    [8760, "365 days"],
    [36, "36 hours"],
    [1, "1 hour"],
  ])("shows %i hours as %s", (hours, shown) => {
    expect(formatHours(hours)).toBe(shown)
  })

  it.each([
    [300, "5 minutes"],
    [86_400, "1 day"],
    [90, "90 seconds"],
    [0, "0 seconds"],
  ])("shows %i seconds as %s", (seconds, shown) => {
    expect(formatSeconds(seconds)).toBe(shown)
  })

  it.each([
    ["30 days", 720],
    ["1 week", 168],
    ["12 hours", 12],
    ["3d", 72],
    [" 2 Weeks ", 336],
    ["0 days", null],
    ["1 month", null],
    ["1.5 days", null],
    ["720", null],
  ])("reads %j as %s hours", (text, hours) => {
    expect(parseHours(text)).toBe(hours)
  })

  it("round-trips an allowance cap through its wire spelling", () => {
    expect([formatCap("30d"), formatCap("720h"), formatCap("36h")]).toEqual(["30 days", "30 days", "36 hours"])
    expect([hoursToCap(720), hoursToCap(36)]).toEqual(["30d", "36h"])
    expect(formatCap("unknown")).toBe("unknown")
  })

  it("reads a price as a cadence or a stretch of access", () => {
    expect(priceIntervalLabel({ billing_interval_hours: 720, access_duration_hours: 720 })).toBe("every 30 days")
    expect(priceIntervalLabel({ billing_interval_hours: null, access_duration_hours: 168 })).toBe("1 week once")
  })
})
