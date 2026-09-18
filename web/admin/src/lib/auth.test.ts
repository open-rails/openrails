// Sign-in state: what the console believes about the operator and which
// merchant its requests are made as.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { getTokens, setTokens } from "@/lib/api/client"
import { twoFactorVerificationBody, type TwoFactorChallenge } from "@/lib/auth"
import { authStateQueryOptions, consumeOIDCFragment } from "@/lib/auth-state"
import { client, server, type Reply } from "@/test/harness"

const membership = (slug: string, persona = "merchant") =>
  ({ persona, instance_slug: slug, instance_name: slug })
const who = { id: "user-1", email: "alice@example.test" }

let routes: Record<string, Reply>
beforeEach(async () => {
  routes = {
    "/capabilities": { password: { login: true } },
    "/me": who,
    "/me/groups": {
      data: [membership("merchant-b"), membership("merchant-a"), membership("ignored", "customer")],
    },
  }
  await server(routes)
  vi.stubGlobal("window", { location: { hash: "", pathname: "/admin", search: "" } })
  vi.stubGlobal("history", { replaceState: vi.fn() })
})
afterEach(() => vi.unstubAllGlobals())

const load = () => client().fetchQuery(authStateQueryOptions())

describe("auth state", () => {
  it("lists only merchant memberships and selects one for every request", async () => {
    setTokens({ access_token: "token" })

    const state = await load()

    expect(state.capabilities).toEqual({ password: { login: true } })
    expect(state.identity?.who).toEqual(who)
    expect(state.identity?.merchants.map((m) => m.instance_slug)).toEqual(["merchant-a", "merchant-b"])
    expect(state.identity?.activeMerchant?.instance_slug).toBe("merchant-a")
    // The selection is written back, so cache keys and headers agree with it.
    expect(getTokens()?.merchant).toBe("merchant-a")
  })

  it("degrades to password-only when capability discovery is unavailable", async () => {
    routes["/capabilities"] = () => new Response(null, { status: 404 })

    const state = await load()

    expect(state.capabilities).toBeUndefined()
    expect(state.identity).toBeUndefined()
  })

  it("clears the session the identity endpoint rejects, and only that one", async () => {
    const expired = { access_token: "expired" }
    setTokens(expired)
    routes["/me"] = () =>
      Response.json({ error: { message: "unauthorized" } }, { status: 401 })

    expect((await load()).identity).toBeUndefined()
    expect(getTokens()).toBeNull()

    setTokens(expired)
    routes["/me"] = () =>
      Response.json({ error: { message: "unavailable" } }, { status: 503 })

    expect((await load()).identity).toBeUndefined()
    expect(getTokens()).toEqual(expired)
  })

  it("never attaches an identity to a session that changed while it loaded", async () => {
    setTokens({ access_token: "first" })
    routes["/me/groups"] = () => {
      setTokens({ access_token: "second", merchant: "merchant-b" })
      return { data: [membership("merchant-a")] }
    }

    expect((await load()).identity).toBeUndefined()
    expect(getTokens()).toEqual({ access_token: "second", merchant: "merchant-b" })
  })

  it("stores callback tokens from the OIDC fragment and clears the URL", () => {
    vi.stubGlobal("window", {
      location: {
        hash: "#access_token=access&refresh_token=refresh&expires_in=60&merchant=merchant-a",
        pathname: "/admin", search: "?next=%2F",
      },
    })
    expect(consumeOIDCFragment()).toBe(true)
    expect(getTokens()).toEqual({
      access_token: "access", refresh_token: "refresh",
      expires_at: expect.any(Number), merchant: "merchant-a",
    })
    expect(history.replaceState).toHaveBeenCalledWith(null, "", "/admin?next=%2F")
  })
})

it("targets the selected second factor, and sends a recovery code without one", () => {
  const challenge: TwoFactorChallenge = {
    challenge: "challenge-token", userID: "user-1", method: "totp",
    factor: { id: "factor-1", method: "totp" },
    factors: [{ id: "factor-1", method: "totp" }], expectedSession: null,
  }
  expect(twoFactorVerificationBody(challenge, " 123456 ", "factor")).toEqual({
    user_id: "user-1", challenge: "challenge-token", factor_id: "factor-1", code: "123456",
  })
  expect(twoFactorVerificationBody(challenge, " recovery-code ", "backup_code")).toEqual({
    user_id: "user-1", challenge: "challenge-token", backup_code: true, code: "recovery-code",
  })
})
