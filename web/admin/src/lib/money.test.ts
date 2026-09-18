// Every console path that parses, submits or displays money. Amounts are
// int64 minor units on the wire: a Number cannot hold them, so nothing here
// may round-trip through one.
import { afterEach, describe, expect, it, vi } from "vitest"

import currencyUnits from "./currency-units.json"
import {
  amountFromInput,
  currencyScale,
  formatNativeAmount,
  formatUnits,
  nativeAmountFromInput,
  nativeAmountToInput,
  unitsToDecimal,
} from "./format"
import { canRevokeCredit, creditGrantInput } from "@/pages/customers/credit-form"
import { invoicePaymentAmount } from "@/pages/invoices/model"
import { durationLabel, priceIntervalLabel } from "@/pages/catalog/price-format"
import type { CreditGrant } from "@/lib/api/credit-types"

const MAX = "9223372036854775807"
const MIN = "-9223372036854775808"
const UNSAFE = "9007199254740993" // 2^53 + 1: a Number cannot hold it
const digits = (value: string) => value.replace(/\D/g, "")

describe("parsing an entered amount at the server's scale", () => {
  it.each([
    ["1.2345", 4, "12345"],
    ["1.23456", 4, null],
    ["12", 0, "12"],
    ["12.1", 0, null],
    ["0.000000000000000001", 18, "1"],
    ["9007199254.740993", 6, UNSAFE],
    ["9223372036854.775807", 6, MAX],
    ["9223372036854.775808", 6, null], // one unit past int64
    ["1e2", 6, null],
    ["", 6, null],
    ["1", -1, null],
  ])("reads %s at scale %i as %s", (input, scale, units) => {
    expect(amountFromInput(input, scale)).toBe(units)
  })

  it("takes the scale of a registered currency, and refuses unknown ones", () => {
    expect(currencyScale(" jpy ")).toBe(4)
    expect(currencyScale("constructor")).toBeUndefined()
    expect(nativeAmountFromInput("500", "JPY")).toBe("5000000")
    expect(nativeAmountFromInput("500", "USD")).toBe("500000000")
    expect(nativeAmountFromInput("1", "UNKNOWN")).toBeNull()
    expect(nativeAmountFromInput("1.234567", "USD")).toBe("1234567")
  })
})

describe("displaying an exact amount", () => {
  it.each([
    [12345, "JPY", 4, "¥1.2345"],
    [-1, "USD", 6, "-$0.000001"],
    [12, "shop/points", 0, "12 shop/points"],
    [12345, "KWD", 3, "KWD 12.345"],
    [MAX, "USD", 6, "$9,223,372,036,854.775807"],
    [MIN, "JPY", 4, "-¥922,337,203,685,477.5808"],
  ])("formats %s %s at scale %i as %s", (units, currency, scale, shown) => {
    expect(formatUnits(units, currency, scale)).toBe(shown)
  })

  it.each([
    [Number.MAX_SAFE_INTEGER + 1, "USD", 6],
    ["12.5", "USD", 6],
    ["9223372036854775808", "USD", 6],
  ])("refuses to display %s %s rather than round it", (units, currency, scale) => {
    expect(formatUnits(units, currency, scale)).toContain(
      "exceeds the exact display range"
    )
  })

  it("keeps every digit of an amount no Number could hold", () => {
    expect(digits(formatUnits("1", "shop/points", 18))).toBe(
      "0000000000000000001"
    )
    expect(unitsToDecimal(MIN, 0)).toBe(MIN)
    expect(unitsToDecimal("-5", 4)).toBe("-0.0005")
    expect(formatNativeAmount(5_000_000, "JPY")).toBe("¥500")
    expect(formatNativeAmount(20_000_000, "usd")).toBe("$20.00")
    expect(formatNativeAmount(1, "XXX")).toContain("unregistered currency")
  })

  it("never prefills a rounded amount from an unsafe JSON number", () => {
    // The prefill must refuse an inexact Number rather than hand
    // nativeAmountFromInput the rounded digits, which it would accept.
    for (const unsafe of [
      Number(UNSAFE),
      -Number(UNSAFE),
      Number(MAX),
      1e21,
      1.5,
      Number.NaN,
    ]) {
      expect(nativeAmountToInput(unsafe, "USD")).toBe("")
      expect(
        nativeAmountFromInput(nativeAmountToInput(unsafe, "USD"), "USD")
      ).toBeNull()
    }
    expect(nativeAmountToInput(Number.MAX_SAFE_INTEGER, "USD")).toBe(
      "9007199254.740991"
    )
    // The exact string form of the same value prefills and round-trips.
    expect(
      nativeAmountFromInput(nativeAmountToInput(UNSAFE, "USD"), "USD")
    ).toBe(UNSAFE)
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
        MAX,
        MIN,
      ]) {
        const input = nativeAmountToInput(units, currency)
        expect(amountFromInput(input, scale)).toBe(units)
        // Display keeps every input digit; Intl may only pad minor-unit zeros.
        const shown = digits(formatNativeAmount(units, currency))
        expect(shown.slice(0, digits(input).length)).toBe(digits(input))
        expect(shown.slice(digits(input).length)).toMatch(/^0*$/)
      }
    }
  )
})

