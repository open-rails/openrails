// The console state extensions see: the signed-in user, their merchants and
// the selected one, plus each extension's host config.
import * as React from "react"

import { useAuth } from "@/lib/auth"

import { useExtensions } from "./registry"
import type { ConsoleHandle } from "./runtime"
import type { ConsoleContext } from "./types"

export function useConsoleContextFor(): (
  extensionId?: string
) => ConsoleContext {
  const { me, merchants, activeMerchant } = useAuth()
  const { config } = useExtensions()
  return React.useCallback(
    (extensionId?: string) => ({
      me: me
        ? { id: me.id, username: me.username, email: me.email ?? undefined }
        : null,
      merchants,
      activeMerchant,
      config: extensionId === undefined ? undefined : config[extensionId],
    }),
    [me, merchants, activeMerchant, config]
  )
}

export function useConsoleHandle(extensionId?: string): ConsoleHandle {
  const { selectMerchant, logout } = useAuth()
  const contextFor = useConsoleContextFor()
  return { ...contextFor(extensionId), selectMerchant, logout }
}
