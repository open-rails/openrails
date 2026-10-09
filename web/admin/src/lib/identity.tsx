/* eslint-disable react-refresh/only-export-components */
// Who the console's operator is, whichever way they signed in: the
// deployment's own accounts (auth-ui's AuthKit session) or a trusted issuer
// (auth-ui's issuer client, an OAuth 2.0 code flow with DPoP).
import * as React from "react"
import { useAuth as useSession } from "@openrails/auth-ui/react"
import { useIssuerAuth } from "@openrails/auth-ui/react"
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
  // Set when staff sign in at a trusted issuer.
  issuer?: {
    name: string
    signIn: (returnTo?: string) => Promise<void>
  }
}

const IdentityContext = React.createContext<ConsoleIdentity | null>(null)

export function useIdentity(): ConsoleIdentity {
  const identity = React.useContext(IdentityContext)
  if (!identity) throw new Error("console identity is not provided")
  return identity
}

export function LocalIdentity({ children }: { children: React.ReactNode }) {
  const session = useSession()
  const { status, user, signOut } = session
  const value = React.useMemo<ConsoleIdentity>(
    () => ({
      status,
      user:
        status === "signed_in" && user
          ? {
              id: user.id,
              email: user.email ?? undefined,
              username: user.username ?? undefined,
            }
          : null,
      signOut,
    }),
    [status, user, signOut]
  )
  return (
    <IdentityContext.Provider value={value}>
      {children}
    </IdentityContext.Provider>
  )
}

export function IssuerIdentity({
  name,
  onUserChange,
  children,
}: {
  name: string
  onUserChange: (previous: string | null) => void
  children: React.ReactNode
}) {
  const { status, user, signIn, signOut } = useIssuerAuth()
  const userId = status === "signed_in" ? (user?.sub ?? null) : null
  const previous = React.useRef<string | null>(null)
  React.useEffect(() => {
    if (status === "loading" || status === "restoring") return
    if (previous.current !== userId) onUserChange(previous.current)
    previous.current = userId
  }, [status, userId, onUserChange])
  const value = React.useMemo<ConsoleIdentity>(
    () => ({
      status,
      user:
        status === "signed_in" && user
          ? {
              id: user.sub,
              email: user.email,
              username: user.preferredUsername ?? user.name,
            }
          : null,
      signOut: () => signOut(),
      issuer: { name, signIn: (returnTo) => signIn({ returnTo }) },
    }),
    [status, user, signIn, signOut, name]
  )
  return (
    <IdentityContext.Provider value={value}>
      {children}
    </IdentityContext.Provider>
  )
}
