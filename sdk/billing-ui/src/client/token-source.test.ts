import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { json } from "../test/billing-server"
import { redeemAssertion, tokenSource } from "./token-source"

describe("tokenSource", () => {
  beforeEach(() => {
    vi.useFakeTimers({ now: new Date("2026-10-11T00:00:00Z") })
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  // host hands out t1, t2, ... each good for `lifetime` seconds.
  const host = (lifetime = 900) => {
    let n = 0
    return vi.fn(async () => ({
      access_token: `t${++n}`,
      expires_in: lifetime,
    }))
  }

  it("caches a token until skew seconds before it expires", async () => {
    const fetchToken = host(900)
    const getToken = tokenSource({ fetchToken })
    expect(await getToken()).toBe("t1")
    vi.advanceTimersByTime(839_000)
    expect(await getToken()).toBe("t1")
    expect(fetchToken).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(1_000)
    expect(await getToken()).toBe("t2")
    expect(fetchToken).toHaveBeenCalledTimes(2)
  })

  it("takes the skew it is given", async () => {
    const fetchToken = host(900)
    const getToken = tokenSource({ fetchToken, skew: 300 })
    await getToken()
    vi.advanceTimersByTime(599_000)
    expect(await getToken()).toBe("t1")
    vi.advanceTimersByTime(1_000)
    expect(await getToken()).toBe("t2")
  })

  it("makes one fetch for ten concurrent calls", async () => {
    let answer!: (token: { access_token: string; expires_in: number }) => void
    const fetchToken = vi.fn(
      () =>
        new Promise<{ access_token: string; expires_in: number }>(
          (resolve) => (answer = resolve)
        )
    )
    const getToken = tokenSource({ fetchToken })
    const calls = Array.from({ length: 10 }, () => getToken())
    answer({ access_token: "t1", expires_in: 900 })
    expect(await Promise.all(calls)).toEqual(Array(10).fill("t1"))
    expect(fetchToken).toHaveBeenCalledTimes(1)
  })

  it("does not cache a rejected fetch", async () => {
    const fetchToken = vi
      .fn<() => Promise<{ access_token: string; expires_in: number }>>()
      .mockRejectedValueOnce(new Error("the host is down"))
      .mockResolvedValueOnce({ access_token: "t1", expires_in: 900 })
    const getToken = tokenSource({ fetchToken })
    await expect(Promise.all([getToken(), getToken()])).rejects.toThrow(
      "the host is down"
    )
    expect(fetchToken).toHaveBeenCalledTimes(1)
    expect(await getToken()).toBe("t1")
    expect(fetchToken).toHaveBeenCalledTimes(2)
  })

  it("refuses an answer that is no token", async () => {
    const getToken = tokenSource({
      fetchToken: async () => ({ access_token: "", expires_in: 900 }),
    })
    await expect(getToken()).rejects.toMatchObject({ code: "invalid_response" })
  })

  it("drops the cached token when invalidated, but not a newer one", async () => {
    const fetchToken = host()
    const getToken = tokenSource({ fetchToken })
    expect(await getToken()).toBe("t1")
    getToken.invalidate("t1")
    expect(await getToken()).toBe("t2")
    // A second caller that also failed with t1 leaves t2 alone.
    getToken.invalidate("t1")
    expect(await getToken()).toBe("t2")
    getToken.invalidate()
    expect(await getToken()).toBe("t3")
    expect(fetchToken).toHaveBeenCalledTimes(3)
  })

  it("writes nothing to browser storage", async () => {
    const writes = [
      vi.spyOn(Storage.prototype, "setItem"),
      vi.spyOn(Storage.prototype, "removeItem"),
      vi.spyOn(Storage.prototype, "getItem"),
    ]
    const getToken = tokenSource({ fetchToken: host() })
    await getToken()
    getToken.invalidate()
    await getToken()
    for (const spy of writes) expect(spy).not.toHaveBeenCalled()
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })
})

describe("redeemAssertion", () => {
  it("posts the JWT-bearer grant with the fetch it is given", async () => {
    const fetch = vi
      .fn<(input: string, init: RequestInit) => Promise<Response>>()
      .mockResolvedValue(
        json(200, { access_token: "at", token_type: "DPoP", expires_in: 900 })
      )
    const token = await redeemAssertion({
      tokenEndpoint: "https://billing.example/oauth2/token",
      assertion: "signed.by.backend",
      fetch,
    })
    expect(token).toEqual({
      access_token: "at",
      token_type: "DPoP",
      expires_in: 900,
    })
    const [url, init] = fetch.mock.calls[0]!
    expect(url).toBe("https://billing.example/oauth2/token")
    expect(init.method).toBe("POST")
    expect(new Headers(init.headers).get("Content-Type")).toBe(
      "application/x-www-form-urlencoded"
    )
    expect(Object.fromEntries(new URLSearchParams(String(init.body)))).toEqual({
      grant_type: "urn:ietf:params:oauth:grant-type:jwt-bearer",
      assertion: "signed.by.backend",
      scope: "openrails:self",
    })
  })

  it("answers the token endpoint's refusal and a body that is no token", async () => {
    const refused = async () =>
      json(400, {
        error: "invalid_grant",
        error_description: "assertion was used",
      })
    await expect(
      redeemAssertion({
        tokenEndpoint: "/oauth2/token",
        assertion: "a",
        fetch: refused,
      })
    ).rejects.toMatchObject({ status: 400, code: "invalid_grant" })
    const empty = async () => json(200, { token_type: "Bearer" })
    await expect(
      redeemAssertion({
        tokenEndpoint: "/oauth2/token",
        assertion: "a",
        fetch: empty,
      })
    ).rejects.toMatchObject({ code: "invalid_response" })
  })

  it("feeds a token source", async () => {
    const fetch = vi.fn(async () =>
      json(200, { access_token: "at", token_type: "Bearer", expires_in: 900 })
    )
    const getToken = tokenSource({
      fetchToken: () =>
        redeemAssertion({
          tokenEndpoint: "/oauth2/token",
          assertion: "a",
          fetch,
        }),
    })
    expect(await Promise.all([getToken(), getToken()])).toEqual(["at", "at"])
    expect(fetch).toHaveBeenCalledTimes(1)
  })
})
