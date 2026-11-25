/* eslint-disable react-refresh/only-export-components */
// Who the console's operator is: the deployment's own account, from
// auth-ui's AuthKit session.
import * as React from "react"
import { useAuth as useSession } from "@openrails/auth-ui/react"
import type { AuthStatus } from "@openrails/auth-ui/react"

export interface ConsoleUser {
  id: string
  email?: string
  username?: string
}

export interface ConsoleIdentity {
  status: AuthStatus
  user: ConsoleUser | null
  signOut: () => Promise<void>
}

const IdentityContext = React.createContext<ConsoleIdentity | null>(null)

export function useIdentity(): ConsoleIdentity {
  const identity = React.useContext(IdentityContext)
  if (!identity) throw new Error("console identity is not provided")
  return identity
}

export function LocalIdentity({ children }: { children: React.ReactNode }) {
  const session = useSession()
  const { status, userId, user, signOut } = session
  const value = React.useMemo<ConsoleIdentity>(
    () => ({
      status,
      user:
        status === "signed_in" && userId
          ? {
              id: userId,
              email: user?.email ?? undefined,
              username: user?.username ?? undefined,
            }
          : null,
      signOut,
    }),
    [status, userId, user, signOut]
  )
  return (
    <IdentityContext.Provider value={value}>
      {children}
    </IdentityContext.Provider>
  )
}
