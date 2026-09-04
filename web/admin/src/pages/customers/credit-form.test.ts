import { describe, expect, it } from "vitest"
import { microsFromInput } from "@/lib/format"
import type { CreditGrant } from "@/lib/api/credit-types"
import {
  canRevokeCredit,
  creditGrantInput,
  creditRevokeInput,
} from "./credit-form"

const input = {
  amount: "1.000001",
  decimals: 6,
  currency: "USD",
  expires: "",
  description: " support ",
  sourceID: "stable",
}
const grant = {
  id: "grant-a",
  state: "active",
  remaining_amount: 70,
} as CreditGrant

describe("credit grant form", () => {
  it("uses the server unit scale for JPY and custom credits", () => {
    expect(
      creditGrantInput(
        { ...input, amount: "1.2345", currency: "JPY", decimals: 4 },
        true
      ).amount
    ).toBe(12345)
    expect(
      creditGrantInput(
        { ...input, amount: "12", currency: "shop/points", decimals: 0 },
        true
      ).amount
    ).toBe(12)
    expect(() =>
      creditGrantInput(
        { ...input, amount: "1.23456", currency: "JPY", decimals: 4 },
        true
      )
    ).toThrow("4 decimal places")
  })
  it("preserves decimal precision and the caller's idempotency key", () => {
    expect(creditGrantInput(input, true)).toEqual({
      amount: 1000001,
      currency: "USD",
      source: "admin",
      source_id: "stable",
      description: "support",
      expires_at: undefined,
    })
    expect(microsFromInput("9007199254.740991")).toBe(Number.MAX_SAFE_INTEGER)
    expect(microsFromInput(".25")).toBe(250000)
    expect(microsFromInput("-0.25")).toBe(-250000)
  })
  it.each([
    "",
    "0",
    "-1",
    "NaN",
    "Infinity",
    "1e3",
    "1.0000001",
    "9007199254.740992",
  ])("rejects invalid or unrepresentable amount %s", (amount) => {
    expect(() => creditGrantInput({ ...input, amount }, true)).toThrow(
      "positive amount"
    )
  })
  it("requires grant permission", () => {
    expect(() => creditGrantInput(input, false)).toThrow("cannot grant")
  })
  it("rejects an expired or malformed expiry", () => {
    expect(() => creditGrantInput({ ...input, expires: "bad" }, true)).toThrow(
      "future date"
    )
    expect(() =>
      creditGrantInput({ ...input, expires: "2020-01-01T12:00" }, true)
    ).toThrow("future date")
  })
})

describe("credit revoke form", () => {
  it("requires explicit authority and a remaining active grant", () => {
    expect(canRevokeCredit(grant, false)).toBe(false)
    for (const state of ["expired", "revoked", "spent", "terminated"] as const)
      expect(canRevokeCredit({ ...grant, state }, true)).toBe(false)
    expect(canRevokeCredit({ ...grant, remaining_amount: 0 }, true)).toBe(false)
    expect(canRevokeCredit({ ...grant, state: "scheduled" }, true)).toBe(true)
  })
  it("requires a reason and preserves the selected grant", () => {
    expect(() => creditRevokeInput(grant, true, " ")).toThrow("reason")
    expect(() => creditRevokeInput(grant, false, "correction")).toThrow(
      "current access"
    )
    expect(creditRevokeInput(grant, true, " correction ")).toEqual({
      grant: "grant-a",
      reason: "correction",
    })
  })
})
