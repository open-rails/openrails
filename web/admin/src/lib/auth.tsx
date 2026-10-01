// The console's operator. auth-ui owns the browser session (sign-in, refresh,
// step-up); the console adds the merchants the user belongs to and the one
// its requests are made as.
import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { useAuth as useSession } from "@openrails/auth-ui/react"
import type { UserProfile } from "@openrails/auth-ui/client"

import { api, selectedMerchant, setSelectedMerchant } from "@/lib/api/client"
import type {
  MerchantMembership,
  MerchantMembershipList,
} from "@/lib/api/types"
import { queryClient } from "@/lib/query-client"

export interface ConsoleAuth {
  // The session has settled and, when signed in, the merchants are known.
  ready: boolean
  signedIn: boolean
  me: UserProfile | null
  merchants: MerchantMembership[]
  activeMerchant?: MerchantMembership
  selectMerchant: (slug: string) => void
  logout: () => Promise<void>
}

const EMPTY_MERCHANTS: MerchantMembership[] = []

export const clearMerchantQueries = () =>
  queryClient.removeQueries({ queryKey: ["merchant"] })

// The user's merchants, sorted; the selected one is kept when it is still
// theirs, else the first becomes the merchant requests are made as.
export async function loadMerchants(): Promise<MerchantMembership[]> {
  const list = await api<MerchantMembershipList>("/merchants")
  const merchants = [...list.data].sort((a, b) => a.slug.localeCompare(b.slug))
  const selected = selectedMerchant()
  if (!merchants.some((merchant) => merchant.slug === selected)) {
    setSelectedMerchant(merchants[0]?.slug)
  }
  return merchants
}

export function useAuth(): ConsoleAuth {
  const session = useSession()
  const signedIn = session.status === "signed_in"
  const membership = useQuery({
    queryKey: ["auth", "merchants", session.userId],
    queryFn: loadMerchants,
    enabled: signedIn,
    staleTime: Infinity,
    retry: false,
  })
  const merchants = membership.data ?? EMPTY_MERCHANTS
  const active = selectedMerchant()

  const selectMerchant = React.useCallback(
    (slug: string) => {
      if (slug === selectedMerchant()) return
      if (!merchants.some((merchant) => merchant.slug === slug)) return
      setSelectedMerchant(slug)
      clearMerchantQueries()
      window.location.reload()
    },
    [merchants]
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
    me: signedIn ? session.user : null,
    merchants,
    activeMerchant: merchants.find((merchant) => merchant.slug === active),
    selectMerchant,
    logout,
  }
}