// A pre-2023 engine (Chrome < 106, Firefox < 116, Safari < 15.4) coerces a
// decimal-string argument through Number before formatting.
describe("engines without exact decimal-string Intl formatting", () => {
  const RealNumberFormat = Intl.NumberFormat
  class LegacyNumberFormat {
    private readonly real: Intl.NumberFormat
    constructor(locales?: string | string[], options?: Intl.NumberFormatOptions) {
      this.real = new RealNumberFormat(locales, options)
    }
    format(value: number | bigint | string): string {
      return this.real.format(Number(value))
    }
  }
  const legacyFormat = async () => {
    vi.stubGlobal("Intl", { ...Intl, NumberFormat: LegacyNumberFormat })
    vi.resetModules()
    return import("./format")
  }
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.resetModules()
  })

  it("is detected, and still formats everything a Number holds exactly", async () => {
    expect((await import("./format")).intlFormatsDecimalStringsExactly).toBe(true)
    const legacy = await legacyFormat()
    expect(legacy.intlFormatsDecimalStringsExactly).toBe(false)
    expect(legacy.formatUnits("999999999999999", "USD", 6)).toBe(
      "$999,999,999.999999"
    )
    expect(legacy.formatUnits("-999999999999999", "JPY", 4)).toBe(
      "-¥99,999,999,999.9999"
    )
    expect(legacy.formatNativeAmount("1234567", "USD")).toBe("$1.234567")
  })

  it("refuses rather than misformats amounts beyond that range", async () => {
    const legacy = await legacyFormat()
    // The hazard the guard exists for: coercion silently changes the digits.
    expect(
      new LegacyNumberFormat("en", { useGrouping: false }).format(UNSAFE)
    ).toBe("9007199254740992")
    for (const [units, currency, scale] of [
      [UNSAFE, "shop/points", 0],
      [UNSAFE, "USD", 6],
      ["1000000000000000", "USD", 6],
      ["-1000000000000000", "JPY", 4],
    ] as const)
      expect(legacy.formatUnits(units, currency, scale)).toContain(
        "this browser's exact display range"
      )
    expect(legacy.formatNativeAmount(MAX, "USD")).toContain(
      "this browser's exact display range"
    )
    // Not an int64 at all: the range refusal is unchanged.
    expect(legacy.formatUnits("9223372036854775808", "USD", 6)).toBe(
      "USD amount exceeds the exact display range"
    )
  })
})

