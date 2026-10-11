// The console's operator. auth-ui owns the browser session (sign-in, refresh,
// step-up); the console adds the merchants the user may act on (OpenRails
// lists none: see loadMerchants) and the one its requests are made as.
import * as React from "react"
import { useQuery } from "@tanstack/react-query"

import { directoryHost, useExtensions } from "@/extensions/registry"
import type { ConsoleExtension, ConsoleMerchant } from "@/extensions/types"
import {
  canActAt,
  isMerchantID,
  mountMerchant,
  selectedMerchant,
  setSelectedMerchant,
  subscribeMerchant,
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
  // A merchant the console was asked to open (#merchant=<id>) that the
  // person holds no role at: the list is shown instead.
  refusedMerchant?: string
  selectMerchant: (slug: string) => void
  logout: () => Promise<void>
}

const EMPTY_MERCHANTS: ConsoleMerchant[] = []

export const clearMerchantQueries = () =>
  queryClient.removeQueries({ queryKey: ["merchant"] })

// A merchant the console was opened on (#merchant=<id>), read before any
// redirect drops the hash and applied once the user's merchants are known: a
// request to exchange for that merchant.
let requestedMerchant =
  typeof window === "undefined" ? undefined : takeMerchantFromHash()

// The merchant last asked for that the exchange refused.
let refused: string | undefined

export const refusedMerchant = () => refused

const named = (merchant: ConsoleMerchant, key: string | undefined) =>
  key !== undefined && (merchant.id === key || merchant.slug === key)

const choose = (merchant: ConsoleMerchant | undefined) =>
  setSelectedMerchant(merchant?.slug, merchant?.id)

// The user's merchants, sorted: the host directory's, else the merchant the
// mount serves, else the one opened by its id. A requested merchant wins when
// the person may act there (the exchange grants it), then the selected one,
// else the first becomes the merchant requests are made as.
export async function loadMerchants(
  host?: ConsoleExtension
): Promise<ConsoleMerchant[]> {
  const fixed = mountMerchant()
  let list: ConsoleMerchant[]
  if (host?.merchants) list = await host.merchants()
  else if (fixed) list = [fixed]
  else {
    const opened = requestedMerchant ?? selectedMerchant()
    list = opened
      ? [{ id: isMerchantID(opened) ? opened : "", slug: opened }]
      : []
  }
  const merchants = [...list].sort((a, b) => a.slug.localeCompare(b.slug))
  const requested = requestedMerchant
  requestedMerchant = undefined
  refused = undefined
  if (requested !== undefined) {
    const wanted = merchants.find((merchant) => named(merchant, requested))
    if (wanted && (await canActAt(wanted.id))) choose(wanted)
    else refused = requested
  }
  const selected = selectedMerchant()
  const listed = merchants.filter((merchant) => !named(merchant, refused))
  if (!listed.some((merchant) => merchant.slug === selected)) {
    choose(listed[0])
  }
  return listed
}

// useSelectedMerchant is the merchant requests are made as, re-rendering on
// a switch.
export function useSelectedMerchant(): string | undefined {
  return React.useSyncExternalStore(subscribeMerchant, selectedMerchant)
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
  const active = useSelectedMerchant()

  const opensByName = !host && !mountMerchant()
  // A switch drops the previous merchant's data and acts on the next one
  // through a token exchanged for it; nothing is reloaded.
  const selectMerchant = React.useCallback(
    (slug: string) => {
      if (slug === selectedMerchant()) return
      const merchant = merchants.find((m) => m.slug === slug)
      if (!merchant && !opensByName) return
      clearMerchantQueries()
      if (merchant) choose(merchant)
      else setSelectedMerchant(slug || undefined)
    },
    [merchants, opensByName]
  )

  const { signOut } = session
  const logout = React.useCallback(async () => {
    setSelectedMerchant(undefined)
    clearMerchantQueries()
    await signOut()
  }, [signOut])
  const refusedOne = membership.isSuccess ? refusedMerchant() : undefined

  return {
    ready:
      session.status === "signed_out" || (signedIn && !membership.isPending),
    signedIn,
    merchantsFailed: membership.isError,
    me: session.user,
    merchants,
    activeMerchant: merchants.find((merchant) => merchant.slug === active),
    opensByName,
    refusedMerchant: refusedOne,
    selectMerchant,
    logout,
  }
}
