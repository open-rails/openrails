import { QueryClient } from "@tanstack/react-query"
import { describe, expect, it, vi } from "vitest"

import { authMutations } from "@/lib/auth-mutations"

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
})
