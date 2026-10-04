import { describe, expect, it } from "vitest"

import {
  cardBrandOf,
  cardEntryDisplay,
  cardEntryErrors,
  formatCardNumber,
  formatExpiry,
  parseCardEntry,
} from "./card-entry"

// Gateway test cards only.
const now = new Date(2026, 9, 3)

describe("native card entry", () => {
  it("formats as the card is printed", () => {
    expect(formatCardNumber("4111111111111111")).toBe("4111 1111 1111 1111")
    expect(formatCardNumber("3782 8224 6310 005")).toBe("3782 822463 10005")
    expect(formatCardNumber("41a1")).toBe("411")
    expect(formatExpiry("1027")).toBe("10 / 27")
    expect(formatExpiry("1")).toBe("1")
  })

  it("names the network from the leading digits", () => {
    expect(
      ["4111", "5431", "2221", "3782", "6011", "3530", ""].map(cardBrandOf)
    ).toEqual([
      "visa",
      "mastercard",
      "mastercard",
      "amex",
      "discover",
      "jcb",
      "",
    ])
  })

  it("accepts a complete card and hands over exactly the wire fields", () => {
    const card = parseCardEntry(
      { number: "4111 1111 1111 1111", expiry: "10 / 27", cvc: "999" },
      now
    )
    expect(card).toEqual({
      number: "4111111111111111",
      exp_month: 10,
      exp_year: 2027,
      cvc: "999",
    })
    expect(cardEntryDisplay(card!)).toEqual({
      last_four: "1111",
      card_type: "visa",
      expiry_date: "10/27",
    })
  })

  it("names each field it refuses", () => {
    const valid = { number: "4111111111111111", expiry: "10/27", cvc: "999" }
    expect(cardEntryErrors(valid, now)).toEqual({})
    for (const [input, field] of [
      [{ ...valid, number: "4111111111111112" }, "number"],
      [{ ...valid, number: "4111" }, "number"],
      [{ ...valid, expiry: "13/27" }, "expiry"],
      [{ ...valid, expiry: "09/26" }, "expiry"],
      [{ ...valid, expiry: "1" }, "expiry"],
      [{ ...valid, cvc: "99" }, "cvv"],
      [{ ...valid, cvc: "9a9" }, "cvv"],
      [{ number: "378282246310005", expiry: "10/27", cvc: "999" }, "cvv"],
    ] as const) {
      expect(Object.keys(cardEntryErrors(input, now))).toEqual([field])
      expect(parseCardEntry(input, now)).toBeNull()
    }
    expect(
      cardEntryErrors(
        { number: "378282246310005", expiry: "10/27", cvc: "9999" },
        now
      )
    ).toEqual({})
    expect(cardEntryErrors({ ...valid, expiry: "10/2026" }, now)).toEqual({})
  })
})
