/* eslint-disable react-refresh/only-export-components */
// AuthKit-backed auth for the console. The issuer's authhttp surface (base
// from /admin/config.json) provides capabilities discovery, password login,
// refresh, logout, /me, and — when the deployment mounts browser OIDC — the
// {provider}/login redirect flow whose callback lands back here with a
// one-time code in the URL fragment. Every sign-in answers an AuthResult.
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
  AuthResult,
  AuthTokens,
  Me,
  MerchantMembership,
} from "@/lib/api/types"
import {
  authStateQueryKey,
  authStateQueryOptions,
  loadIdentity,
  sessionFrom,
  signInStep,
  type AuthStateData,
  type PendingSignIn,
  type TwoFactorChallenge,
} from "@/lib/auth-state"
import { queryClient } from "@/lib/query-client"

export type { TwoFactorChallenge }

interface AuthState {
  ready: boolean
  bootError?: string
  config?: BootstrapConfig
  capabilities?: AuthCapabilities
  me: Me | null
  /** A browser sign-in that returned still waiting on a step. */
  pendingSignIn?: PendingSignIn
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
  const pendingSignIn = authState.data?.pending
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

  // Every sign-in ends the same way: store the pair, load the identity,
  // drop any merchant data belonging to whoever was signed in before.
  const completeSession = React.useCallback(
    async (tokens: AuthTokens, expectedSession: ReturnType<typeof getTokens>) => {
      const session = sessionFrom(tokens)
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

  // A finished sign-in completes; one that needs a second factor hands the
  // challenge back so the caller can ask for a code.
  const settle = React.useCallback(
    async (
      result: AuthResult,
      expectedSession: ReturnType<typeof getTokens>
    ): Promise<TwoFactorChallenge | null> => {
      const step = signInStep(result, expectedSession)
      if ("challenge" in step) return step.challenge
      await completeSession(step.tokens, expectedSession)
      return null
    },
    [completeSession]
  )

  const loginWithPassword = React.useCallback(
    async (
      identifier: string,
      password: string
    ): Promise<TwoFactorChallenge | null> => {
      const expectedSession = getTokens()
      const result = await authApi<AuthResult>("/password/login", {
        method: "POST",
        body: { identifier, password },
      })
      return settle(result, expectedSession)
    },
    [settle]
  )

  const completeTwoFactor = React.useCallback(
    async (
      challenge: TwoFactorChallenge,
      code: string,
      mode: TwoFactorVerificationMode
    ) => {
      const result = await authApi<AuthResult>("/2fa/verify", {
        method: "POST",
        body: twoFactorVerificationBody(challenge, code, mode),
      })
      if (await settle(result, challenge.expectedSession)) {
        throw new Error("Unexpected second verification step")
      }
    },
    [settle]
  )

  const selectTwoFactor = React.useCallback(
    async (challenge: TwoFactorChallenge, factorId: string) => {
      // Switching factors re-issues the challenge for the chosen factor.
      const result = await authApi<AuthResult>("/2fa/challenge", {
        method: "POST",
        body: {
          user_id: challenge.userID,
          challenge: challenge.challenge,
          factor_id: factorId,
        },
      })
      const step = signInStep(result, challenge.expectedSession)
      if (!("challenge" in step)) {
        throw new Error(
          "Unexpected response while selecting a verification factor"
        )
      }
      return step.challenge
    },
    []
  )

  const selectMerchant = React.useCallback(
    (slug: string) => {
      const selected = merchants.find(
        (merchant) => merchant.slug === slug
      )
      const session = getTokens()
      if (
        !selected ||
        !session ||
        session.merchant === selected.slug
      ) {
        return
      }
      if (
        !setTokensIfCurrent(
          { ...session, merchant: selected.slug },
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
      pendingSignIn,
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
      pendingSignIn,
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