describe("money submitted by console forms", () => {
  const credit = {
    amount: "1.000001",
    decimals: 6,
    currency: "USD",
    expires: "",
    description: " support ",
    sourceID: "stable",
  }

  it("grants credit at the server unit scale, keeping the caller's key", () => {
    expect(creditGrantInput(credit, true)).toEqual({
      amount: "1000001",
      currency: "USD",
      source: "admin",
      source_id: "stable",
      description: "support",
      expires_at: undefined,
    })
    const amount = (over: Partial<typeof credit>) =>
      creditGrantInput({ ...credit, ...over }, true).amount
    expect(amount({ amount: "9223372036854.775807" })).toBe(MAX)
    expect(amount({ amount: ".25" })).toBe("250000")
    expect(amount({ amount: "1.2345", currency: "JPY", decimals: 4 })).toBe(
      "12345"
    )
    expect(amount({ amount: "12", currency: "shop/points", decimals: 0 })).toBe(
      "12"
    )
    expect(() =>
      amount({ amount: "1.23456", currency: "JPY", decimals: 4 })
    ).toThrow("4 decimal places")
  })

  it.each([
    "",
    "0",
    "-1",
    "NaN",
    "Infinity",
    "1e3",
    "1.0000001",
    "9223372036854.775808",
  ])("refuses the invalid or unrepresentable grant amount %s", (amount) => {
    expect(() => creditGrantInput({ ...credit, amount }, true)).toThrow(
      "positive amount"
    )
  })

  it("requires granting authority and a real future expiry", () => {
    expect(() => creditGrantInput(credit, false)).toThrow("cannot grant")
    expect(
      creditGrantInput({ ...credit, expires: "2099-01-02T03:04" }, true)
        .expires_at
    ).toBe(new Date("2099-01-02T03:04").toISOString())
    for (const expires of ["bad", "2020-01-01T12:00"])
      expect(() => creditGrantInput({ ...credit, expires }, true)).toThrow(
        "future date"
      )
  })

  it("revokes only a grant that still holds value, with authority", () => {
    const grant = {
      id: "grant-a",
      state: "active",
      remaining_amount: "70",
    } as CreditGrant
    expect(canRevokeCredit(grant, false)).toBe(false)
    for (const state of ["expired", "revoked", "spent", "terminated"] as const)
      expect(canRevokeCredit({ ...grant, state }, true)).toBe(false)
    expect(canRevokeCredit({ ...grant, remaining_amount: "0" }, true)).toBe(false)
    expect(canRevokeCredit({ ...grant, state: "scheduled" }, true)).toBe(true)
  })

  it("remits invoices at the invoice's own scale, never beyond the balance", () => {
    expect(invoicePaymentAmount("12", "120000", 4)).toBe("120000")
    expect(invoicePaymentAmount("2", "120000", 4)).toBe("20000")
    expect(invoicePaymentAmount("9007199254.740993", MAX, 6)).toBe(UNSAFE)
    expect(invoicePaymentAmount("9223372036854.775807", MAX, 6)).toBe(MAX)
    for (const amount of ["0.00001", "13", "0", "9223372036854.775808", UNSAFE])
      expect(() => invoicePaymentAmount(amount, "120000", 4)).toThrow()
  })

  it("reads a price as a cadence or a stretch of access", () => {
    expect([durationLabel(720), durationLabel(168), durationLabel(36)]).toEqual([
      "1 month",
      "1 week",
      "36 hours",
    ])
    expect(
      priceIntervalLabel({ auto_renew: true, access_duration_hours: 744 })
    ).toBe("every 31 days")
    expect(
      priceIntervalLabel({ auto_renew: false, access_duration_hours: 48 })
    ).toBe("2 days once")
    expect(
      priceIntervalLabel({ auto_renew: false, access_duration_hours: undefined })
    ).toBe("one-time")
  })
})

// The canonical Go fixtures (testdata/wire, byte-pinned by
// TestCanonicalWireFixtures) must survive a real browser JSON parse: Go's
// assertJavaScriptSafe only approximates this engine.
describe("canonical wire fixtures in the browser", () => {
  const fixtures = import.meta.glob<string>("../../../../testdata/wire/*.json", {
    query: "?raw",
    import: "default",
    eager: true,
  })
  const fixture = (name: string) =>
    JSON.parse(
      Object.entries(fixtures).find(([path]) => path.endsWith(`/${name}`))![1]
    )

  it("contains only JSON numbers JavaScript represents exactly", () => {
    expect(Object.keys(fixtures).length).toBeGreaterThanOrEqual(6)
    for (const [path, raw] of Object.entries(fixtures))
      for (const token of raw
        .replace(/"(?:[^"\\]|\\.)*"/g, '""')
        .match(/-?\d[\d.eE+-]*/g) ?? [])
        expect(
          Number.isSafeInteger(Number(token)) && String(Number(token)) === token,
          `${path}: ${token}`
        ).toBe(true)
  })

  it("round-trips int64 boundary money through the console's own parsing", () => {
    const page = fixture("page_credit_transactions.json")
    expect([page.data[0].amount, page.data[1].balance_after]).toEqual([MAX, MIN])
    expect(amountFromInput("9223372036854.775807", 6)).toBe(page.data[0].amount)
    expect(digits(formatUnits(page.data[1].amount, "JPY", 0))).toBe(MIN.slice(1))
    expect(page.data[0].created_at).toBe("2026-09-16T00:00:00.123456789Z")
    expect(
      fixture("merchant_settings.json").billing_policies[1].spend_windows[0]
        .limit
    ).toBe(MAX)
    expect(fixture("error_envelope.json").error.metadata.committed_amount).toBe(
      MAX
    )
    expect(
      digits(formatUnits(fixture("subscription.json").price.unit_amount, "USD", 6))
    ).toBe(digits(MAX))
  })
})
