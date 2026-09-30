import { queryOptions } from "@tanstack/react-query"

import {
  api,
  ApiError,
  authApi,
  clearTokensIfCurrent,
  getTokens,
  loadBootstrap,
  sameTokenSession,
  setTokensIfCurrent,
  type BootstrapConfig,
} from "@/lib/api/client"
import type {
  AuthCapabilities,
  AuthResult,
  AuthTokens,
  Me,
  MerchantMembership,
  MerchantMembershipList,
  SecondFactorStep,
  TwoFactorFactor,
} from "@/lib/api/types"
import {
  AccountRecoveryRequired,
  takeAccountRecovery,
  type RecoveryState,
} from "@/lib/account-recovery"

export interface AuthIdentity {
  who: Me
  merchants: MerchantMembership[]
  activeMerchant?: MerchantMembership
}

// Everything /2fa/verify needs, carried between the two sign-in steps. The
// expected session is captured at the first step so a session that changes
// underneath us mid-sign-in is still caught.
export interface TwoFactorChallenge {
  challenge: string
  userID: string
  factor: TwoFactorFactor
  factors: TwoFactorFactor[]
  method: string
  verificationID?: string
  expectedSession: ReturnType<typeof getTokens>
}

// A browser sign-in that returned to the console still waiting on a step.
export interface PendingSignIn {
  challenge?: TwoFactorChallenge
  recovery?: RecoveryState
}

export interface AuthStateData {
  config: BootstrapConfig
  capabilities?: AuthCapabilities
  identity?: AuthIdentity
  pending?: PendingSignIn
}

export function challengeFrom(
  step: SecondFactorStep,
  expectedSession: ReturnType<typeof getTokens>
): TwoFactorChallenge {
  return {
    challenge: step.challenge,
    userID: step.user_id,
    factor: step.factor,
    factors: step.factors,
    method: step.factor.method,
    verificationID: step.factor.email ?? step.factor.phone_number ?? undefined,
    expectedSession,
  }
}

// signInStep reads an AuthResult, AuthKit's answer to every sign-in: the
// tokens of a finished one, or the second-factor step it waits on. Account
// recovery is AccountRecoveryRequired; steps the console cannot take throw.
export function signInStep(
  result: AuthResult,
  expectedSession: ReturnType<typeof getTokens>
): { tokens: AuthTokens } | { challenge: TwoFactorChallenge } {
  switch (result.status) {
    case "complete":
      if (result.token_set) return { tokens: result.token_set }
      break
    case "second_factor_required":
      if (result.second_factor)
        return {
          challenge: challengeFrom(result.second_factor, expectedSession),
        }
      break
    case "account_recovery_required":
      throw new AccountRecoveryRequired(result.recovery)
    case "verification_required":
      throw new Error(
        "This account requires verification before it can sign in."
      )
    case "enrollment_required":
      throw new Error(
        "This account must set up two-step verification before it can sign in here."
      )
  }
  throw new Error("Unexpected sign-in response")
}

export function sessionFrom(tokens: AuthTokens) {
  return {
    access_token: tokens.access_token,
    refresh_token: tokens.refresh_token ?? undefined,
    expires_at: tokens.expires_in
      ? Date.now() + tokens.expires_in * 1000
      : undefined,
  }
}

export const authStateQueryKey = ["auth", "state"] as const

function merchantMemberships(list: MerchantMembershipList) {
  return [...list.data].sort((a, b) => a.slug.localeCompare(b.slug))
}

export async function loadIdentity(
  expectedSession: NonNullable<ReturnType<typeof getTokens>>
): Promise<AuthIdentity> {
  const who = await authApi<Me>("/me")
  const memberships = await api<MerchantMembershipList>("/merchants")
  if (!sameTokenSession(expectedSession, getTokens())) {
    throw new Error("Your session changed while your account was loading")
  }

  const merchants = merchantMemberships(memberships)
  const storedSession = getTokens()
  if (!storedSession) {
    throw new Error("Your session ended while your account was loading")
  }
  const activeMerchant =
    merchants.find((merchant) => merchant.slug === storedSession.merchant) ??
    merchants[0]

  if (storedSession.merchant !== activeMerchant?.slug) {
    const updated = {
      ...storedSession,
      merchant: activeMerchant?.slug,
    }
    if (!setTokensIfCurrent(updated, storedSession)) {
      throw new Error("Your session changed while your account was loading")
    }
  }

  return { who, merchants, activeMerchant }
}

// AuthKit's browser OIDC returns to the console with a one-time code in the
// URL fragment (#code=); /oidc/exchange trades it for the sign-in's
// AuthResult. The fragment is cleared either way.
export function takeOIDCCode(): string | null {
  const params = new URLSearchParams(window.location.hash.slice(1))
  if (!params.has("code") && !params.has("error")) return null
  history.replaceState(
    null,
    "",
    window.location.pathname + window.location.search
  )
  return params.get("code")
}

async function exchangeOIDCCode(
  code: string
): Promise<PendingSignIn | undefined> {
  const expectedSession = getTokens()
  try {
    const step = signInStep(
      await authApi<AuthResult>("/oidc/exchange", {
        method: "POST",
        body: { code },
      }),
      expectedSession
    )
    if ("challenge" in step) return { challenge: step.challenge }
    setTokensIfCurrent(sessionFrom(step.tokens), expectedSession)
  } catch (error) {
    return {
      recovery: takeAccountRecovery(error) ?? {
        notice: error instanceof Error ? error.message : String(error),
      },
    }
  }
}

async function loadAuthState(): Promise<AuthStateData> {
  const config = await loadBootstrap()
  const code = takeOIDCCode()
  const pending = code ? await exchangeOIDCCode(code) : undefined

  let capabilities: AuthCapabilities | undefined
  try {
    capabilities = await authApi<AuthCapabilities>("/capabilities")
  } catch {
    // Capability discovery is optional; absence degrades to password-only.
  }

  let identity: AuthIdentity | undefined
  const session = getTokens()
  if (session) {
    try {
      identity = await loadIdentity(session)
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        clearTokensIfCurrent(session)
      }
    }
  }

  return { config, capabilities, identity, pending }
}

export const authStateQueryOptions = () =>
  queryOptions({
    queryKey: authStateQueryKey,
    queryFn: loadAuthState,
    staleTime: Infinity,
    gcTime: Infinity,
    retry: false,
  })
