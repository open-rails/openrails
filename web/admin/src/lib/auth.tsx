// The console's operator. auth-ui owns the browser session (sign-in, refresh,
// step-up); the console adds the merchants the user may act on and the one
// its requests are made as. OpenRails lists no user's merchants: a host's
// extension does (merchants), else the mount serves one merchant, else the
// user opens one by name.
import * as React from "react"
import { useQuery } from "@tanstack/react-query"

import { directoryHost, useExtensions } from "@/extensions/registry"
import type { ConsoleExtension, ConsoleMerchant } from "@/extensions/types"
import {
  mountMerchant,
  selectedMerchant,
  setSelectedMerchant,
  takeMerchantFromHash,
} from "@/lib/api/client"
import { useIdentity, type ConsoleUser } from "@/lib/identity"
import { queryClient } from "@/lib/query-client"

export interface ConsoleAuth {
  // The session has settled and, when signed in, the merchants are known.
  ready: boolean
  signedIn: boolean
  // The merchant list could not be loaded: not the same as having none.
  merchantsFailed: boolean
  me: ConsoleUser | null
  merchants: ConsoleMerchant[]
  activeMerchant?: ConsoleMerchant
  // Merchants are opened by name: no host directory and no merchant the
  // mount serves.
  opensByName: boolean
  selectMerchant: (slug: string) => void
  logout: () => Promise<void>
}

const EMPTY_MERCHANTS: ConsoleMerchant[] = []

export const clearMerchantQueries = () =>
  queryClient.removeQueries({ queryKey: ["merchant"] })

// A merchant the console was opened on (#merchant=<slug>), read before any
// redirect drops the hash and applied once the user's merchants are known.
let requestedMerchant =
  typeof window === "undefined" ? undefined : takeMerchantFromHash()

// The user's merchants, sorted: the host directory's, else the merchant the
// mount serves, else the one opened by name. A requested merchant wins when it
// is theirs, then the selected one, else the first becomes the merchant
// requests are made as.
export async function loadMerchants(
  host?: ConsoleExtension
): Promise<ConsoleMerchant[]> {
  const fixed = mountMerchant()
  let list: ConsoleMerchant[]
  if (host?.merchants) list = await host.merchants()
  else if (fixed) list = [fixed]
  else {
    const opened = requestedMerchant ?? selectedMerchant()
    list = opened ? [{ id: "", slug: opened }] : []
  }
  const merchants = [...list].sort((a, b) => a.slug.localeCompare(b.slug))
  if (merchants.some((merchant) => merchant.slug === requestedMerchant)) {
    setSelectedMerchant(requestedMerchant)
  }
  requestedMerchant = undefined
  const selected = selectedMerchant()
  if (!merchants.some((merchant) => merchant.slug === selected)) {
    setSelectedMerchant(merchants[0]?.slug)
  }
  return merchants
}

export function useAuth(): ConsoleAuth {
  const session = useIdentity()
  const signedIn = session.status === "signed_in"
  // A build's extensions are fixed: the directory never changes under a key.
  const host = directoryHost(useExtensions().extensions)
  // eslint-disable-next-line @tanstack/query/exhaustive-deps
  const membership = useQuery({
    queryKey: ["auth", "merchants", session.user?.id ?? null],
    queryFn: () => loadMerchants(host),
    enabled: signedIn,
    staleTime: Infinity,
    retry: false,
  })
  const merchants = membership.data ?? EMPTY_MERCHANTS
  const active = selectedMerchant()

  const opensByName = !host && !mountMerchant()
  const selectMerchant = React.useCallback(
    (slug: string) => {
      if (slug === selectedMerchant()) return
      const known = merchants.some((merchant) => merchant.slug === slug)
      if (!known && !opensByName) return
      setSelectedMerchant(slug || undefined)
      clearMerchantQueries()
      window.location.reload()
    },
    [merchants, opensByName]
  )

  const { signOut } = session
  const logout = React.useCallback(async () => {
    setSelectedMerchant(undefined)
    clearMerchantQueries()
    await signOut()
  }, [signOut])

  return {
    ready:
      session.status === "signed_out" || (signedIn && !membership.isPending),
    signedIn,
    merchantsFailed: membership.isError,
    me: session.user,
    merchants,
    activeMerchant: merchants.find((merchant) => merchant.slug === active),
    opensByName,
    selectMerchant,
    logout,
  }
}
