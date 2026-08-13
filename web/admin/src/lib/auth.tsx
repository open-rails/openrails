/* eslint-disable react-refresh/only-export-components */
// AuthKit-backed auth for the console. The issuer's authhttp surface (base
// from /admin/config.json) provides capabilities discovery, password login,
// refresh, logout, /me, and — when the deployment mounts browser OIDC — the
// {provider}/login redirect flow whose callback lands back here with tokens
// in the URL fragment.
import * as React from "react"
import { useQuery } from "@tanstack/react-query"

import {
  authApi,
  getBootstrap,
  getTokens,
  logoutSession,
  setTokens,
  setTokensIfCurrent,
  setUnauthorizedHandler,
  type BootstrapConfig,
} from "@/lib/api/client"
import type {
  AuthCapabilities,
  AuthTokens,
  Me,
  MerchantMembership,
  PasswordLoginResponse,
  TwoFactorChallengeResponse,
  TwoFactorFactor,
} from "@/lib/api/types"
import {
  authStateQueryKey,
  authStateQueryOptions,
  loadIdentity,
  type AuthStateData,
} from "@/lib/auth-state"
import { queryClient } from "@/lib/query-client"

interface AuthState {
  ready: boolean
  bootError?: string
  config?: BootstrapConfig
  capabilities?: AuthCapabilities
  me: Me | null
  merchants: MerchantMembership[]
  activeMerchant?: MerchantMembership
  selectMerchant: (slug: string) => void
  /** Resolves to a challenge when the account needs a second factor, else null. */
  loginWithPassword: (
    login: string,
    password: string
  ) => Promise<TwoFactorChallenge | null>
  completeTwoFactor: (
    challenge: TwoFactorChallenge,
    code: string,
    mode: TwoFactorVerificationMode
  ) => Promise<void>
  selectTwoFactor: (
    challenge: TwoFactorChallenge,
    factorId: string
  ) => Promise<TwoFactorChallenge>
  startOIDC: (providerId: string) => void
  logout: () => Promise<void>
}

// Everything /2fa/verify needs, carried between the two sign-in steps. The
// expected session is captured at the password step so a session that changes
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

export type TwoFactorVerificationMode = "factor" | "backup_code"

export function twoFactorVerificationBody(
  challenge: TwoFactorChallenge,
  code: string,
  mode: TwoFactorVerificationMode
) {
  return {
    user_id: challenge.userID,
    challenge: challenge.challenge,
    code: code.trim(),
    ...(mode === "backup_code"
      ? { backup_code: true }
      : { factor_id: challenge.factor.id }),
  }
}

const AuthContext = React.createContext<AuthState | undefined>(undefined)
const EMPTY_MERCHANTS: MerchantMembership[] = []

