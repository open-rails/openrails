import { describe, expect, it } from "vitest"

import {
  twoFactorVerificationBody,
  type TwoFactorChallenge,
} from "@/lib/auth"

const challenge: TwoFactorChallenge = {
  challenge: "challenge-token",
  userID: "user-1",
  factor: { id: "factor-1", method: "totp" },
  factors: [{ id: "factor-1", method: "totp" }],
  method: "totp",
  expectedSession: null,
}

describe("two-factor verification body", () => {
  it("targets the selected factor for a verification code", () => {
    expect(twoFactorVerificationBody(challenge, " 123456 ", "factor")).toEqual(
      {
        user_id: "user-1",
        challenge: "challenge-token",
        factor_id: "factor-1",
        code: "123456",
      }
    )
  })

  it("marks a recovery code without sending a factor id", () => {
    expect(
      twoFactorVerificationBody(challenge, " recovery-code ", "backup_code")
    ).toEqual({
      user_id: "user-1",
      challenge: "challenge-token",
      backup_code: true,
      code: "recovery-code",
    })
  })
})
