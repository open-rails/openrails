/* eslint-disable react-refresh/only-export-components */
// AuthKit-backed auth for the console. The issuer's authhttp surface (base
// from /admin/config.json) provides capabilities discovery, password login,
// refresh, logout, /me, and — when the deployment mounts browser OIDC — the
// {provider}/login redirect flow whose callback lands back here with tokens
// in the URL fragment.
import * as React from "react"

import {
  ApiError,
  authApi,
  clearTokensIfCurrent,
  getBootstrap,
  getTokens,
  loadBootstrap,
  logoutSession,
  sameTokenSession,
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
  MerchantMembershipList,
} from "@/lib/api/types"
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

function merchantMemberships(groups: MerchantMembershipList) {
  return [...groups.data]
    .filter((group) => group.persona === "merchant")
    .sort((a, b) => a.instance_slug.localeCompare(b.instance_slug))
}

async function loadIdentity(
  expectedSession: NonNullable<ReturnType<typeof getTokens>>
) {
  const who = await authApi<Me>("/me")
  const groups = await authApi<MerchantMembershipList>("/me/groups")
  if (!sameTokenSession(expectedSession, getTokens())) {
    throw new Error("Your session changed while your account was loading")
  }

  const merchants = merchantMemberships(groups)
  const storedSession = getTokens()
  if (!storedSession) {
    throw new Error("Your session ended while your account was loading")
  }
  const activeMerchant =
    merchants.find(
      (merchant) => merchant.instance_slug === storedSession.merchant
    ) ?? merchants[0]

  if (storedSession.merchant !== activeMerchant?.instance_slug) {
    const updated = {
      ...storedSession,
      merchant: activeMerchant?.instance_slug,
    }
    if (!setTokensIfCurrent(updated, storedSession)) {
      throw new Error("Your session changed while your account was loading")
    }
  }

  return { who, merchants, activeMerchant }
}

// consumeOIDCFragment reads AuthKit's browser-OIDC fragment redirect
// (#access_token=…&refresh_token=…&merchant=…) if present and stores the
// tokens plus the host-selected merchant context.
function consumeOIDCFragment(): boolean {
  const hash = window.location.hash
  if (!hash.includes("access_token=")) return false
  const params = new URLSearchParams(hash.slice(1))
  const accessToken = params.get("access_token")
  if (!accessToken) return false
  const expiresIn = Number(params.get("expires_in") || "0")
  setTokens({
    access_token: accessToken,
    refresh_token: params.get("refresh_token") ?? undefined,
    expires_at: expiresIn ? Date.now() + expiresIn * 1000 : undefined,
    merchant: params.get("merchant")?.trim() || undefined,
  })
  history.replaceState(
    null,
    "",
    window.location.pathname + window.location.search
  )
  return true
}

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [ready, setReady] = React.useState(false)
  const [bootError, setBootError] = React.useState<string>()
  const [config, setConfig] = React.useState<BootstrapConfig>()
  const [capabilities, setCapabilities] = React.useState<AuthCapabilities>()
  const [me, setMe] = React.useState<Me | null>(null)
  const [merchants, setMerchants] = React.useState<MerchantMembership[]>([])
  const [activeMerchant, setActiveMerchant] =
    React.useState<MerchantMembership>()

  React.useEffect(() => {
    setUnauthorizedHandler(() => {
      queryClient.clear()
      setMe(null)
      setMerchants([])
      setActiveMerchant(undefined)
    })
    return () => setUnauthorizedHandler(null)
  }, [])

  React.useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const cfg = await loadBootstrap()
        if (cancelled) return
        setConfig(cfg)
        consumeOIDCFragment()
        // Capability discovery is unauthenticated; absence of OIDC providers
        // (or an older issuer without the endpoint) degrades to password-only.
        try {
          const caps = await authApi<AuthCapabilities>("/capabilities")
          if (!cancelled) setCapabilities(caps)
        } catch {
          if (!cancelled) setCapabilities(undefined)
        }
        const session = getTokens()
        if (session) {
          try {
            const identity = await loadIdentity(session)
            if (!cancelled) {
              setMe(identity.who)
              setMerchants(identity.merchants)
              setActiveMerchant(identity.activeMerchant)
            }
          } catch (err) {
            if (
              err instanceof ApiError &&
              err.status === 401 &&
              clearTokensIfCurrent(session) &&
              !cancelled
            ) {
              setMe(null)
            }
          }
        }
      } catch (err) {
        if (!cancelled)
          setBootError(err instanceof Error ? err.message : String(err))
      } finally {
        if (!cancelled) setReady(true)
      }
    })()
    return () => {
      cancelled = true
    }
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
      queryClient.clear()
      setMe(identity.who)
      setMerchants(identity.merchants)
      setActiveMerchant(identity.activeMerchant)
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
      queryClient.clear()
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
    queryClient.clear()
    setMe(null)
    setMerchants([])
    setActiveMerchant(undefined)
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
