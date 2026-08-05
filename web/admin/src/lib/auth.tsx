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
  loginWithPassword: (login: string, password: string) => Promise<void>
  startOIDC: (providerId: string) => void
  logout: () => Promise<void>
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

  const loginWithPassword = React.useCallback(
    async (login: string, password: string) => {
      const expectedSession = getTokens()
      const res = await authApi<AuthTokens>("/password/login", {
        method: "POST",
        body: { login, password },
      })
      if (res.requires_2fa) {
        throw new Error(
          "This account requires 2FA; the admin console does not support 2FA login yet."
        )
      }
      if (res.requires_verification) {
        throw new Error(
          "This account requires verification before it can sign in."
        )
      }
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
