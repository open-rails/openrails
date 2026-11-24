import { localError, readBillingError } from "./errors"

/** A token endpoint's answer (RFC 6749 §5.1). */
export interface TokenResponse {
  access_token: string
  /** Seconds until the token expires. */
  expires_in: number
}

export interface TokenSourceOptions {
  /**
   * Asks the host for the customer's access token: one bound to the
   * merchant, from the host's own backend or `redeemAssertion`.
   */
  fetchToken: () => Promise<TokenResponse>
  /** Seconds before its expiry at which a token is replaced. Default 60. */
  skew?: number
}

/**
 * A `getToken` for `createBillingClient`: the current token, fetched when
 * there is none or the cached one is about to expire.
 */
export interface TokenSource {
  (): Promise<string>
  /**
   * Drops the cached token, so the next call fetches a new one. With
   * `token`, only when that is the one cached.
   */
  invalidate(token?: string): void
}

/**
 * Keeps the customer's access token for a billing client: it caches the
 * token until `skew` seconds before it expires and fetches the next one with
 * a single request, however many calls are waiting. A failed fetch is not
 * cached. Tokens stay in memory, never in browser storage, so a reload
 * fetches again. The client retries a call answered 401 `credential_expired`
 * once with a new token.
 */
export function tokenSource({
  fetchToken,
  skew = 60,
}: TokenSourceOptions): TokenSource {
  let cached: { token: string; staleAt: number } | undefined
  let pending: Promise<string> | undefined

  const source = (): Promise<string> => {
    if (cached && Date.now() < cached.staleAt)
      return Promise.resolve(cached.token)
    pending ??= fetchToken()
      .then((answer) => {
        if (
          typeof answer?.access_token !== "string" ||
          !answer.access_token ||
          !Number.isFinite(answer.expires_in)
        )
          throw localError(
            "invalid_response",
            "fetchToken answered no access_token and expires_in"
          )
        cached = {
          token: answer.access_token,
          staleAt: Date.now() + (answer.expires_in - skew) * 1000,
        }
        return answer.access_token
      })
      .finally(() => {
        pending = undefined
      })
    return pending
  }
  source.invalidate = (token?: string) => {
    if (token === undefined || cached?.token === token) cached = undefined
  }
  return source
}

/** Whether `getToken` is a `tokenSource`, whose token the client can replace. */
export const isTokenSource = (getToken: unknown): getToken is TokenSource =>
  typeof getToken === "function" &&
  typeof (getToken as Partial<TokenSource>).invalidate === "function"

export interface RedeemAssertionOptions {
  /** The authorization server's token endpoint, such as `https://billing.example/oauth2/token`. */
  tokenEndpoint: string
  /**
   * The assertion the merchant's backend signed for this customer (RFC
   * 7523): single use and minutes long, so redeem each one once.
   */
  assertion: string
  /**
   * Transport. Pass a fetch that adds a DPoP proof (auth-ui's `dpopFetch`)
   * to bind the token to the browser's key.
   */
  fetch?: (input: string, init: RequestInit) => Promise<Response>
}

/**
 * Redeems a customer assertion for an access token to OpenRails
 * (`scope=openrails:self`): a `fetchToken` for `tokenSource`, after the
 * host's backend signed the assertion.
 */
export async function redeemAssertion(
  options: RedeemAssertionOptions
): Promise<TokenResponse & { token_type: string }> {
  const doFetch =
    options.fetch ?? ((input: string, init: RequestInit) => fetch(input, init))
  const res = await doFetch(options.tokenEndpoint, {
    method: "POST",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/x-www-form-urlencoded",
    },
    body: new URLSearchParams({
      grant_type: "urn:ietf:params:oauth:grant-type:jwt-bearer",
      assertion: options.assertion,
      scope: "openrails:self",
    }).toString(),
  })
  if (!res.ok) throw await readBillingError(res)
  const answer = (await res.json().catch(() => null)) as Record<
    string,
    unknown
  > | null
  if (
    typeof answer?.access_token !== "string" ||
    typeof answer.expires_in !== "number"
  )
    throw localError(
      "invalid_response",
      "the token endpoint answered no access_token and expires_in"
    )
  return {
    access_token: answer.access_token,
    expires_in: answer.expires_in,
    token_type:
      typeof answer.token_type === "string" ? answer.token_type : "Bearer",
  }
}
