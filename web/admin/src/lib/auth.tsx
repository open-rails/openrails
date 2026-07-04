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
  getBootstrap,
  getTokens,
  loadBootstrap,
  setTokens,
  setUnauthorizedHandler,
  type BootstrapConfig,
} from "@/lib/api/client"
import type { AuthCapabilities, AuthTokens, Me } from "@/lib/api/types"

interface AuthState {
  ready: boolean
  bootError?: string
  config?: BootstrapConfig
  capabilities?: AuthCapabilities
  me: Me | null
  loginWithPassword: (login: string, password: string) => Promise<void>
  startOIDC: (providerId: string) => void
  logout: () => Promise<void>
}

const AuthContext = React.createContext<AuthState | undefined>(undefined)

// consumeOIDCFragment reads AuthKit's browser-OIDC fragment redirect
// (#access_token=…&refresh_token=…) if present and stores the tokens.
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
  })
  history.replaceState(null, "", window.location.pathname + window.location.search)
  return true
}

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [ready, setReady] = React.useState(false)
  const [bootError, setBootError] = React.useState<string>()
  const [config, setConfig] = React.useState<BootstrapConfig>()
  const [capabilities, setCapabilities] = React.useState<AuthCapabilities>()
  const [me, setMe] = React.useState<Me | null>(null)

  React.useEffect(() => {
    setUnauthorizedHandler(() => setMe(null))
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
        if (getTokens()) {
          try {
            const who = await authApi<Me>("/me")
            if (!cancelled) setMe(who)
          } catch (err) {
            if (err instanceof ApiError && err.status === 401) setTokens(null)
          }
        }
      } catch (err) {
        if (!cancelled) setBootError(err instanceof Error ? err.message : String(err))
      } finally {
        if (!cancelled) setReady(true)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  const loginWithPassword = React.useCallback(async (login: string, password: string) => {
    const res = await authApi<AuthTokens>("/password/login", {
      method: "POST",
      body: { login, password },
    })
    if (res.requires_2fa) {
      throw new Error("This account requires 2FA; the admin console does not support 2FA login yet.")
    }
    if (res.requires_verification) {
      throw new Error("This account requires verification before it can sign in.")
    }
    setTokens({
      access_token: res.access_token,
      refresh_token: res.refresh_token,
      expires_at: res.expires_in ? Date.now() + res.expires_in * 1000 : undefined,
    })
    setMe(await authApi<Me>("/me"))
  }, [])

  const startOIDC = React.useCallback((providerId: string) => {
    const { auth_base_url } = getBootstrap()
    const returnTo = encodeURIComponent(import.meta.env.BASE_URL)
    window.location.href = `${auth_base_url}/${providerId}/login?return_to=${returnTo}`
  }, [])

  const logout = React.useCallback(async () => {
    try {
      await authApi("/logout", { method: "DELETE" })
    } catch {
      // best effort — clear locally regardless
    }
    setTokens(null)
    setMe(null)
  }, [])

  const value = React.useMemo(
    () => ({ ready, bootError, config, capabilities, me, loginWithPassword, startOIDC, logout }),
    [ready, bootError, config, capabilities, me, loginWithPassword, startOIDC, logout],
  )
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const ctx = React.useContext(AuthContext)
  if (!ctx) throw new Error("useAuth must be used within AuthProvider")
  return ctx
}
