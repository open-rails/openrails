import { describe, expect, it } from "vitest"

import { durationLabel, priceIntervalLabel } from "./price-format"

describe("durationLabel", () => {
  it("names whole months, weeks and days", () => {
    expect(durationLabel(720)).toBe("1 month")
    expect(durationLabel(1440)).toBe("2 months")
    expect(durationLabel(168)).toBe("1 week")
    expect(durationLabel(744)).toBe("31 days")
    expect(durationLabel(24)).toBe("1 day")
  })

  it("keeps hours when nothing divides evenly", () => {
    expect(durationLabel(1)).toBe("1 hour")
    expect(durationLabel(36)).toBe("36 hours")
  })
})

describe("priceIntervalLabel", () => {
  it("reads a renewing price as a cadence", () => {
    expect(
      priceIntervalLabel({ auto_renew: true, access_duration_hours: 744 })
    ).toBe("every 31 days")
  })

  it("reads a one-off with access as a stretch of time", () => {
    expect(
      priceIntervalLabel({ auto_renew: false, access_duration_hours: 48 })
    ).toBe("2 days once")
  })

  it("reads a price with no duration as one-time", () => {
    expect(
      priceIntervalLabel({ auto_renew: false, access_duration_hours: undefined })
    ).toBe("one-time")
  })
})
