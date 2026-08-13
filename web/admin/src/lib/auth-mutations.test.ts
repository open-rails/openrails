import { QueryClient } from "@tanstack/react-query"
import { describe, expect, it, vi } from "vitest"

import { authMutations } from "@/lib/auth-mutations"
import type { TwoFactorChallenge } from "@/lib/auth"

const challenge: TwoFactorChallenge = {
  challenge: "challenge-token",
  userID: "user-1",
  factor: { id: "factor-1", method: "totp" },
  factors: [
    { id: "factor-1", method: "totp" },
    { id: "factor-2", method: "sms" },
  ],
  method: "totp",
  expectedSession: null,
}

describe("auth mutations", () => {
  it("submits password credentials through the auth provider", async () => {
    const queryClient = new QueryClient()
    const loginWithPassword = vi.fn().mockResolvedValue(undefined)

    await queryClient
      .getMutationCache()
      .build(queryClient, authMutations.login(loginWithPassword))
      .execute({ login: "alice@example.com", password: "secret" })

    expect(loginWithPassword).toHaveBeenCalledWith(
      "alice@example.com",
      "secret"
    )
  })

  it("runs the auth provider logout", async () => {
    const queryClient = new QueryClient()
    const logout = vi.fn().mockResolvedValue(undefined)

    await queryClient
      .getMutationCache()
      .build(queryClient, authMutations.logout(logout))
      .execute()

    expect(logout).toHaveBeenCalledOnce()
  })

  it("forwards the selected verification mode", async () => {
    const queryClient = new QueryClient()
    const completeTwoFactor = vi.fn().mockResolvedValue(undefined)

    await queryClient
      .getMutationCache()
      .build(
        queryClient,
        authMutations.verifyTwoFactor(completeTwoFactor)
      )
      .execute({ challenge, code: "backup-code", mode: "backup_code" })

    expect(completeTwoFactor).toHaveBeenCalledWith(
      challenge,
      "backup-code",
      "backup_code"
    )
  })

  it("requests a challenge for an alternate factor", async () => {
    const queryClient = new QueryClient()
    const selectTwoFactor = vi.fn().mockResolvedValue({
      ...challenge,
      factor: challenge.factors[1],
      method: "sms",
    })

    const result = await queryClient
      .getMutationCache()
      .build(queryClient, authMutations.selectTwoFactor(selectTwoFactor))
      .execute({ challenge, factorId: "factor-2" })

    expect(selectTwoFactor).toHaveBeenCalledWith(challenge, "factor-2")
    expect(result.factor.id).toBe("factor-2")
  })
})
