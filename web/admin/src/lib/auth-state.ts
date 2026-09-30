import { queryOptions } from "@tanstack/react-query"

import {
  api,
  ApiError,
  authApi,
  clearTokensIfCurrent,
  getTokens,
  loadBootstrap,
  sameTokenSession,
  setTokens,
  setTokensIfCurrent,
  type BootstrapConfig,
} from "@/lib/api/client"
import type {
  AuthCapabilities,
  Me,
  MerchantMembership,
  MerchantMembershipList,
} from "@/lib/api/types"

export interface AuthIdentity {
  who: Me
  merchants: MerchantMembership[]
  activeMerchant?: MerchantMembership
}

export interface AuthStateData {
  config: BootstrapConfig
  capabilities?: AuthCapabilities
  identity?: AuthIdentity
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

// AuthKit's browser-OIDC callback returns tokens in the URL fragment.
export function consumeOIDCFragment(): boolean {
  const hash = window.location.hash
  if (!hash.includes("access_token=")) return false
  const params = new URLSearchParams(hash.slice(1))
  if (params.has("error")) return false
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

async function loadAuthState(): Promise<AuthStateData> {
  const config = await loadBootstrap()
  consumeOIDCFragment()

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

  return { config, capabilities, identity }
}

export const authStateQueryOptions = () =>
  queryOptions({
    queryKey: authStateQueryKey,
    queryFn: loadAuthState,
    staleTime: Infinity,
    gcTime: Infinity,
    retry: false,
  })