const clearMerchantQueries = () =>
  queryClient.removeQueries({ queryKey: ["merchant"] })

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const authState = useQuery(authStateQueryOptions())
  const ready = !authState.isPending
  const bootError = authState.error
    ? authState.error instanceof Error
      ? authState.error.message
      : String(authState.error)
    : undefined
  const config = authState.data?.config
  const capabilities = authState.data?.capabilities
  const me = authState.data?.identity?.who ?? null
  const merchants = authState.data?.identity?.merchants ?? EMPTY_MERCHANTS
  const activeMerchant = authState.data?.identity?.activeMerchant

  React.useEffect(() => {
    setUnauthorizedHandler(() => {
      clearMerchantQueries()
      queryClient.setQueryData<AuthStateData>(authStateQueryKey, (current) =>
        current ? { ...current, identity: undefined } : current
      )
    })
    return () => setUnauthorizedHandler(null)
  }, [])

  // Both sign-in paths end the same way: store the pair, load the identity,
  // drop any merchant data belonging to whoever was signed in before.
  const completeSession = React.useCallback(
    async (res: AuthTokens, expectedSession: ReturnType<typeof getTokens>) => {
      const session = {
        access_token: res.access_token,
        refresh_token: res.refresh_token,
        expires_at: res.expires_in
          ? Date.now() + res.expires_in * 1000
          : undefined,
      }
      if (!setTokensIfCurrent(session, expectedSession)) {
        throw new Error("Your session changed while sign-in was completing")
      }
      const identity = await loadIdentity(session)
      clearMerchantQueries()
      queryClient.setQueryData<AuthStateData>(authStateQueryKey, (current) =>
        current ? { ...current, identity } : current
      )
    },
    []
  )

  const loginWithPassword = React.useCallback(
    async (
      login: string,
      password: string
    ): Promise<TwoFactorChallenge | null> => {
      const expectedSession = getTokens()
      const res = await authApi<PasswordLoginResponse>("/password/login", {
        method: "POST",
        body: { login, password },
      })
      // A 2FA account gets no tokens here. Hand the challenge back so the caller
      // can ask for a code, rather than treating a normal policy as a failure.
      if ("requires_2fa" in res && res.requires_2fa) {
        return {
          challenge: res.challenge,
          userID: res.user_id,
          factor: res.default_factor,
          factors: res.available_factors,
          method: res.method,
          verificationID: res.verification_id,
          expectedSession,
        }
      }
      if ("requires_verification" in res && res.requires_verification) {
        throw new Error(
          "This account requires verification before it can sign in."
        )
      }
      await completeSession(res, expectedSession)
      return null
    },
    [completeSession]
  )

  const completeTwoFactor = React.useCallback(
    async (
      challenge: TwoFactorChallenge,
      code: string,
      mode: TwoFactorVerificationMode
    ) => {
      const res = await authApi<AuthTokens>("/2fa/verify", {
        method: "POST",
        body: twoFactorVerificationBody(challenge, code, mode),
      })
      await completeSession(res, challenge.expectedSession)
    },
    [completeSession]
  )

  const selectTwoFactor = React.useCallback(
    async (challenge: TwoFactorChallenge, factorId: string) => {
      const res = await authApi<TwoFactorChallengeResponse>("/2fa/challenge", {
        method: "POST",
        body: {
          user_id: challenge.userID,
          challenge: challenge.challenge,
          factor_id: factorId,
        },
      })
      return {
        ...challenge,
        factor: res.factor,
        method: res.method,
        verificationID: res.verification_id,
      }
    },
    []
  )

  const selectMerchant = React.useCallback(
    (slug: string) => {
      const selected = merchants.find(
        (merchant) => merchant.instance_slug === slug
      )
      const session = getTokens()
      if (
        !selected ||
        !session ||
        session.merchant === selected.instance_slug
      ) {
        return
      }
      if (
        !setTokensIfCurrent(
          { ...session, merchant: selected.instance_slug },
          session
        )
      ) {
        return
      }
      clearMerchantQueries()
      window.location.reload()
    },
    [merchants]
  )

  const startOIDC = React.useCallback((providerId: string) => {
    const { auth_base_url } = getBootstrap()
    const returnTo = encodeURIComponent(import.meta.env.BASE_URL)
    window.location.href = `${auth_base_url}/${providerId}/login?return_to=${returnTo}`
  }, [])

  const logout = React.useCallback(async () => {
    const session = getTokens()
    setTokens(null)
    clearMerchantQueries()
    queryClient.setQueryData<AuthStateData>(authStateQueryKey, (current) =>
      current ? { ...current, identity: undefined } : current
    )
    if (!session) return
    try {
      await logoutSession(session)
    } catch {
      // best effort — clear locally regardless
    }
  }, [])

  const value = React.useMemo(
    () => ({
      ready,
      bootError,
      config,
      capabilities,
      me,
      merchants,
      activeMerchant,
      selectMerchant,
      loginWithPassword,
      completeTwoFactor,
      selectTwoFactor,
      startOIDC,
      logout,
    }),
    [
      ready,
      bootError,
      config,
      capabilities,
      me,
      merchants,
      activeMerchant,
      selectMerchant,
      loginWithPassword,
      completeTwoFactor,
      selectTwoFactor,
      startOIDC,
      logout,
    ]
  )
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const ctx = React.useContext(AuthContext)
  if (!ctx) throw new Error("useAuth must be used within AuthProvider")
  return ctx
}
