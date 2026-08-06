import { QueryClient } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  ApiError,
  authApi,
  clearTokensIfCurrent,
  getTokens,
  loadBootstrap,
  sameTokenSession,
  setTokens,
  setTokensIfCurrent,
} from "@/lib/api/client"
import { authStateQueryOptions, consumeOIDCFragment } from "@/lib/auth-state"

vi.mock("@/lib/api/client", () => ({
  ApiError: class ApiError extends Error {
    status: number

    constructor(status: number, _body: unknown, message: string) {
      super(message)
      this.status = status
    }
  },
  authApi: vi.fn(),
  clearTokensIfCurrent: vi.fn(),
  getTokens: vi.fn(),
  loadBootstrap: vi.fn(),
  sameTokenSession: vi.fn(),
  setTokens: vi.fn(),
  setTokensIfCurrent: vi.fn(),
}))

const config = {
  auth_base_url: "/auth",
  api_base_url: "/v1",
  nl_widgets_enabled: true,
  ask_enabled: true,
  catalog_copilot_enabled: true,
  catalog_drafting_enabled: false,
}

beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(loadBootstrap).mockResolvedValue(config)
  vi.mocked(sameTokenSession).mockReturnValue(true)
  vi.mocked(setTokensIfCurrent).mockReturnValue(true)
  vi.stubGlobal("window", {
    location: { hash: "", pathname: "/admin", search: "" },
  })
  vi.stubGlobal("history", { replaceState: vi.fn() })
})

afterEach(() => vi.unstubAllGlobals())

describe("auth state query", () => {
  it("loads capabilities and the selected merchant identity", async () => {
    const queryClient = new QueryClient()
    const session = { access_token: "token" }
    vi.mocked(getTokens).mockReturnValue(session)
    vi.mocked(authApi).mockImplementation(async (path) => {
      if (path === "/capabilities") return { password: { login: true } }
      if (path === "/me") {
        return { id: "user-1", email: "alice@example.com" }
      }
      return {
        data: [
          {
            persona: "merchant",
            instance_slug: "merchant-b",
            instance_name: "Merchant B",
          },
          {
            persona: "merchant",
            instance_slug: "merchant-a",
            instance_name: "Merchant A",
          },
          {
            persona: "customer",
            instance_slug: "ignored",
            instance_name: "Ignored",
          },
        ],
      }
    })

    const state = await queryClient.fetchQuery(authStateQueryOptions())

    expect(state.config).toEqual(config)
    expect(state.capabilities).toEqual({ password: { login: true } })
    expect(state.identity?.who).toEqual({
      id: "user-1",
      email: "alice@example.com",
    })
    expect(
      state.identity?.merchants.map((merchant) => merchant.instance_slug)
    ).toEqual(["merchant-a", "merchant-b"])
    expect(state.identity?.activeMerchant?.instance_slug).toBe("merchant-a")
    expect(setTokensIfCurrent).toHaveBeenCalledWith(
      { access_token: "token", merchant: "merchant-a" },
      session
    )
  })

  it("degrades cleanly when optional capabilities are unavailable", async () => {
    const queryClient = new QueryClient()
    vi.mocked(getTokens).mockReturnValue(null)
    vi.mocked(authApi).mockRejectedValueOnce(new Error("not supported"))

    const state = await queryClient.fetchQuery(authStateQueryOptions())

    expect(state).toEqual({
      config,
      capabilities: undefined,
      identity: undefined,
    })
  })

  it("clears an unchanged session rejected by the identity endpoint", async () => {
    const queryClient = new QueryClient()
    const session = { access_token: "expired" }
    vi.mocked(getTokens).mockReturnValue(session)
    vi.mocked(authApi)
      .mockResolvedValueOnce({ password: { login: true } })
      .mockRejectedValueOnce(new ApiError(401, null, "unauthorized"))

    const state = await queryClient.fetchQuery(authStateQueryOptions())

    expect(state.identity).toBeUndefined()
    expect(clearTokensIfCurrent).toHaveBeenCalledWith(session)
  })
})

describe("OIDC fragment consumption", () => {
  it("stores callback tokens and removes the fragment from the URL", () => {
    vi.stubGlobal("window", {
      location: {
        hash: "#access_token=access&refresh_token=refresh&expires_in=60&merchant=merchant-a",
        pathname: "/admin",
        search: "?next=%2F",
      },
    })

    expect(consumeOIDCFragment()).toBe(true)
    expect(setTokens).toHaveBeenCalledWith({
      access_token: "access",
      refresh_token: "refresh",
      expires_at: expect.any(Number),
      merchant: "merchant-a",
    })
    expect(history.replaceState).toHaveBeenCalledWith(
      null,
      "",
      "/admin?next=%2F"
    )
  })
})
